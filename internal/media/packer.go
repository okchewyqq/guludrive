// Package media implements the current BMP pixel-area and MP4 UUID carriers.
package media

import (
	"context"
	"crypto/sha1" // Google Photos' upload protocol requires whole-carrier SHA-1.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"guludrive/core"
)

const (
	BMPFormat                 = "guludrive-bmp-v2"
	VideoFormat               = "guludrive-mp4-v2"
	MaxBMPCarrierSize   int64 = 150_000_000
	MaxVideoCarrierSize int64 = 9_000_000_000
	MaxCarrierSize            = MaxVideoCarrierSize
)

// MaxPayloadSize is the maximum payload of one MP4 carrier, not a logical-file cap.
var MaxPayloadSize int64 = MaxVideoCarrierSize - VideoPayloadOffset

// Packer is immutable and safe for concurrent use with distinct destinations.
// Logical filenames belong to SQLite, not to carrier headers or payload bytes.
type Packer struct{}

var _ core.Packer = (*Packer)(nil)

func New() (*Packer, error) {
	if err := validateVideoTemplate(); err != nil {
		return nil, err
	}
	return &Packer{}, nil
}

// carrierPlan contains only bounded framing bytes, never logical payload bytes.
type carrierPlan struct {
	format        string
	prefix        []byte
	size          int64
	payloadOffset int64
	payloadSize   int64
	padding       int64
}

func invalid() error { return fmt.Errorf("invalid carrier: %w", core.ErrInvalid) }

func safeError(action string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		err = le.Err
	}
	return fmt.Errorf("carrier %s: %w", action, err)
}

// PackPart wraps exactly the planned logical source range, retaining its kind
// even for a small final part. Publication is atomic; framing and a 128 KiB
// buffer are the only content held in memory. The complete source must remain
// an unchanged regular file throughout packing.
func (p *Packer) PackPart(ctx context.Context, sourcePath, destinationPath string, part core.PartPlan) (core.Carrier, error) {
	return packFile(ctx, sourcePath, destinationPath, part, planPart)
}

func packFile(ctx context.Context, sourcePath, destinationPath string, part core.PartPlan, planner func(core.PartPlan) (carrierPlan, error)) (core.Carrier, error) {
	var result core.Carrier
	if err := ctx.Err(); err != nil {
		return result, err
	}
	src, err := os.Open(sourcePath)
	if err != nil {
		return result, safeError("open source", err)
	}
	defer src.Close()
	before, err := src.Stat()
	if err != nil {
		return result, safeError("stat source", err)
	}
	if !before.Mode().IsRegular() || before.Size() < 0 {
		return result, core.ErrInvalid
	}
	if part.Offset < 0 || part.Length < 0 || part.Offset > before.Size() || part.Length > before.Size()-part.Offset {
		return result, core.ErrInvalid
	}
	plan, err := planner(part)
	if err != nil {
		return result, err
	}
	if dst, statErr := os.Stat(destinationPath); statErr == nil && os.SameFile(before, dst) {
		return result, core.ErrInvalid
	}
	// The caller journals destinationPath before packing, so crash recovery can
	// remove this exact unpublished path. Never truncate another packer's file.
	tempPath := destinationPath + ".packing"
	tmp, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, safeError("create destination", err)
	}
	defer func() { tmp.Close(); os.Remove(tempPath) }()
	hash := sha1.New()
	out := io.MultiWriter(tmp, hash)
	if _, err := out.Write(plan.prefix); err != nil {
		return result, safeError("write header", err)
	}
	section := io.NewSectionReader(src, part.Offset, part.Length)
	remaining := part.Length
	buf := make([]byte, 128*1024)
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		n := min(int64(len(buf)), remaining)
		got, err := io.ReadFull(section, buf[:int(n)])
		if err != nil {
			return result, fmt.Errorf("carrier source changed or unreadable: %w", core.ErrConflict)
		}
		if _, err := out.Write(buf[:got]); err != nil {
			return result, safeError("write payload", err)
		}
		remaining -= int64(got)
	}
	var extra [1]byte
	if n, err := section.Read(extra[:]); n != 0 || err != io.EOF {
		return result, fmt.Errorf("carrier source changed: %w", core.ErrConflict)
	}
	after, err := src.Stat()
	if err != nil {
		return result, safeError("stat source", err)
	}
	current, err := os.Stat(sourcePath)
	if err != nil {
		return result, fmt.Errorf("carrier source changed: %w", core.ErrConflict)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !os.SameFile(before, current) || current.Size() != before.Size() || !current.ModTime().Equal(before.ModTime()) {
		return result, fmt.Errorf("carrier source changed: %w", core.ErrConflict)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	clear(buf[:int(plan.padding)])
	if _, err := out.Write(buf[:int(plan.padding)]); err != nil {
		return result, safeError("write padding", err)
	}
	if err := tmp.Sync(); err != nil {
		return result, safeError("sync", err)
	}
	if err := tmp.Close(); err != nil {
		return result, safeError("close", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := os.Rename(tempPath, destinationPath); err != nil {
		return result, safeError("publish", err)
	}
	return core.Carrier{Path: destinationPath, Format: plan.format, SHA1: hex.EncodeToString(hash.Sum(nil)), Size: plan.size, PayloadOffset: plan.payloadOffset, PayloadSize: part.Length}, nil
}

// Inspect is for upload sealing/recovery only. It checks the current framing
// and exact file length using bounded prefix reads, never reading or hashing
// the payload or BMP row padding. PackPart supplies the upload SHA-1.
func (p *Packer) Inspect(path string) (core.Carrier, error) {
	var result core.Carrier
	f, err := os.Open(path)
	if err != nil {
		return result, safeError("open", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return result, safeError("stat", err)
	}
	if !info.Mode().IsRegular() || info.Size() < BMPPayloadOffset || info.Size() > MaxCarrierSize {
		return result, invalid()
	}
	var lead [8]byte
	if _, err := io.ReadFull(f, lead[:]); err != nil {
		return result, invalid()
	}
	headerSize := int64(BMPPayloadOffset)
	switch {
	case string(lead[:2]) == "BM":
	case string(lead[4:8]) == "ftyp":
		headerSize = VideoPayloadOffset
	default:
		return result, invalid()
	}
	if info.Size() < headerSize {
		return result, invalid()
	}
	header := make([]byte, int(headerSize))
	copy(header, lead[:])
	if _, err := io.ReadFull(f, header[len(lead):]); err != nil {
		return result, invalid()
	}
	var plan carrierPlan
	if string(lead[:2]) == "BM" {
		plan, err = inspectImage(header, info.Size())
	} else {
		plan, err = inspectVideo(header, info.Size())
	}
	if err != nil {
		return result, err
	}
	after, err := f.Stat()
	if err != nil {
		return result, safeError("stat", err)
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return result, core.ErrConflict
	}
	return core.Carrier{Path: path, Format: plan.format, Size: info.Size(), PayloadOffset: plan.payloadOffset, PayloadSize: plan.payloadSize}, nil
}
