package media

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"guludrive/core"
)

func TestRoundTrip(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if BMPFormat != "guludrive-bmp-v2" || BMPPayloadOffset != 86 || MaxBMPCarrierSize != 150000000 || MaxBMPPayloadSize != 149999584 {
		t.Fatal("pixel-area BMP carrier limits changed")
	}
	// Check the limit using arithmetic only, without constructing a 150 MB image.
	limit, err := planImage(MaxBMPPayloadSize)
	if err != nil || limit.height != 48828 || limit.pixelSize != 149999616 || limit.size != 149999670 || limit.padding != 0 || limit.size+rowStride <= MaxBMPCarrierSize {
		t.Fatalf("maximum image plan: %+v, %v", limit, err)
	}
	for _, size := range []int64{-1, MaxBMPPayloadSize + 1, 1<<63 - 1} {
		if _, err := planImage(size); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("invalid payload size accepted: %d", size)
		}
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"binary", []byte{0, 255, 0, 17, 128, 13, 10}},
		{"before-row-boundary", bytes.Repeat([]byte{17}, 3039)},
		{"row-boundary", bytes.Repeat([]byte{34}, 3040)},
		{"after-row-boundary", bytes.Repeat([]byte{51}, 3041)},
		{"multiple-copy-buffers", bytes.Repeat([]byte{0, 255, 17}, 128*1024/3+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "carrier.bmp")
			source := append([]byte{7, 8, 9}, tc.data...)
			source = append(source, 10)
			if err := os.WriteFile(src, source, 0600); err != nil {
				t.Fatal(err)
			}
			parts, err := p.Plan(int64(len(tc.data)), core.UploadPolicy{Format: "mp4", PartSize: minVideoPartSize})
			if err != nil || len(parts) != 1 {
				t.Fatalf("small-file plan: %v, %v", parts, err)
			}
			part := parts[0]
			part.Offset = 3
			c, err := p.PackPart(context.Background(), src, dst, part)
			if err != nil {
				t.Fatal(err)
			}
			rows := (32 + len(tc.data) + 3071) / 3072
			wantSize := int64(54 + rows*3072)
			info, err := p.Inspect(dst)
			if err != nil || info.PayloadOffset != 86 || info.PayloadSize != int64(len(tc.data)) || info.Size != wantSize || info.Format != "guludrive-bmp-v2" || info.SHA1 != "" {
				t.Fatalf("inspection: %+v, %v", info, err)
			}
			raw, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			// Standard BITMAPINFOHEADER: width 1024, bottom-up BGR24, BI_RGB.
			header, err := hex.DecodeString("424d0000000000000000360000002800000000040000010000000100180000000000000c000000000000000000000000000000000000")
			if err != nil || len(header) != 54 {
				t.Fatal("invalid golden BMP header")
			}
			binary.LittleEndian.PutUint32(header[2:6], uint32(wantSize))
			binary.LittleEndian.PutUint32(header[22:26], uint32(rows))
			binary.LittleEndian.PutUint32(header[34:38], uint32(rows*3072))
			if !bytes.Equal(raw[:54], header) || int64(len(raw)) != wantSize {
				t.Fatal("BMP header does not describe the complete pixel array")
			}
			if string(raw[54:62]) != "GPSTBMP!" || binary.LittleEndian.Uint32(raw[62:66]) != 2 || !bytes.Equal(raw[66:70], make([]byte, 4)) || binary.LittleEndian.Uint64(raw[70:78]) != uint64(len(tc.data)) || !bytes.Equal(raw[78:86], make([]byte, 8)) {
				t.Fatal("invalid pixel-area metadata")
			}
			payloadEnd := BMPPayloadOffset + len(tc.data)
			if !bytes.Equal(raw[payloadEnd:], make([]byte, len(raw)-payloadEnd)) {
				t.Fatal("last pixel row was not zero padded")
			}
			sum := sha1.Sum(raw)
			if c.Format != BMPFormat || c.PayloadOffset != BMPPayloadOffset || c.Size != int64(len(raw)) || c.PayloadSize != int64(len(tc.data)) || hex.EncodeToString(sum[:]) != c.SHA1 || !bytes.Equal(raw[c.PayloadOffset:payloadEnd], tc.data) {
				t.Fatal("carrier metadata, digest or payload changed")
			}
			f, err := os.Open(dst)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(io.NewSectionReader(f, info.PayloadOffset, info.PayloadSize))
			f.Close()
			if err != nil || !bytes.Equal(got, tc.data) {
				t.Fatal("payload roundtrip")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := p.PackPart(ctx, src, dst, part); !errors.Is(err, context.Canceled) {
				t.Fatal("packing ignored cancellation")
			}
			unchanged, err := os.ReadFile(dst)
			if err != nil || !bytes.Equal(unchanged, raw) {
				t.Fatal("canceled packing changed the published carrier")
			}
			if tc.name != "binary" {
				return
			}
			// Inspect checks only structure, not logical bytes or final padding.
			damaged := bytes.Clone(raw)
			damaged[86] ^= 1
			damaged[len(damaged)-1] ^= 1
			if err := os.WriteFile(dst, damaged, 0600); err != nil {
				t.Fatal(err)
			}
			if got, err := p.Inspect(dst); err != nil || got.PayloadSize != int64(len(tc.data)) {
				t.Fatalf("Inspect attempted payload/padding validation: %+v, %v", got, err)
			}
			// File/header dimensions, image extent, magic, version, reserved fields
			// and an overflowing logical length must all be rejected.
			for _, offset := range []int{2, 6, 10, 18, 22, 30, 34, 54, 62, 66, 77, 78} {
				damaged = bytes.Clone(raw)
				damaged[offset] ^= 0x80
				if err := os.WriteFile(dst, damaged, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := p.Inspect(dst); !errors.Is(err, core.ErrInvalid) {
					t.Fatalf("invalid carrier field accepted at byte %d: %v", offset, err)
				}
			}
			if err := os.WriteFile(dst, raw[:len(raw)-1], 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Inspect(dst); !errors.Is(err, core.ErrInvalid) {
				t.Fatal("truncated carrier accepted")
			}
		})
	}
}

func TestVideoRoundTrip(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source.anything"), filepath.Join(dir, "carrier")
	data := []byte{0, 255, 17, 128, 13, 10}
	source := append([]byte("before"), data...)
	source = append(source, []byte("after")...)
	if err := os.WriteFile(src, source, 0600); err != nil {
		t.Fatal(err)
	}
	// A forced small final MP4 part must retain MP4 and exclude both source sides.
	part := core.PartPlan{Index: 2, Offset: 6, Length: int64(len(data)), Kind: "mp4", Budget: minVideoPartSize}
	c, err := p.PackPart(context.Background(), src, dst, part)
	if err != nil {
		t.Fatal(err)
	}
	info, err := p.Inspect(dst)
	if err != nil || info.Format != VideoFormat || info.PayloadOffset != VideoPayloadOffset || info.PayloadSize != int64(len(data)) || info.Size != VideoPayloadOffset+int64(len(data)) || info.SHA1 != "" {
		t.Fatalf("video inspection: %+v, %v", info, err)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(raw)
	if c.Format != VideoFormat || c.Size != info.Size || c.PayloadOffset != VideoPayloadOffset || c.PayloadSize != int64(len(data)) || c.SHA1 != hex.EncodeToString(sum[:]) || !bytes.Equal(raw[:len(videoTemplate)], videoTemplate) || !bytes.Equal(raw[VideoPayloadOffset:], data) {
		t.Fatal("video carrier changed playable prefix, payload or digest")
	}
	metadata := raw[len(videoTemplate)+videoBoxHeaderSize : VideoPayloadOffset]
	if string(metadata[:8]) != videoMetadataMagic || binary.BigEndian.Uint32(metadata[8:12]) != 2 || !bytes.Equal(metadata[12:16], make([]byte, 4)) || binary.BigEndian.Uint64(metadata[16:24]) != uint64(len(data)) || !bytes.Equal(metadata[24:32], make([]byte, 8)) {
		t.Fatal("invalid MP4 versioned metadata")
	}
	// Inspect must not read/check logical payload bytes.
	damaged := bytes.Clone(raw)
	damaged[len(damaged)-1] ^= 1
	if err := os.WriteFile(dst, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Inspect(dst); err != nil {
		t.Fatal(err)
	}
	box := len(videoTemplate)
	for _, offset := range []int{0, len(videoTemplate) - 1, box, box + 4, box + 8, box + 16, box + 32, box + 40, box + 44, box + 48, box + 56} {
		damaged = bytes.Clone(raw)
		damaged[offset] ^= 0x80
		if err := os.WriteFile(dst, damaged, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Inspect(dst); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("invalid MP4 framing accepted at %d: %v", offset, err)
		}
	}
	for _, damaged := range [][]byte{raw[:len(raw)-1], append(bytes.Clone(raw), 0)} {
		if err := os.WriteFile(dst, damaged, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Inspect(dst); !errors.Is(err, core.ErrInvalid) {
			t.Fatal("incorrect MP4 extent accepted")
		}
	}
}

func TestFailedPackKeepsDestination(t *testing.T) {
	for _, cancelPack := range []bool{false, true} {
		dir := t.TempDir()
		src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "carrier")
		if err := os.WriteFile(src, []byte("payload"), 0600); err != nil {
			t.Fatal(err)
		}
		old := []byte("existing destination")
		if err := os.WriteFile(dst, old, 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		part := core.PartPlan{Length: 7, Kind: "mp4", Budget: minVideoPartSize}
		_, err := packFile(ctx, src, dst, part, func(part core.PartPlan) (carrierPlan, error) {
			if cancelPack {
				cancel()
			} else if err := os.Truncate(src, 0); err != nil {
				t.Fatal(err)
			}
			return planPart(part)
		})
		cancel()
		want := core.ErrConflict
		if cancelPack {
			want = context.Canceled
		}
		if !errors.Is(err, want) {
			t.Fatalf("failed pack: %v, want %v", err, want)
		}
		got, err := os.ReadFile(dst)
		if err != nil || !bytes.Equal(got, old) {
			t.Fatal("failed pack overwrote destination")
		}
		files, err := os.ReadDir(dir)
		if err != nil || len(files) != 2 {
			t.Fatal("failed pack left temporary carrier")
		}
	}
}

func TestPackPartValidation(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "carrier")
	if err := os.WriteFile(src, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, part := range []core.PartPlan{
		{Index: -1, Length: 1, Kind: "bmp", Budget: MaxBMPCarrierSize},
		{Offset: -1, Length: 1, Kind: "bmp", Budget: MaxBMPCarrierSize},
		{Length: -1, Kind: "bmp", Budget: MaxBMPCarrierSize},
		{Offset: 6, Length: 2, Kind: "bmp", Budget: MaxBMPCarrierSize},
		{Offset: 1<<63 - 1, Length: 1, Kind: "mp4", Budget: MaxVideoCarrierSize},
		{Length: 1, Kind: "unknown", Budget: MaxBMPCarrierSize},
		{Length: 1, Kind: "mp4", Budget: MaxBMPCarrierSize},
	} {
		if _, err := p.PackPart(context.Background(), src, dst, part); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("invalid part accepted: %+v, %v", part, err)
		}
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("invalid plan published a destination")
	}
	// A pre-existing deterministic staging path belongs to recovery or another
	// attempt; O_EXCL must not truncate or remove it, or the prior destination.
	old, staging := []byte("old destination"), []byte("owned elsewhere")
	if err := os.WriteFile(dst, old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".packing", staging, 0600); err != nil {
		t.Fatal(err)
	}
	part := core.PartPlan{Length: 7, Kind: "bmp", Budget: MaxBMPCarrierSize}
	if _, err := p.PackPart(context.Background(), src, dst, part); !errors.Is(err, os.ErrExist) {
		t.Fatalf("staging collision: %v", err)
	}
	for path, want := range map[string][]byte{dst: old, dst + ".packing": staging} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("staging collision changed an existing file")
		}
	}
}
