package api

import (
	"context"
	"guludrive/ui/i18n"
)

// Settings are the public core contract; never a local preference or upload cap.
func validUploadSettings(v UploadSettings) bool {
	return (v.LargeFileFormat == "mp4" || v.LargeFileFormat == "bmp") &&
		v.MP4PartSize >= 1_000_000_000 && v.MP4PartSize <= 9_000_000_000 &&
		v.BMPPartSize >= 100_000_000 && v.BMPPartSize <= 150_000_000
}
func (c *Client) uploadSettings(ctx context.Context, method string, input any) (UploadSettings, error) {
	var settings UploadSettings
	if err := c.json(ctx, method, "/api/upload-settings", input, &settings); err != nil {
		return UploadSettings{}, err
	}
	if !validUploadSettings(settings) {
		return UploadSettings{}, i18n.Err("The server returned an invalid API response")
	}
	return settings, nil
}
func (c *Client) GetUploadSettings(ctx context.Context) (UploadSettings, error) {
	return c.uploadSettings(ctx, "GET", nil)
}
func (c *Client) SetUploadSettings(ctx context.Context, input UploadSettings) (UploadSettings, error) {
	return c.uploadSettings(ctx, "PUT", input)
}
