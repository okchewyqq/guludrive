package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"guludrive/ui/i18n"
)

type AdminUpdate struct {
	CurrentPassword string  `json:"currentPassword"`
	Username        *string `json:"username,omitempty"`
	NewPassword     string  `json:"newPassword,omitempty"`
}

func validAdminUsername(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.ContainsRune(value, ':') {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

func validAdminPassword(value string) bool {
	return utf8.ValidString(value) && len(value) <= 72 && !strings.ContainsAny(value, "\r\n\x00")
}

// Administrator endpoints never echo server prose, password hashes or secrets.
func adminError(err error) error {
	var remote *Error
	if errors.As(err, &remote) && remote.Code != invalidCurrentPassword {
		return &Error{Status: remote.Status, Message: fmt.Sprintf("Server returned HTTP %d (%s)", remote.Status, http.StatusText(remote.Status)), fallback: true}
	}
	return err
}

func (c *Client) AdminProfile(ctx context.Context) (AdminProfile, error) {
	var wire struct {
		Username *string `json:"username"`
	}
	if err := c.json(ctx, "GET", "/api/admin", nil, &wire); err != nil {
		return AdminProfile{}, adminError(err)
	}
	if wire.Username == nil || !validAdminUsername(*wire.Username) {
		return AdminProfile{}, i18n.Err("The server returned an invalid API response")
	}
	return AdminProfile{Username: *wire.Username}, nil
}

// UpdateAdmin does not log out: success has already invalidated every session.
// The HTTP cookie jar processes the server's cookie deletion normally.
func (c *Client) UpdateAdmin(ctx context.Context, input AdminUpdate) (AdminProfile, error) {
	var wire struct {
		Username *string `json:"username"`
		Relogin  *bool   `json:"relogin"`
	}
	if err := c.json(ctx, "PATCH", "/api/admin", input, &wire); err != nil {
		return AdminProfile{}, adminError(err)
	}
	if wire.Username == nil || !validAdminUsername(*wire.Username) || wire.Relogin == nil || !*wire.Relogin {
		return AdminProfile{}, i18n.Err("The server returned an invalid API response")
	}
	return AdminProfile{Username: *wire.Username}, nil
}
