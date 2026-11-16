package api

import (
	"context"

	"guludrive/ui/i18n"
)

// ValidateInitialAdmin uses the same credential rules as administrator updates.
func ValidateInitialAdmin(username, password string) error {
	if !validAdminUsername(username) {
		return i18n.Err("Username must contain 1–256 UTF-8 bytes, without control characters or a colon")
	}
	if password == "" {
		return i18n.Err("Enter your administrator username and password")
	}
	if !validAdminPassword(password) {
		return i18n.Err("Passwords must be valid UTF-8, at most 72 bytes, without CR, LF or NUL")
	}
	return nil
}
func (c *Client) GetSetup(ctx context.Context) (SetupStatus, error) {
	var wire struct {
		Initialized *bool `json:"initialized"`
	}
	if err := c.json(ctx, "GET", "/api/setup", nil, &wire); err != nil {
		return SetupStatus{}, adminError(err)
	}
	if wire.Initialized == nil {
		return SetupStatus{}, i18n.Err("The server returned an invalid API response")
	}
	return SetupStatus{Initialized: *wire.Initialized}, nil
}
func (c *Client) Initialize(ctx context.Context, username, password string) error {
	if err := ValidateInitialAdmin(username, password); err != nil {
		return err
	}
	var wire struct {
		Initialized *bool `json:"initialized"`
	}
	if err := c.json(ctx, "POST", "/api/setup", map[string]string{"username": username, "password": password}, &wire); err != nil {
		return adminError(hideSecret(err, password))
	}
	if wire.Initialized == nil || !*wire.Initialized {
		return i18n.Err("The server returned an invalid API response")
	}
	return nil
}
