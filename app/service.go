// Package app assembles the Google Photos file service shared by CLI and UI.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"

	"github.com/gofrs/flock"

	"guludrive/core"
	"guludrive/internal/database"
	"guludrive/internal/gdirect"
	"guludrive/internal/media"
	"guludrive/internal/photos"
	"guludrive/internal/store"
	"guludrive/internal/webui"
)

// Version can be set with -ldflags -X guludrive/app.Version=... .
var Version = "dev"

// Config contains internal service settings, never administrator or Google
// credentials. An empty DataDir uses the executable directory.
// MaxFileSize is an optional logical file cap; zero
// means no configured total cap. Individual carriers have separate budgets.
type Config struct {
	DataDir      string `json:"dataDir"`
	Listen       string `json:"listen"`
	Proxy        string `json:"proxy"`
	PublicOrigin string `json:"publicOrigin"`
	MaxFileSize  int64  `json:"maxFileSize"`
}

// downloadRouter is the runtime routing boundary; persistence and HTTP policy
// remain in app, while the router alone owns IP selection and cancellation.
type downloadRouter interface {
	SetEnabled(bool)
	Status() core.GDirectStatus
	Refresh(context.Context) (core.GDirectStatus, error)
}

// Server owns services and active requests, but not the listener/http.Server.
type Server struct {
	handler   http.Handler
	fs        core.FileSystem
	accounts  core.Accounts
	security  *security
	downloads *downloads
	uploads   *uploadSettings
	gdirect   downloadRouter
	maxFile   int64
	gate      chan struct{}
	lock      *flock.Flock
	db        *sql.DB
	closers   []io.Closer
	lifetime  context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	closing   bool
	requests  sync.WaitGroup
	done      chan struct{}
	closeErr  error
}

// Open creates a Google-only service. Empty account state is valid; opening it
// does not log in to Google. Runtime data is never migrated or deleted here.
func Open(ctx context.Context, cfg Config) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.MaxFileSize < 0 {
		return nil, fmt.Errorf("app: maxFileSize must be nonnegative (0 means no configured total cap): %w", core.ErrInvalid)
	}
	origin, err := canonicalOrigin(cfg.PublicOrigin)
	if err != nil {
		return nil, errors.New("app: public origin must be an HTTP(S) origin without a path, query, fragment or userinfo")
	}
	cfg.PublicOrigin = origin
	root, lock, err := privateRoot(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = root
	var db *sql.DB
	var filesystem core.FileSystem
	var closers []io.Closer
	opened := false
	defer func() {
		if !opened {
			if filesystem != nil {
				_ = filesystem.Close()
			}
			for i := len(closers) - 1; i >= 0; i-- {
				_ = closers[i].Close()
			}
			if db != nil {
				_ = db.Close()
			}
			_ = lock.Close() // Always last; never unlink the lockfile.
		}
	}()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	db, err = database.Open(ctx, root)
	if err != nil {
		return nil, err
	}
	admin, err := loadAdministrator(ctx, db)
	if err != nil {
		return nil, err
	}
	downloads, err := loadDownloads(ctx, db)
	if err != nil {
		return nil, err
	}
	if downloads.settings.publicOrigin != "" {
		cfg.PublicOrigin = downloads.settings.publicOrigin
	}
	uploads := &uploadSettings{db: db}
	if _, err = uploads.Get(ctx); err != nil {
		return nil, err
	}
	router, err := gdirect.New(cfg.Proxy)
	if err != nil {
		return nil, errors.New("app: cannot initialize Google routing; check proxy settings")
	}
	// Register first so reverse-order shutdown closes routing after the store
	// and Google clients, but before their shared database.
	closers = append(closers, router)
	router.SetEnabled(downloads.settings.mode == "gdirect")
	googleHTTP := router.Client()
	accounts, err := photos.OpenAccounts(db, photos.AuthOptions{HTTPClient: googleHTTP})
	if err != nil {
		return nil, errors.New("app: cannot open Google accounts")
	}
	if closer, ok := any(accounts).(io.Closer); ok {
		closers = append(closers, closer)
	}
	// Photos retains its existing-object download bound independently of the
	// current media upload caps, so older larger carriers remain readable.
	client, err := photos.New(accounts, photos.ClientOptions{HTTPClient: googleHTTP})
	if err != nil {
		return nil, errors.New("app: cannot initialize Google client; check proxy settings")
	}
	if closer, ok := any(client).(io.Closer); ok {
		closers = append(closers, closer)
	}
	packer, err := media.New()
	if err != nil {
		return nil, errors.New("app: cannot initialize file carrier")
	}
	st, err := store.Open(ctx, store.Config{DB: db, DataDir: root, Backend: client, Accounts: accounts, Packer: packer, MaxFileSize: cfg.MaxFileSize, UploadPolicy: uploads.Policy})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, core.ErrUncertain) {
			return nil, errors.Join(errors.New("app: uncertain remote outcome; preserve the data directory and do not retry automatically"), core.ErrUncertain)
		}
		return nil, errors.New("app: cannot open namespace; preserve the data directory for recovery")
	}
	filesystem = st
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	s, err := newServer(ctx, cfg, filesystem, accounts, admin)
	if err != nil {
		return nil, err
	}
	s.db, s.lock, s.closers, s.downloads, s.gdirect, s.uploads = db, lock, closers, downloads, router, uploads
	opened = true
	s.startGDirect()
	return s, nil
}

func newServer(ctx context.Context, cfg Config, filesystem core.FileSystem, accounts core.Accounts, admin administrator) (*Server, error) {
	if filesystem == nil || accounts == nil || cfg.MaxFileSize < 0 {
		return nil, core.ErrInvalid
	}
	if cfg.MaxFileSize == 0 {
		cfg.MaxFileSize = math.MaxInt64
	}
	s := &Server{fs: filesystem, accounts: accounts, maxFile: cfg.MaxFileSize, gate: make(chan struct{}, 1), done: make(chan struct{})}
	s.security = newSecurity(cfg, admin)
	lifetime, cancel := context.WithCancel(ctx)
	s.lifetime, s.cancel = lifetime, cancel
	mux := http.NewServeMux()
	api := s.serialize(s.security.api(http.HandlerFunc(s.api)))
	mux.Handle("/api/", api)
	mux.Handle("/api", api)
	mux.HandleFunc("/download", s.signedDownload)
	mux.HandleFunc("/download-manifest", s.downloadManifest)
	dav := s.davHandler()
	mux.Handle("/dav/", dav)
	mux.Handle("/dav", dav)
	mux.Handle("/", staticHandler(webui.Assets()))
	s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			httpError(w, r, http.StatusServiceUnavailable)
			return
		}
		s.requests.Add(1)
		s.mu.Unlock()
		defer s.requests.Done()
		requestCtx, release := context.WithCancel(r.Context())
		stop := context.AfterFunc(lifetime, release)
		stopBody := context.AfterFunc(requestCtx, func() {
			if r.Body != nil {
				_ = r.Body.Close()
			}
		})
		defer stop()
		defer stopBody()
		defer release()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		mux.ServeHTTP(w, r.WithContext(requestCtx))
	})
	return s, nil
}

// startGDirect runs only after all service resources have been assembled. The
// server lifetime cancels startup probing, and shutdown drains it with requests.
func (s *Server) startGDirect() {
	if s.downloads.settings.mode != "gdirect" {
		return
	}
	s.requests.Add(1)
	go func() {
		defer s.requests.Done()
		_, _ = s.gdirect.Refresh(s.lifetime)
	}()
}

// Handler returns the reusable HTTP boundary. The caller owns its listener.
func (s *Server) Handler() http.Handler { return s.handler }

// Close stops admission, cancels and drains requests, then closes resources.
// Cleanup continues after a caller's deadline. The instance lock is released last.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closing {
		s.closing = true
		s.cancel()
		go func() {
			s.requests.Wait()
			err := s.fs.Close()
			for i := len(s.closers) - 1; i >= 0; i-- {
				err = errors.Join(err, s.closers[i].Close())
			}
			if s.db != nil {
				err = errors.Join(err, s.db.Close())
			}
			if s.lock != nil {
				err = errors.Join(err, s.lock.Close())
			}
			if err != nil {
				s.closeErr = errors.New("app: shutdown incomplete; preserve the data directory")
				if errors.Is(err, core.ErrUncertain) {
					s.closeErr = errors.Join(s.closeErr, core.ErrUncertain)
				}
			}
			close(s.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return s.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// removeAccount is the single coordination policy between account storage and
// the bucket namespace. The transport mutation gate also covers bucket creation.
func (s *Server) removeAccount(ctx context.Context, id string) error {
	buckets, err := s.fs.ListBuckets(ctx)
	if err != nil {
		return err
	}
	for _, bucket := range buckets {
		if bucket.Album.AccountID == id {
			return core.ErrConflict
		}
	}
	// Buckets are not the only owners: unreferenced objects can still need GC,
	// and interrupted/uncertain receipts must retain their Google credentials.
	// The transport mutation gate excludes new namespace/account mutations;
	// this bounded read holds no transaction across account removal.
	var referenced bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM objects WHERE account=?) OR EXISTS(SELECT 1 FROM jobs WHERE (account=? OR remote_account=?) AND state NOT IN ('published','aborted','failed','conflict'))`, id, id, id).Scan(&referenced)
	if err != nil {
		return errors.New("app: cannot check account references")
	}
	if referenced {
		return core.ErrConflict
	}
	return s.accounts.Remove(ctx, id)
}

// Serialize namespace/account mutations across both transports so checks and
// Remove/Copy/Move run together. This is not a DAV lock implementation.
func (s *Server) serialize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET", "HEAD", "OPTIONS", "PROPFIND":
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/session" || (r.URL.Path == "/api/gdirect/refresh" && r.Method == http.MethodPost) {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case s.gate <- struct{}{}:
			defer func() { <-s.gate }()
			next.ServeHTTP(w, r)
		case <-r.Context().Done():
			httpError(w, r, http.StatusRequestTimeout)
		}
	})
}
