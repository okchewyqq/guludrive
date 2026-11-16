package app

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"

	"guludrive/core"
)

// privateRoot validates the data directory and takes the instance lock before
// any database or service is opened. Never unlink the lockfile.
func privateRoot(dataDir string) (string, *flock.Flock, error) {
	if dataDir == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", nil, errors.New("app: cannot locate executable directory; specify -data-dir")
		}
		executable, err = filepath.EvalSymlinks(executable)
		if err != nil {
			return "", nil, errors.New("app: cannot resolve executable directory; specify -data-dir")
		}
		dataDir = filepath.Dir(executable)
	}
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return "", nil, errors.New("app: invalid data directory")
	}
	if filepath.Dir(root) == root {
		return "", nil, errors.New("app: data directory must not be a filesystem root")
	}
	if resolved, e := filepath.EvalSymlinks(root); e == nil && filepath.Dir(resolved) == resolved {
		return "", nil, errors.New("app: data directory must not be a filesystem root")
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", nil, errors.New("app: cannot create data directory")
	}
	if st, e := os.Lstat(root); e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", nil, errors.New("app: data directory must be a directory, not a symlink")
	}
	// Resolve after creation too: previously missing paths can traverse an
	// intermediate symlink or junction.
	root, err = canonicalPath(root)
	if err != nil {
		return "", nil, errors.New("app: cannot resolve data directory")
	}
	if err = os.Chmod(root, 0700); err != nil {
		return "", nil, errors.New("app: cannot restrict data directory")
	}
	lockPath := filepath.Join(root, "instance.lock")
	if st, e := os.Lstat(lockPath); e == nil && (!st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0) {
		return "", nil, errors.New("app: instance lock must be a regular file")
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		_ = lock.Close()
		return "", nil, errors.New("app: cannot lock data directory")
	}
	if !locked {
		_ = lock.Close()
		return "", nil, errors.Join(errors.New("app: data directory is already in use"), core.ErrBusy)
	}
	return root, lock, nil
}

// canonicalPath also resolves existing parents of a not-yet-created path.
// Broken links and other resolution errors fail closed rather than hiding an
// unknown target behind a lexical path.
func canonicalPath(p string) (string, error) {
	absolute, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, statErr := os.Lstat(absolute); !errors.Is(statErr, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(absolute)
	if parent == absolute {
		return "", err
	}
	resolved, err = canonicalPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(absolute)), nil
}
