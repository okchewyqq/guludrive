package api

import (
	"context"
	"strings"
	"unicode/utf8"

	"guludrive/ui/i18n"
)

func validProxyPassword(password string) bool {
	return password != "" && len(password) <= 4096 && utf8.ValidString(password) && !strings.ContainsAny(password, "\r\n\x00")
}

// GenerateProxyScript is input-only and never persists or follows its response.
func (c *Client) GenerateProxyScript(ctx context.Context, publicOrigin, password string) (string, error) {
	if !validWorkerURL(publicOrigin) {
		return "", i18n.Err("Enter an HTTP(S) server public address without credentials, query parameters or a fragment")
	}
	if !validProxyPassword(password) {
		return "", i18n.Err("Proxy password must contain 1–4096 UTF-8 bytes without CR, LF or NUL")
	}
	var wire struct {
		Script *string `json:"script"`
	}
	if err := c.json(ctx, "POST", "/api/proxy-script", map[string]string{"publicOrigin": publicOrigin, "password": password}, &wire); err != nil {
		return "", adminError(hideSecret(err, password))
	}
	if wire.Script == nil || *wire.Script == "" {
		return "", i18n.Err("The server returned an invalid API response")
	}
	return *wire.Script, nil
}
