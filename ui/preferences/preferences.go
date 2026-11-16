// Package preferences stores only non-secret language and appearance choices.
package preferences

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"guludrive/ui/i18n"
)

type Theme string

const (
	System Theme = "system"
	Light  Theme = "light"
	Dark   Theme = "dark"
)

func (t Theme) Valid() bool { return t == System || t == Light || t == Dark }

type Choices struct {
	Language i18n.Locale `json:"language"`
	Theme    Theme       `json:"theme"`
	// Server is the optional user-chosen server; empty means the co-hosted default.
	Server string `json:"server,omitempty"`
}

func Defaults() Choices { return Choices{Language: i18n.System, Theme: System} }
func (c Choices) Valid() bool {
	return c.Language.Valid() && c.Theme.Valid() && (c.Server == "" || ValidServerURL(c.Server))
}

// ValidServerURL mirrors the address rule of api.New: an absolute http(s) URL
// with a host and without userinfo, query or fragment. It is duplicated here
// so this package never imports the API client.
func ValidServerURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// The persistence schema cannot accept credentials or arbitrary UI state.
func Encode(choices Choices) ([]byte, error) {
	if !choices.Valid() {
		return nil, errors.New("invalid preferences")
	}
	return json.Marshal(choices)
}
func Decode(raw []byte) (Choices, error) {
	var d Choices
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil || !d.Valid() {
		return Defaults(), errors.New("invalid preferences")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return Defaults(), errors.New("invalid trailing preferences data")
	}
	return d, nil
}

type FileStore struct{ Directory string }

func (s FileStore) filename() (string, error) {
	if s.Directory == "" {
		return "", errors.New("missing preferences directory")
	}
	d, err := filepath.Abs(s.Directory)
	if err != nil || filepath.Dir(d) == d {
		return "", errors.New("invalid preferences directory")
	}
	return filepath.Join(d, "preferences.json"), nil
}
func (s FileStore) Load() (Choices, error) {
	name, err := s.filename()
	if err != nil {
		return Defaults(), err
	}
	info, err := os.Lstat(name)
	if os.IsNotExist(err) {
		return Defaults(), nil
	}
	if err != nil {
		return Defaults(), err
	}
	if !info.Mode().IsRegular() {
		return Defaults(), errors.New("preferences must be a regular file")
	}
	f, err := os.Open(name)
	if err != nil {
		return Defaults(), err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return Defaults(), errors.New("invalid preferences size")
	}
	return Decode(b)
}
func (s FileStore) Save(choices Choices) error {
	b, err := Encode(choices)
	if err != nil {
		return err
	}
	name, err := s.filename()
	if err != nil {
		return err
	}
	dir := filepath.Dir(name)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("preferences directory cannot be a link")
	}
	if err = protect(dir, true); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".preferences-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if err = protect(tmp, false); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}
