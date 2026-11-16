// Package i18n provides immutable catalogs and explicit, per-window locales.
package i18n

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Locale string

const (
	System  Locale = "system"
	Chinese Locale = "zh"
	English Locale = "en"
)

func Parse(s string) Locale {
	if s == string(English) {
		return English
	}
	return Chinese
}
func (l Locale) Valid() bool { return l == System || l == Chinese || l == English }

//go:embed locales/*.json
var resources embed.FS
var catalogs = loadCatalogs()

func loadCatalogs() map[Locale]map[string]string {
	out := map[Locale]map[string]string{}
	for _, l := range []Locale{Chinese, English} {
		b, err := resources.ReadFile("locales/" + string(l) + ".json")
		if err != nil {
			panic(err)
		}
		var m map[string]string
		if err = json.Unmarshal(b, &m); err != nil {
			panic(err)
		}
		out[l] = m
	}
	return out
}
func exists(id string) bool { _, ok := catalogs[English][id]; return ok }
func T(l Locale, id string, args ...any) string {
	l = Parse(string(l))
	s, ok := catalogs[l][id]
	if !ok {
		s = catalogs[English][id]
	}
	if s == "" {
		s = id
	}
	if len(args) == 0 {
		return s
	}
	v := make([]any, len(args))
	for i, a := range args {
		switch x := a.(type) {
		case *Message:
			v[i] = x.Text(l)
		case error:
			v[i] = ErrorText(l, x)
		default:
			v[i] = a
		}
	}
	return fmt.Sprintf(s, v...)
}

// Message preserves an error's identity and data instead of freezing its locale.
// English msgids are stable catalog keys, never filenames or operation IDs.
type Message struct {
	ID   string
	Args []any
}

func Err(id string, args ...any) error  { return &Message{ID: id, Args: args} }
func (m *Message) Text(l Locale) string { return T(l, m.ID, m.Args...) }
func (m *Message) Error() string        { return m.Text(English) }
func (m *Message) Unwrap() error {
	for _, a := range m.Args {
		if e, ok := a.(error); ok {
			return e
		}
	}
	return nil
}
func ErrorText(l Locale, err error) string {
	if err == nil {
		return ""
	}
	if m, ok := err.(*Message); ok {
		return m.Text(l)
	}
	if localized, ok := err.(interface{ Localized(Locale) string }); ok {
		return localized.Localized(l)
	}
	if group, ok := err.(interface{ Unwrap() []error }); ok {
		parts := []string{}
		for _, e := range group.Unwrap() {
			parts = append(parts, ErrorText(l, e))
		}
		return strings.Join(parts, "\n")
	}
	if exists(err.Error()) {
		return T(l, err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return T(l, "Request timed out. Check the connection and refresh before retrying.")
	}
	return err.Error()
}
func Modified(l Locale, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	if Parse(string(l)) == Chinese {
		return t.Local().Format("2006年01月02日 15:04")
	}
	return t.Local().Format("02 Jan 2006 · 15:04")
}
