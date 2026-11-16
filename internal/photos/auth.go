// Portions derived from gotohp/core/googleauth.go, Copyright (c) 2024 xob0t.
// See THIRD_PARTY_NOTICES.md for the MIT license.

package photos

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"guludrive/core"
)

const photosPackage = "com.google.android.apps.photos"
const photosSig = "24bb24c05e47e0aefa68a58a766179d9b613a600"
const playSig = "38918a453d07199354f8b19af05ec6562ced5788"
const photosService = "oauth2:openid https://www.googleapis.com/auth/mobileapps.native https://www.googleapis.com/auth/photos.native"

// LoginStage identifies only a remote authentication step, not local input or storage work.
type LoginStage string

const (
	LoginStageExchangeMaster LoginStage = "exchange_master"
	LoginStageExchangeBearer LoginStage = "exchange_bearer"
	LoginStageValidatePhotos LoginStage = "validate_photos"
)

// LoginError contains only safe diagnostics. The cause retains cancellation or a
// definitive credential rejection, never a transport error or Google's raw text.
type LoginError struct {
	Stage          LoginStage
	UpstreamStatus int
	Reason         string
	cause          error
}

func (e *LoginError) Error() string { return "auth: Google authentication failed" }
func (e *LoginError) Unwrap() error { return e.cause }

func loginReason(reason string) string {
	switch reason {
	case "BadAuthentication", "NeedsBrowser", "ServiceUnavailable":
		return reason
	default:
		return "unknown"
	}
}

func loginFailure(ctx context.Context, stage LoginStage, status int, reason string, err error) *LoginError {
	reason = loginReason(reason)
	var cause error
	switch {
	case ctx.Err() != nil:
		cause = ctx.Err()
	case errors.Is(err, context.Canceled):
		cause = context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		cause = context.DeadlineExceeded
	case status == http.StatusUnauthorized || status == http.StatusForbidden ||
		((status >= 200 && status < 300 || status == http.StatusBadRequest) && (reason == "BadAuthentication" || reason == "NeedsBrowser")):
		cause = core.ErrUnauthorized
	}
	return &LoginError{Stage: stage, UpstreamStatus: status, Reason: reason, cause: cause}
}

// AuthEndpoints are constructor-only, loopback overrides for offline tests.
type AuthEndpoints struct{ Exchange, Bearer, Validate string }
type AuthOptions struct {
	Proxy      string
	HTTPClient *http.Client
	Endpoints  AuthEndpoints
}

type credential struct {
	ID        string
	Email     string
	Master    string
	AndroidID string
	bearer    string
	expiry    time.Time
}
type accountState struct {
	Selected string
	Accounts map[string]*credential
}

const selectedAccountKey = "google.selected_account"

// Accounts borrows the private application database for recoverable credentials.
// Its context-aware gate serializes refreshes and account mutations so one expired
// token causes only one exchange. Bearer tokens and expiry remain memory-only;
// credentials are available only to internal backend callers, never lists.
type Accounts struct {
	gate      chan struct{}
	db        *sql.DB
	client    *http.Client
	endpoints AuthEndpoints
	state     accountState
}

var _ core.Accounts = (*Accounts)(nil)
var _ TokenSource = (*Accounts)(nil)

// OpenAccounts loads credentials and selection from the shared application
// database. The caller owns its schema, lifetime and private directory protection;
// the database and its backups contain recoverable credentials.
func OpenAccounts(db *sql.DB, opts AuthOptions) (*Accounts, error) {
	if db == nil {
		return nil, fmt.Errorf("auth: database: %w", core.ErrInvalid)
	}
	client, err := newHTTPClient(opts.Proxy, opts.HTTPClient)
	if err != nil {
		return nil, err
	}
	// Authentication exchanges are small; downloads use a separate client with
	// no default whole-body timeout so large carriers are not cut off.
	if client.Timeout == 0 {
		client.Timeout = 30 * time.Second
	}
	ep := opts.Endpoints
	if ep.Exchange, err = testEndpoint(ep.Exchange, "https://android.clients.google.com/auth"); err != nil {
		return nil, err
	}
	if ep.Bearer, err = testEndpoint(ep.Bearer, "https://android.googleapis.com/auth"); err != nil {
		return nil, err
	}
	if ep.Validate, err = testEndpoint(ep.Validate, "https://photosdata-pa.googleapis.com/6439526531001121323/5084965799730810217"); err != nil {
		return nil, err
	}
	m := &Accounts{gate: make(chan struct{}, 1), db: db, client: client, endpoints: ep, state: accountState{Accounts: map[string]*credential{}}}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, errors.New("auth: cannot read account store")
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT id, email, android_id, master_token FROM accounts LIMIT 257")
	if err != nil {
		return nil, errors.New("auth: cannot read account store")
	}
	defer rows.Close()
	for rows.Next() {
		c := new(credential)
		if err = rows.Scan(&c.ID, &c.Email, &c.AndroidID, &c.Master); err != nil {
			return nil, errors.New("auth: cannot read account store")
		}
		if !validHex(c.ID, 16) || !validHex(c.AndroidID, 8) || !validEmail(c.Email) || !validSecret(c.Master) || len(m.state.Accounts) >= 256 {
			return nil, errors.New("auth: invalid account store")
		}
		m.state.Accounts[c.ID] = c
	}
	if rows.Err() != nil {
		return nil, errors.New("auth: cannot read account store")
	}
	if err = rows.Close(); err != nil {
		return nil, errors.New("auth: cannot read account store")
	}
	if err = tx.QueryRow("SELECT value FROM settings WHERE key = ?", selectedAccountKey).Scan(&m.state.Selected); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("auth: cannot read selected account")
	}
	if m.state.Selected != "" && m.state.Accounts[m.state.Selected] == nil {
		return nil, errors.New("auth: invalid selected account")
	}
	if err = tx.Commit(); err != nil {
		return nil, errors.New("auth: cannot read account store")
	}
	return m, nil
}
func validHex(s string, n int) bool { b, e := hex.DecodeString(s); return e == nil && len(b) == n }
func validSecret(s string) bool {
	if len(s) == 0 || len(s) > 16384 {
		return false
	}
	for i := range s {
		if s[i] < 33 || s[i] > 126 {
			return false
		}
	}
	return true
}
func validEmail(s string) bool {
	a, e := mail.ParseAddress(s)
	return len(s) <= 320 && e == nil && a.Address == s
}
func (m *Accounts) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *Accounts) unlock() { <-m.gate }
func (m *Accounts) account(id string) (*credential, error) {
	if id == "" {
		id = m.state.Selected
	}
	c := m.state.Accounts[id]
	if c == nil {
		return nil, core.ErrUnauthorized
	}
	return c, nil
}
func (m *Accounts) Accounts(ctx context.Context) ([]core.Account, error) {
	if err := m.lock(ctx); err != nil {
		return nil, err
	}
	defer m.unlock()
	out := make([]core.Account, 0, len(m.state.Accounts))
	for _, c := range m.state.Accounts {
		out = append(out, core.Account{ID: c.ID, Email: c.Email, Selected: c.ID == m.state.Selected})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

// DefaultAccount returns safe metadata for the currently selected account.
func (m *Accounts) DefaultAccount(ctx context.Context) (core.Account, error) {
	if err := m.lock(ctx); err != nil {
		return core.Account{}, err
	}
	defer m.unlock()
	c, err := m.account("")
	if err != nil {
		return core.Account{}, err
	}
	return core.Account{ID: c.ID, Email: c.Email, Selected: true}, nil
}
func (m *Accounts) Select(ctx context.Context, id string) error {
	if err := m.lock(ctx); err != nil {
		return err
	}
	defer m.unlock()
	if id == "" || m.state.Accounts[id] == nil {
		return core.ErrNotFound
	}
	if err := m.persist(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", selectedAccountKey, id)
		return err
	}); err != nil {
		return err
	}
	m.state.Selected = id
	return nil
}

// Remove removes only local credentials. Existing account-bound buckets remain
// bound and inaccessible until that account logs in again (its ID is retained
// deterministically across removal via a nonsecret, lowercase email identity).
func (m *Accounts) Remove(ctx context.Context, id string) error {
	if err := m.lock(ctx); err != nil {
		return err
	}
	defer m.unlock()
	if m.state.Accounts[id] == nil {
		return core.ErrNotFound
	}
	if err := m.persist(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key = ? AND value = ?", selectedAccountKey, id)
		return err
	}); err != nil {
		return err
	}
	delete(m.state.Accounts, id)
	if m.state.Selected == id {
		m.state.Selected = ""
	}
	return nil
}

func normalizeToken(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "oauth_token=")
	if len(s) < 16 || len(s) > 8192 || !validSecret(s) || strings.ContainsRune(s, ';') {
		return "", fmt.Errorf("auth: paste only the Embedded Setup oauth_token value: %w", core.ErrInvalid)
	}
	return s, nil
}

// Login exchanges and validates a pasted oauth_token before persisting anything.
// Only cookie-derived, unbound credentials are supported.
func (m *Accounts) Login(ctx context.Context, oauthToken string) (core.Account, error) {
	token, err := normalizeToken(oauthToken)
	if err != nil {
		return core.Account{}, err
	}
	if err = m.lock(ctx); err != nil {
		return core.Account{}, err
	}
	defer m.unlock()
	androidID, err := randomHex(8)
	if err != nil {
		return core.Account{}, err
	}
	form := url.Values{"accountType": {"HOSTED_OR_GOOGLE"}, "Email": {"oauth-token@example.com"}, "has_permission": {"1"}, "add_account": {"1"}, "ACCESS_TOKEN": {"1"}, "Token": {token}, "service": {"ac2dm"}, "source": {"android"}, "androidId": {androidID}, "device_country": {"us"}, "operatorCountry": {"us"}, "lang": {"en"}, "sdk_version": {"17"}, "google_play_services_version": {"240913000"}, "client_sig": {playSig}, "callerSig": {playSig}, "droidguard_results": {"dummy123"}}
	values, status, err := m.exchange(ctx, LoginStageExchangeMaster, m.endpoints.Exchange, form)
	if err != nil {
		return core.Account{}, err
	}
	email := strings.ToLower(strings.TrimSpace(values["Email"]))
	master := values["Token"]
	if !validEmail(email) || !validSecret(master) {
		return core.Account{}, loginFailure(ctx, LoginStageExchangeMaster, status, "unknown", nil)
	}
	// Stable opaque identity supports re-login after local credential removal. It
	// is SHA-1 identity hashing only, never a content-integrity requirement.
	id := accountID(email)
	c := &credential{ID: id, Email: email, Master: master, AndroidID: androidID}
	if err = m.refresh(ctx, c); err != nil {
		return core.Account{}, err
	}
	if err = m.validate(ctx, c.bearer); err != nil {
		return core.Account{}, err
	}
	if err = ctx.Err(); err != nil {
		return core.Account{}, err
	}
	if m.state.Accounts[id] == nil && len(m.state.Accounts) >= 256 {
		return core.Account{}, errors.New("auth: account limit reached")
	}
	if err = m.persist(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO accounts(id, email, android_id, master_token) VALUES(?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET email = excluded.email, android_id = excluded.android_id, master_token = excluded.master_token", c.ID, c.Email, c.AndroidID, c.Master); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", selectedAccountKey, id)
		return err
	}); err != nil {
		return core.Account{}, err
	}
	m.state.Accounts[id] = c
	m.state.Selected = id
	return core.Account{ID: id, Email: email, Selected: true}, nil
}

// BearerToken resolves an explicit account ID, or the selected account when id
// is empty. Expiration and refresh are serialized and cancellation-aware.
func (m *Accounts) BearerToken(ctx context.Context, id string) (string, error) {
	if err := m.lock(ctx); err != nil {
		return "", err
	}
	defer m.unlock()
	c, err := m.account(id)
	if err != nil {
		return "", err
	}
	if c.bearer == "" || !time.Now().Add(30*time.Second).Before(c.expiry) {
		if err = m.refresh(ctx, c); err != nil {
			return "", err
		}
	}
	return c.bearer, nil
}

// InvalidateBearer invalidates only the rejected generation. Concurrent successful
// refreshes cannot be accidentally discarded by an older unauthorized request.
func (m *Accounts) InvalidateBearer(ctx context.Context, id, rejected string) error {
	if err := m.lock(ctx); err != nil {
		return err
	}
	defer m.unlock()
	c, err := m.account(id)
	if err != nil {
		return err
	}
	if c.bearer == rejected {
		c.bearer = ""
		c.expiry = time.Time{}
	}
	return nil
}
func (m *Accounts) refresh(ctx context.Context, c *credential) error {
	f := url.Values{"androidId": {c.AndroidID}, "app": {photosPackage}, "callerPkg": {photosPackage}, "callerSig": {photosSig}, "client_sig": {photosSig}, "device_country": {"us"}, "Email": {c.Email}, "google_play_services_version": {"240913000"}, "lang": {"en_US"}, "oauth2_foreground": {"1"}, "operatorCountry": {"us"}, "sdk_version": {"33"}, "service": {photosService}, "source": {"android"}, "Token": {c.Master}}
	v, status, err := m.exchange(ctx, LoginStageExchangeBearer, m.endpoints.Bearer, f)
	if err != nil {
		return err
	}
	if v["TokenEncrypted"] == "1" {
		return loginFailure(ctx, LoginStageExchangeBearer, status, "unknown", nil)
	}
	expiry, err := strconv.ParseInt(v["Expiry"], 10, 64)
	if err != nil || expiry <= time.Now().Unix() || !validSecret(v["Auth"]) {
		return loginFailure(ctx, LoginStageExchangeBearer, status, "unknown", nil)
	}
	c.bearer = v["Auth"]
	c.expiry = time.Unix(expiry, 0)
	return nil
}
func (m *Accounts) exchange(ctx context.Context, stage LoginStage, endpoint string, form url.Values) (map[string]string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, loginFailure(ctx, stage, 0, "unknown", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "GoogleAuth/1.4")
	if form.Get("app") != "" {
		req.Header.Set("User-Agent", "GoogleAuth/1.4 (Pixel XL PQ2A.190205.001); gzip")
		req.Header.Set("app", photosPackage)
		req.Header.Set("device", form.Get("androidId"))
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, 0, loginFailure(ctx, stage, 0, "unknown", err)
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	// Error responses are bounded too; only an allowlisted Error identifier may escape.
	body, err := readBody(resp.Body, 64<<10)
	if err != nil || ctx.Err() != nil {
		return nil, status, loginFailure(ctx, stage, status, "unknown", err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		p := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(p) == 2 {
			if _, ok := values[p[0]]; ok {
				return nil, status, loginFailure(ctx, stage, status, "unknown", nil)
			}
			values[p[0]] = p[1]
		}
	}
	if status < 200 || status >= 300 || values["Error"] != "" {
		return nil, status, loginFailure(ctx, stage, status, values["Error"], nil)
	}
	return values, status, nil
}
func (m *Accounts) validate(ctx context.Context, token string) error {
	// HashCheck: 1:{1:{1:<20 zero bytes>},2:{}}. No user media is changed.
	body := blob(1, join(blob(1, blob(1, make([]byte, 20))), blob(2, nil)))
	req, err := http.NewRequestWithContext(ctx, "POST", m.endpoints.Validate, strings.NewReader(string(body)))
	if err != nil {
		return loginFailure(ctx, LoginStageValidatePhotos, 0, "unknown", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "en_US")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := m.client.Do(req)
	if err != nil {
		return loginFailure(ctx, LoginStageValidatePhotos, 0, "unknown", err)
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	b, err := readBody(resp.Body, 1<<20)
	if err != nil || ctx.Err() != nil || status < 200 || status >= 300 {
		return loginFailure(ctx, LoginStageValidatePhotos, status, "unknown", err)
	}
	// The verified RemoteMatches media-key path is 1.2.2.1; a zero hash
	// must not match. Reuse the strict protobuf path reader used by RPCs.
	match, err := bytesAt(b, 1, 2, 2, 1)
	if err != nil || len(match) != 0 {
		return loginFailure(ctx, LoginStageValidatePhotos, status, "unknown", err)
	}
	return nil
}

func accountID(email string) string {
	sum := sha1.Sum([]byte("guludrive-account-v1:" + email))
	return hex.EncodeToString(sum[:16])
}
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		return "", errors.New("auth: secure random generation failed")
	}
	return hex.EncodeToString(b), nil
}

// persist holds a database transaction only for local writes. Callers update
// memory after commit, and must complete all remote authentication beforehand.
func (m *Accounts) persist(ctx context.Context, change func(*sql.Tx) error) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback()
		if err = change(tx); err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Driver errors can contain credential values or SQL details.
		return errors.New("auth: cannot persist account store")
	}
	return nil
}
