// Package state contains UI-only path, sorting and formatting rules.
package state

import (
	"fmt"
	"guludrive/core"
	"guludrive/ui/i18n"
	"path"
	"sort"
	"strings"

	"guludrive/ui/api"
)

type SortOrder string

const (
	SortName   SortOrder = "name"
	SortNewest SortOrder = "newest"
	SortSize   SortOrder = "size"
)

type Crumb struct{ Name, Path string }

func Normalize(p string) (string, error) {
	clean, err := core.NormalizePath(p)
	if err != nil {
		return "", i18n.Err("Use an absolute UTF-8 path without parent segments, backslashes or control characters")
	}
	return clean, nil
}
func Name(s string) error {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\") {
		return i18n.Err("Enter a name without slashes or backslashes")
	}
	_, err := Normalize("/" + s)
	return err
}
func Join(directory, name string) (string, error) {
	if err := Name(name); err != nil {
		return "", err
	}
	d, err := Normalize(directory)
	if err != nil {
		return "", err
	}
	return path.Join(d, name), nil
}
func Destination(directory, input string) (string, error) {
	if !strings.HasPrefix(input, "/") {
		input = directory + "/" + input
	}
	return Normalize(input)
}
func Crumbs(p string) []Crumb {
	out := []Crumb{{"All buckets", "/"}}
	current := ""
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == "" {
			continue
		}
		current += "/" + part
		out = append(out, Crumb{part, current})
	}
	return out
}
func Entries(entries []api.Entry, query string, order SortOrder) []api.Entry {
	out := make([]api.Entry, 0, len(entries))
	query = strings.ToLower(query)
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Name), query) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		switch order {
		case SortNewest:
			if !a.ModTime.Equal(b.ModTime) {
				return a.ModTime.After(b.ModTime)
			}
		case SortSize:
			if a.Size != b.Size {
				return a.Size > b.Size
			}
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out
}
func Bytes(n int64) string {
	units := []string{"KB", "MB", "GB", "TB", "PB", "EB"}
	v := float64(n) / 1024
	for _, u := range units {
		if v < 1024 || u == "EB" {
			return fmt.Sprintf("%.1f %s", v, u)
		}
		v /= 1024
	}
	return ""
}
