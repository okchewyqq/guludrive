package core

import (
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// NormalizePath canonicalizes an already URL-decoded absolute VFS path.
// An empty path denotes root. Invalid UTF-8, parent ("..") segments, backslashes,
// NUL, and control characters are rejected before cleaning; repeated/trailing slashes
// and "." segments are normalized. Percent escapes remain literal: neither
// this function nor its callers should URL-decode its result a second time.
func NormalizePath(p string) (string, error) {
	if !utf8.ValidString(p) {
		return "", fmt.Errorf("%w: path must be valid UTF-8", ErrInvalid)
	}
	if p == "" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: path must be absolute", ErrInvalid)
	}
	for _, r := range p {
		if r == '\\' || unicode.IsControl(r) {
			return "", fmt.Errorf("%w: path contains a backslash or control character", ErrInvalid)
		}
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: parent path segments are not allowed", ErrInvalid)
		}
	}
	return path.Clean(p), nil
}

// BucketName returns the first component of a normalized VFS path, or an empty
// string for root. It validates its input with NormalizePath before extracting.
func BucketName(p string) (string, error) {
	p, err := NormalizePath(p)
	if err != nil {
		return "", err
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	return name, nil
}
