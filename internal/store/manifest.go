package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"guludrive/core"
)

// Cancellation after a dispatch marker is conservative unless a complete
// successful receipt was returned. The backend may add ErrUncertain itself.
func unknownResult(ctx context.Context, err error) bool {
	return errors.Is(err, core.ErrUncertain) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil
}

func validPolicy(p core.UploadPolicy) bool {
	return (p.Format == "bmp" || p.Format == "mp4") && p.PartSize > 0
}

// manifestCarrierKind deliberately uses the immutable per-part plans, not the
// logical file name, content type, or policy snapshot. Mixed or unknown plans
// are unsafe to specialize and retain the standard byte-range behavior.
func manifestCarrierKind(m core.PartsManifest) string {
	if len(m.Parts) == 0 {
		return ""
	}
	kind := m.Parts[0].Plan.Kind
	if kind != "bmp" && kind != "mp4" {
		return ""
	}
	for _, part := range m.Parts[1:] {
		if part.Plan.Kind != kind {
			return ""
		}
	}
	return kind
}

// validateManifest checks metadata only, never content or download hashes.
func validateManifest(m core.PartsManifest, account string, confirmed bool) error {
	if m.Version != 1 || m.LogicalSize < 0 || !validPolicy(m.Policy) || !validReference(account) || len(m.Parts) == 0 {
		return core.ErrInvalid
	}
	var offset int64
	for i, p := range m.Parts {
		q := p.Plan
		if q.Index != i || q.Offset != offset || q.Length < 0 || q.Length > m.LogicalSize-offset || (q.Length == 0 && (m.LogicalSize != 0 || len(m.Parts) != 1)) || q.Budget <= 0 || (q.Kind != "bmp" && q.Kind != "mp4") {
			return core.ErrInvalid
		}
		offset += q.Length
		switch p.UploadState {
		case "queued", "packing":
			if confirmed {
				return core.ErrInvalid
			}
		case "sealed", "uploading", "uncertain", "confirmed":
			if !validPartCarrier(p) {
				return core.ErrInvalid
			}
		default:
			return core.ErrInvalid
		}
		if p.Remote.ID != "" && !validRemote(p.Remote, account, p.Carrier) {
			return core.ErrInvalid
		}
		if p.AlbumState != "" && p.AlbumState != "attaching" && p.AlbumState != "attached" && p.AlbumState != "uncertain" {
			return core.ErrInvalid
		}
		if p.TrashState != "" && p.TrashState != "trashing" && p.TrashState != "uncertain" && p.TrashState != "trashed" {
			return core.ErrInvalid
		}
		if p.UploadState == "confirmed" {
			if !validRemote(p.Remote, account, p.Carrier) {
				return core.ErrInvalid
			}
		} else if confirmed {
			return core.ErrInvalid
		}
		if confirmed && p.TrashState != "" {
			return core.ErrBusy
		}
	}
	if offset != m.LogicalSize {
		return core.ErrInvalid
	}
	for album, states := range m.AlbumStates {
		if !validReference(album) || len(states) != len(m.Parts) {
			return core.ErrInvalid
		}
		for _, state := range states {
			if state != "" && state != "attached" && state != "attaching" && state != "uncertain" {
				return core.ErrInvalid
			}
		}
	}
	return nil
}

func validPartCarrier(p core.ObjectPart) bool {
	c := p.Carrier
	return c.Format != "" && c.Size >= 0 && c.Size <= p.Plan.Budget && c.PayloadOffset >= 0 && c.PayloadOffset <= c.Size && c.PayloadSize >= 0 && c.PayloadSize == p.Plan.Length && c.PayloadSize <= c.Size-c.PayloadOffset && c.Size-c.PayloadSize <= carrierAllowance
}

func validRemote(r core.RemoteObject, account string, c core.Carrier) bool {
	return validReference(r.ID) && r.AccountID == account && r.Size == c.Size && (c.SHA1 == "" || r.SHA1 == "" || strings.EqualFold(c.SHA1, r.SHA1))
}

func decodeManifest(raw, account string) (core.PartsManifest, error) {
	var m core.PartsManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return m, core.ErrInvalid
	}
	return m, validateManifest(m, account, false)
}

func manifestJSON(m core.PartsManifest) (string, error) {
	raw, err := json.Marshal(m)
	return string(raw), err
}

type executer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func saveManifest(ctx context.Context, q executer, o objectRecord) error {
	raw, err := manifestJSON(o.manifest)
	if err != nil {
		return err
	}
	res, err := q.ExecContext(ctx, "UPDATE objects SET parts=? WHERE id=?", raw, o.id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return core.ErrConflict
	}
	return nil
}

func (s *Store) partPath(id string, index int) (string, error) {
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != 16 || index < 0 || strings.ToLower(id) != id {
		return "", core.ErrInvalid
	}
	return filepath.Join(s.cfg.DataDir, "temp", fmt.Sprintf("%s.%d.carrier", id, index)), nil
}

func membership(m *core.PartsManifest, album string) []string {
	if m.AlbumStates == nil {
		m.AlbumStates = map[string][]string{}
	}
	if m.AlbumStates[album] == nil {
		m.AlbumStates[album] = make([]string, len(m.Parts))
	}
	return m.AlbumStates[album]
}
