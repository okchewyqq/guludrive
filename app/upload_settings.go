package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"guludrive/core"
	"guludrive/internal/media"
)

const (
	uploadFormatKey      = "upload.large_file_format"
	uploadMP4PartSizeKey = "upload.mp4_part_size"
	uploadBMPPartSizeKey = "upload.bmp_part_size"
)

// uploadSettings borrows the service database. One SELECT observes a coherent
// snapshot; Set commits all preferences together. The store snapshots Policy
// only when a future upload is sealed, never while receiving its input.
type uploadSettings struct{ db *sql.DB }

func (u *uploadSettings) Get(ctx context.Context) (core.UploadSettings, error) {
	settings := media.DefaultUploadSettings()
	rows, err := u.db.QueryContext(ctx, `SELECT key,value FROM settings WHERE key IN (?,?,?)`, uploadFormatKey, uploadMP4PartSizeKey, uploadBMPPartSizeKey)
	if err != nil {
		return core.UploadSettings{}, errors.New("app: cannot read upload settings")
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err = rows.Scan(&key, &value); err != nil {
			return core.UploadSettings{}, errors.New("app: cannot read upload settings")
		}
		switch key {
		case uploadFormatKey:
			settings.LargeFileFormat = value
		case uploadMP4PartSizeKey:
			settings.MP4PartSize, err = strconv.ParseInt(value, 10, 64)
		case uploadBMPPartSizeKey:
			settings.BMPPartSize, err = strconv.ParseInt(value, 10, 64)
		}
		if err != nil {
			return core.UploadSettings{}, errors.New("app: invalid persisted upload settings")
		}
	}
	if err = rows.Err(); err != nil {
		return core.UploadSettings{}, errors.New("app: cannot read upload settings")
	}
	if err = media.ValidateUploadSettings(settings); err != nil {
		return core.UploadSettings{}, errors.New("app: invalid persisted upload settings")
	}
	return settings, nil
}

func (u *uploadSettings) Set(ctx context.Context, settings core.UploadSettings) error {
	if err := media.ValidateUploadSettings(settings); err != nil {
		return core.ErrInvalid
	}
	tx, err := u.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("app: cannot save upload settings")
	}
	defer tx.Rollback()
	for _, item := range []struct{ key, value string }{
		{uploadFormatKey, settings.LargeFileFormat},
		{uploadMP4PartSizeKey, strconv.FormatInt(settings.MP4PartSize, 10)},
		{uploadBMPPartSizeKey, strconv.FormatInt(settings.BMPPartSize, 10)},
	} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, item.key, item.value); err != nil {
			return errors.New("app: cannot save upload settings")
		}
	}
	if err = tx.Commit(); err != nil {
		return errors.New("app: cannot save upload settings")
	}
	return nil
}

func (u *uploadSettings) Policy(ctx context.Context) (core.UploadPolicy, error) {
	settings, err := u.Get(ctx)
	if err != nil {
		return core.UploadPolicy{}, err
	}
	return media.PolicyFor(settings)
}

func (s *Server) uploadSettingsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		settings, err := s.uploads.Get(r.Context())
		if err != nil {
			apiFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, settings)
		return
	}
	var settings core.UploadSettings
	if !decodeJSON(w, r, &settings) {
		return
	}
	if err := s.uploads.Set(r.Context(), settings); err != nil {
		apiFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}
