// Package store implements the caller-owned, single-process SQLite filesystem.
// Its owner holds the DataDir instance lock until Close returns. Store borrows
// its database, backend and packer; local files contain only upload spools.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"

	"guludrive/core"
)

const carrierAllowance int64 = 1 << 20

type Config struct {
	// DB is initialized by internal/database and remains open until after Close.
	DB *sql.DB
	// DataDir is the root private directory; uploads use only its temp directory.
	DataDir string
	Backend core.Backend
	// Accounts supplies safe selected-account metadata for new local buckets.
	// Store borrows it and never logs in or changes account selection.
	Accounts core.Accounts
	Packer   core.Packer
	// UploadPolicy is snapshotted after the complete raw input has been synced.
	UploadPolicy func(context.Context) (core.UploadPolicy, error)
	// MaxFileSize optionally limits logical input; zero means the int64 limit.
	MaxFileSize int64
}

type Store struct {
	// Mutations are serialized, but neither network I/O nor waiting for this
	// mutex holds stateMu or blocks metadata readers.
	mutationMu sync.Mutex
	// stateMu protects lifecycle, sessions, handles and remote-object pins.
	// Read lookup/pinning and cleanup claims share this short lock.
	stateMu   sync.Mutex
	ops       sync.WaitGroup
	closed    bool
	closeDone chan struct{}
	closeErr  error
	leases    map[int64]int
	sessions  map[*writeSession]struct{}
	handles   map[*readHandle]struct{}

	db          *sql.DB
	cfg         Config
	cleanupDone chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
}

var _ core.FileSystem = (*Store)(nil)

// Open recovers durable commit intents before admitting callers, then starts
// one cleanup worker. The owner must already hold the DataDir instance lock
// and keep the borrowed database open until after Store.Close returns.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DB == nil || cfg.Backend == nil || cfg.Accounts == nil || cfg.Packer == nil || cfg.DataDir == "" || cfg.MaxFileSize < 0 || cfg.UploadPolicy == nil {
		return nil, core.ErrInvalid
	}
	if cfg.MaxFileSize == 0 {
		cfg.MaxFileSize = math.MaxInt64
	}
	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = abs
	for _, d := range []string{abs, filepath.Join(abs, "temp")} {
		if err = os.MkdirAll(d, 0700); err != nil {
			return nil, err
		}
	}
	lifetime, cancel := context.WithCancel(context.Background())
	s := &Store{db: cfg.DB, cfg: cfg, ctx: lifetime, cancel: cancel, closeDone: make(chan struct{}), cleanupDone: make(chan struct{}), leases: map[int64]int{}, sessions: map[*writeSession]struct{}{}, handles: map[*readHandle]struct{}{}}
	if err = s.recover(ctx); err != nil {
		cancel()
		return nil, err
	}
	go s.cleanup()
	return s, nil
}

// Admission and the closed check share stateMu so Add cannot race shutdown's
// zero-counter Wait. The returned context combines caller and store lifetime.
func (s *Store) start(ctx context.Context) (context.Context, func(), error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if s.closed || s.ctx.Err() != nil {
		return nil, nil, core.ErrBusy
	}
	s.ops.Add(1)
	c, cancel := s.network(ctx)
	return c, func() { cancel(); s.ops.Done() }, nil
}

func (s *Store) mutation(ctx context.Context) (context.Context, func(), error) {
	c, done, err := s.start(ctx)
	if err != nil {
		return nil, nil, err
	}
	s.mutationMu.Lock()
	if err = c.Err(); err != nil {
		s.mutationMu.Unlock()
		done()
		return nil, nil, err
	}
	return c, func() { s.mutationMu.Unlock(); done() }, nil
}

func (s *Store) network(ctx context.Context) (context.Context, func()) {
	c, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	return c, func() { stop(); cancel() }
}

func safeError(err error, op string) error {
	// Preserve only allowlisted diagnostics and shared sentinels, never the
	// provider's raw message or chain. Uncertainty must survive cancellation.
	var cause error
	for _, sent := range []error{core.ErrUncertain, context.Canceled, context.DeadlineExceeded, core.ErrNotFound, core.ErrNotDir, core.ErrIsDir, core.ErrExists, core.ErrInvalid, core.ErrConflict, core.ErrPrecondition, core.ErrUnsupported, core.ErrUnauthorized, core.ErrBusy} {
		if errors.Is(err, sent) {
			cause = errors.Join(cause, sent)
		}
	}
	var remote *core.RemoteError
	if errors.As(err, &remote) {
		diagnostic := remote.Safe()
		diagnostic.Cause = cause
		return diagnostic
	}
	if cause != nil {
		return fmt.Errorf("%s: %w", op, cause)
	}
	return fmt.Errorf("%s failed", op)
}

func (s *Store) Close() error {
	s.stateMu.Lock()
	if s.closed {
		done := s.closeDone
		s.stateMu.Unlock()
		<-done
		return s.closeErr
	}
	s.closed = true
	s.cancel()
	for w := range s.sessions {
		w.cancel()
	}
	s.stateMu.Unlock()

	// Drain operations and cleanup before closing handles and sessions. The
	// owner can close the borrowed database only after this shutdown completes.
	s.ops.Wait()
	<-s.cleanupDone
	s.stateMu.Lock()
	sessions := make([]*writeSession, 0, len(s.sessions))
	for w := range s.sessions {
		sessions = append(sessions, w)
	}
	handles := make([]*readHandle, 0, len(s.handles))
	for r := range s.handles {
		handles = append(handles, r)
	}
	s.stateMu.Unlock()
	for _, w := range sessions {
		w.mu.Lock()
		w.abortLocked()
		w.mu.Unlock()
	}
	var err error
	for _, r := range handles {
		err = errors.Join(err, r.Close())
	}
	s.stateMu.Lock()
	s.closeErr = err
	close(s.closeDone)
	s.stateMu.Unlock()
	return err
}
