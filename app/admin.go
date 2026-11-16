package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"guludrive/core"
)

const (
	adminUsernameKey = "admin.username"
	adminPasswordKey = "admin.password_hash"
)

type administrator struct {
	username     string
	passwordHash []byte
}

// loadAdministrator reads a coherent stored pair without changing existing settings.
// A missing verifier is the first-run state, not a startup failure.
func loadAdministrator(ctx context.Context, db *sql.DB) (administrator, error) {
	var username, hash sql.NullString
	err := db.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key=?), (SELECT value FROM settings WHERE key=?)`, adminUsernameKey, adminPasswordKey).Scan(&username, &hash)
	if err != nil {
		return administrator{}, errors.New("app: cannot read administrator settings")
	}
	if !hash.Valid {
		return administrator{}, nil
	}
	if _, err = bcrypt.Cost([]byte(hash.String)); err != nil || len(hash.String) != 60 {
		return administrator{}, errors.New("app: invalid administrator password verifier; preserve the database")
	}
	if !validAdministratorUsername(username.String) {
		return administrator{}, errors.New("app: invalid stored administrator username")
	}
	return administrator{username: username.String, passwordHash: []byte(hash.String)}, nil
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, core.SetupStatus{Initialized: s.security.initialized()})
		return
	}
	if s.security.initialized() {
		apiFailure(w, core.ErrConflict)
		return
	}
	if !s.security.allowAttempt(w, r) {
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !validAdministratorUsername(in.Username) || !validAdministratorPassword(in.Password) {
		apiFailure(w, core.ErrInvalid)
		return
	}
	if err := s.initializeAdministrator(r.Context(), in.Username, in.Password); err != nil {
		apiFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, core.SetupStatus{Initialized: true})
}

func (s *Server) initializeAdministrator(ctx context.Context, username, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return errors.New("app: cannot create administrator password verifier")
	}
	// Setup and credential changes publish settings/epoch together. Login may
	// hash outside this lock but cannot associate an old verifier with a new epoch.
	s.security.credentialMu.Lock()
	defer s.security.credentialMu.Unlock()
	if len(s.security.passwordHash) != 0 {
		return core.ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("app: cannot save administrator settings")
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO NOTHING", adminPasswordKey, string(hash))
	if err != nil {
		return errors.New("app: cannot save administrator settings")
	}
	if n, err := result.RowsAffected(); err != nil {
		return errors.New("app: cannot save administrator settings")
	} else if n != 1 {
		return core.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", adminUsernameKey, username); err != nil {
		return errors.New("app: cannot save administrator settings")
	}
	if err = tx.Commit(); err != nil {
		return errors.New("app: cannot save administrator settings")
	}
	s.security.replaceAdministrator(administrator{username: username, passwordHash: hash})
	return nil
}

func validAdministratorUsername(username string) bool {
	// The same administrator also authenticates DAV Basic, whose username
	// cannot contain the ':' separator.
	return len(username) > 0 && len(username) <= 256 && utf8.ValidString(username) && !strings.Contains(username, ":") && strings.IndexFunc(username, unicode.IsControl) < 0
}

func writeAdministrator(ctx context.Context, tx *sql.Tx, admin administrator) error {
	for _, setting := range []struct{ key, value string }{{adminUsernameKey, admin.username}, {adminPasswordKey, string(admin.passwordHash)}} {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value WHERE settings.value <> excluded.value", setting.key, setting.value); err != nil {
			return errors.New("app: cannot save administrator settings")
		}
	}
	return nil
}

// Optional fields distinguish omission from an explicitly invalid null value.
// Empty newPassword retains the current verifier; an empty username is invalid.
type administratorString struct {
	value string
	set   bool
}

func (s *administratorString) UnmarshalJSON(raw []byte) error {
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return core.ErrInvalid
	}
	s.value, s.set = *value, true
	return nil
}

type administratorChange struct {
	CurrentPassword string              `json:"currentPassword"`
	Username        administratorString `json:"username"`
	NewPassword     administratorString `json:"newPassword"`
}

var errInvalidCurrentPassword = errors.New("app: current administrator password is incorrect")

// changeAdministrator is called under the common mutation gate. Password work
// runs before SQL, and only a successful commit publishes credentials and epoch.
func (s *Server) changeAdministrator(ctx context.Context, in administratorChange) (core.AdminProfile, error) {
	admin := s.security.administrator()
	if !validAdministratorPassword(in.CurrentPassword) || (in.Username.set && !validAdministratorUsername(in.Username.value)) || (in.NewPassword.value != "" && !validAdministratorPassword(in.NewPassword.value)) {
		return core.AdminProfile{}, core.ErrInvalid
	}
	if (!in.Username.set || in.Username.value == admin.username) && in.NewPassword.value == "" {
		return core.AdminProfile{}, core.ErrInvalid
	}
	if bcrypt.CompareHashAndPassword(admin.passwordHash, []byte(in.CurrentPassword)) != nil {
		return core.AdminProfile{}, errInvalidCurrentPassword
	}
	if in.Username.set {
		admin.username = in.Username.value
	}
	if in.NewPassword.value != "" {
		encoded, err := bcrypt.GenerateFromPassword([]byte(in.NewPassword.value), bcrypt.DefaultCost)
		if err != nil {
			return core.AdminProfile{}, errors.New("app: cannot create administrator password verifier")
		}
		admin.passwordHash = encoded
	}
	// Login bypasses the mutation gate. Hold the credential lock across commit
	// and publication so it can never bind an old password to the new epoch.
	s.security.credentialMu.Lock()
	defer s.security.credentialMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.AdminProfile{}, errors.New("app: cannot save administrator settings")
	}
	defer tx.Rollback()
	if err = writeAdministrator(ctx, tx, admin); err != nil {
		return core.AdminProfile{}, err
	}
	if err = tx.Commit(); err != nil {
		return core.AdminProfile{}, errors.New("app: cannot save administrator settings")
	}
	s.security.replaceAdministrator(admin)
	return core.AdminProfile{Username: admin.username}, nil
}

func (s *Server) administratorAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, core.AdminProfile{Username: s.security.administrator().username})
		return
	}
	var in administratorChange
	if !decodeJSON(w, r, &in) {
		return
	}
	profile, err := s.changeAdministrator(r.Context(), in)
	if errors.Is(err, errInvalidCurrentPassword) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Current password is incorrect", "code": "invalid_current_password"})
		return
	}
	if err != nil {
		apiFailure(w, err)
		return
	}
	if err = s.security.manager(r).Destroy(r.Context()); err != nil {
		apiFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		core.AdminProfile
		Relogin bool `json:"relogin"`
	}{profile, true})
}

func validAdministratorPassword(password string) bool {
	return len(password) > 0 && len(password) <= 72 && utf8.ValidString(password) && !strings.ContainsAny(password, "\r\n\x00")
}
