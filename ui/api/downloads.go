package api

import (
	"context"
	"net"
	"net/url"
	"time"

	"guludrive/ui/i18n"
)

type DownloadMode string

const (
	DownloadLocal   DownloadMode = "local"
	DownloadGDirect DownloadMode = "gdirect"
	DownloadProxy   DownloadMode = "proxy"
)

func (mode DownloadMode) Valid() bool {
	return mode == DownloadLocal || mode == DownloadGDirect || mode == DownloadProxy
}

// DownloadSettings contains server configuration, never the input-only secret or runtime IP.
type DownloadSettings struct {
	Mode              DownloadMode `json:"mode"`
	WorkerURL         string       `json:"workerURL"`
	WorkerConfigured  bool         `json:"workerConfigured"`
	PublicOriginReady bool         `json:"publicOriginReady"`
	PublicOrigin      string       `json:"publicOrigin"`
}
type DownloadSettingsUpdate struct {
	Mode         DownloadMode `json:"mode"`
	WorkerURL    string       `json:"workerURL"`
	WorkerSecret string       `json:"workerSecret,omitempty"`
	PublicOrigin string       `json:"publicOrigin"`
}
type DownloadLink struct {
	URL       string       `json:"url"`
	ExpiresAt time.Time    `json:"expiresAt"`
	Mode      DownloadMode `json:"mode"`
}

func validDownloadURL(raw string) bool {
	if len(raw) == 0 || len(raw) > 16384 {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Hostname() != "" && u.User == nil && u.Fragment == "" && (u.Scheme == "https" || u.Scheme == "http")
}
func validWorkerURL(raw string) bool {
	if !validDownloadURL(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	return u.RawQuery == ""
}
func (c *Client) DownloadSettings(ctx context.Context) (DownloadSettings, error) {
	var wire struct {
		Mode              *DownloadMode `json:"mode"`
		WorkerURL         *string       `json:"workerURL"`
		WorkerConfigured  *bool         `json:"workerConfigured"`
		PublicOriginReady *bool         `json:"publicOriginReady"`
		PublicOrigin      *string       `json:"publicOrigin"`
	}
	if err := c.json(ctx, "GET", "/api/download-settings", nil, &wire); err != nil {
		return DownloadSettings{}, err
	}
	if wire.Mode == nil || !wire.Mode.Valid() || wire.WorkerURL == nil || wire.WorkerConfigured == nil || wire.PublicOriginReady == nil || wire.PublicOrigin == nil || (*wire.PublicOrigin != "" && !validWorkerURL(*wire.PublicOrigin)) ||
		(*wire.WorkerURL != "" && !validWorkerURL(*wire.WorkerURL)) ||
		(*wire.Mode == DownloadProxy && (!*wire.WorkerConfigured || *wire.WorkerURL == "")) {
		return DownloadSettings{}, i18n.Err("The server returned an invalid API response")
	}
	return DownloadSettings{Mode: *wire.Mode, WorkerURL: *wire.WorkerURL, WorkerConfigured: *wire.WorkerConfigured, PublicOriginReady: *wire.PublicOriginReady, PublicOrigin: *wire.PublicOrigin}, nil
}
func (c *Client) UpdateDownloadSettings(ctx context.Context, input DownloadSettingsUpdate) (DownloadSettings, error) {
	if !input.Mode.Valid() {
		return DownloadSettings{}, i18n.Err("Choose Local, GDirect or CF Proxy")
	}
	if input.WorkerURL != "" && !validWorkerURL(input.WorkerURL) {
		return DownloadSettings{}, i18n.Err("Enter an HTTP(S) Worker address without credentials, query parameters or a fragment")
	}
	if input.Mode == DownloadProxy && input.WorkerURL == "" {
		return DownloadSettings{}, i18n.Err("Configure a Worker address before using CF Proxy")
	}
	if input.PublicOrigin != "" && !validWorkerURL(input.PublicOrigin) {
		return DownloadSettings{}, i18n.Err("Enter an HTTP(S) server public address without credentials, query parameters or a fragment")
	}
	if input.WorkerSecret != "" && !validProxyPassword(input.WorkerSecret) {
		return DownloadSettings{}, i18n.Err("Proxy password must contain 1–4096 UTF-8 bytes without CR, LF or NUL")
	}
	if err := c.json(ctx, "PUT", "/api/download-settings", input, nil); err != nil {
		return DownloadSettings{}, hideSecret(err, input.WorkerSecret)
	}
	settings, err := c.DownloadSettings(ctx)
	return settings, hideSecret(err, input.WorkerSecret)
}

// RefreshGDirect returns only the server's runtime selection, never a cached preference.
func (c *Client) RefreshGDirect(ctx context.Context) (GDirectStatus, error) {
	var wire struct {
		Enabled   *bool   `json:"enabled"`
		IP        *string `json:"ip"`
		Selecting *bool   `json:"selecting"`
	}
	if err := c.json(ctx, "POST", "/api/gdirect/refresh", nil, &wire); err != nil {
		return GDirectStatus{}, err
	}
	if wire.Enabled == nil || wire.IP == nil || wire.Selecting == nil || (*wire.IP != "" && net.ParseIP(*wire.IP) == nil) {
		return GDirectStatus{}, i18n.Err("The server returned an invalid API response")
	}
	return GDirectStatus{Enabled: *wire.Enabled, IP: *wire.IP, Selecting: *wire.Selecting}, nil
}

// CreateDownloadLink uses the server's active mode. It never follows the returned URL.
func (c *Client) CreateDownloadLink(ctx context.Context, path string) (DownloadLink, error) {
	var link DownloadLink
	if err := c.json(ctx, "POST", "/api/download-links", map[string]string{"path": path}, &link); err != nil {
		return DownloadLink{}, err
	}
	if !link.Mode.Valid() || !validDownloadURL(link.URL) || !link.ExpiresAt.After(time.Now()) {
		return DownloadLink{}, i18n.Err("The server returned an invalid API response")
	}
	return link, nil
}
