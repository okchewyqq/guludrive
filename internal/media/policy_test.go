package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"guludrive/core"
)

func TestCarrierPlans(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if VideoFormat != "guludrive-mp4-v2" || MaxVideoCarrierSize != 9000000000 || MaxCarrierSize != MaxVideoCarrierSize || VideoPayloadOffset != int64(len(videoTemplate)+64) || MaxPayloadSize != MaxVideoCarrierSize-VideoPayloadOffset {
		t.Fatal("current video carrier limits changed")
	}
	minBMPCapacity, err := partCapacity("bmp", minBMPPartSize)
	if err != nil {
		t.Fatal(err)
	}
	minVideoCapacity, err := partCapacity("mp4", minVideoPartSize)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		size   int64
		policy core.UploadPolicy
		kind   string
		budget int64
		count  int
	}{
		{0, core.UploadPolicy{Format: "mp4", PartSize: minVideoPartSize}, "bmp", MaxBMPCarrierSize, 1},
		{0, core.UploadPolicy{Format: "bmp", PartSize: minBMPPartSize}, "bmp", minBMPPartSize, 1},
		{minBMPCapacity, core.UploadPolicy{Format: "bmp", PartSize: minBMPPartSize}, "bmp", minBMPPartSize, 1},
		{minBMPCapacity + 1, core.UploadPolicy{Format: "bmp", PartSize: minBMPPartSize}, "bmp", minBMPPartSize, 2},
		{MaxBMPPayloadSize, core.UploadPolicy{Format: "bmp", PartSize: minBMPPartSize}, "bmp", minBMPPartSize, 2},
		{MaxBMPPayloadSize, core.UploadPolicy{Format: "bmp", PartSize: MaxBMPCarrierSize}, "bmp", MaxBMPCarrierSize, 1},
		{MaxBMPPayloadSize, core.UploadPolicy{Format: "mp4", PartSize: minVideoPartSize}, "bmp", MaxBMPCarrierSize, 1},
		{MaxBMPPayloadSize + 1, core.UploadPolicy{Format: "mp4", PartSize: minVideoPartSize}, "mp4", minVideoPartSize, 1},
		{minVideoCapacity, core.UploadPolicy{Format: "mp4", PartSize: minVideoPartSize}, "mp4", minVideoPartSize, 1},
		{minVideoCapacity + 1, core.UploadPolicy{Format: "mp4", PartSize: minVideoPartSize}, "mp4", minVideoPartSize, 2},
		{1 << 32, core.UploadPolicy{Format: "mp4", PartSize: minVideoPartSize}, "mp4", minVideoPartSize, 5},
		{MaxPayloadSize, core.UploadPolicy{Format: "mp4", PartSize: MaxVideoCarrierSize}, "mp4", MaxVideoCarrierSize, 1},
		{MaxPayloadSize + 1, core.UploadPolicy{Format: "mp4", PartSize: MaxVideoCarrierSize}, "mp4", MaxVideoCarrierSize, 2},
		{2*MaxPayloadSize + 17, core.UploadPolicy{Format: "mp4", PartSize: MaxVideoCarrierSize}, "mp4", MaxVideoCarrierSize, 3},
		{MaxBMPPayloadSize + 1, core.UploadPolicy{Format: "bmp", PartSize: MaxBMPCarrierSize}, "bmp", MaxBMPCarrierSize, 2},
	} {
		parts, err := p.Plan(tc.size, tc.policy)
		if err != nil || len(parts) != tc.count {
			t.Fatalf("plan %d/%+v: %v, %v", tc.size, tc.policy, parts, err)
		}
		var offset int64
		for index, part := range parts {
			if part.Index != index || part.Offset != offset || part.Length < 0 || part.Length > tc.size-offset || part.Kind != tc.kind || part.Budget != tc.budget {
				t.Fatalf("invalid source coverage or policy: %+v", part)
			}
			plan, err := planPart(part)
			if err != nil || plan.payloadSize != part.Length || plan.size > part.Budget {
				t.Fatalf("part framing: %+v, %v", plan, err)
			}
			if part.Kind == "mp4" {
				box := plan.prefix[len(videoTemplate):]
				if binary.BigEndian.Uint32(box[:4]) != 1 || string(box[4:8]) != "uuid" || binary.BigEndian.Uint64(box[8:16]) != uint64(part.Length)+64 || !bytes.Equal(box[16:32], videoUserType[:]) {
					t.Fatal("MP4 UUID does not use a complete extended-size box")
				}
			}
			offset += part.Length
		}
		if offset != tc.size {
			t.Fatal("parts did not cover the complete logical source")
		}
		if tc.size == MaxPayloadSize {
			plan, _ := planPart(parts[0])
			if plan.size != MaxVideoCarrierSize {
				t.Fatal("exact 9 GB carrier boundary changed")
			}
		}
	}
	for _, policy := range []core.UploadPolicy{{Format: "unknown", PartSize: MaxVideoCarrierSize}, {Format: "mp4", PartSize: MaxBMPCarrierSize}, {Format: "bmp", PartSize: minVideoPartSize}} {
		if _, err := p.Plan(0, policy); !errors.Is(err, core.ErrInvalid) {
			t.Fatal("invalid policy accepted for an empty source")
		}
	}
	if _, err := p.Plan(-1, core.UploadPolicy{Format: "mp4", PartSize: MaxVideoCarrierSize}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("negative logical source accepted")
	}
	if _, err := partCount(1<<63-1, 1); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("native slice byte-size overflow accepted")
	}
	if _, err := planPart(core.PartPlan{Offset: 1<<63 - 1, Length: 1, Kind: "mp4", Budget: minVideoPartSize}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("source range sum overflow accepted")
	}
}

func TestUploadSettings(t *testing.T) {
	settings := DefaultUploadSettings()
	policy, err := PolicyFor(settings)
	if err != nil || settings != (core.UploadSettings{LargeFileFormat: "bmp", MP4PartSize: 6_000_000_000, BMPPartSize: 150_000_000}) || policy != (core.UploadPolicy{Format: "bmp", PartSize: 150_000_000}) {
		t.Fatal("invalid defaults")
	}
	settings.LargeFileFormat = "mp4"
	settings.MP4PartSize, settings.BMPPartSize = minVideoPartSize, minBMPPartSize
	mp4, err := PolicyFor(settings)
	if err != nil || mp4.Format != "mp4" || mp4.PartSize != minVideoPartSize || policy != (core.UploadPolicy{Format: "bmp", PartSize: 150_000_000}) {
		t.Fatal("policy did not snapshot selected settings")
	}
	for _, invalid := range []core.UploadSettings{
		{LargeFileFormat: "invalid", MP4PartSize: MaxVideoCarrierSize, BMPPartSize: MaxBMPCarrierSize},
		{LargeFileFormat: "bmp", MP4PartSize: minVideoPartSize - 1, BMPPartSize: MaxBMPCarrierSize},
		{LargeFileFormat: "bmp", MP4PartSize: MaxVideoCarrierSize + 1, BMPPartSize: MaxBMPCarrierSize},
		{LargeFileFormat: "mp4", MP4PartSize: MaxVideoCarrierSize, BMPPartSize: minBMPPartSize - 1},
		{LargeFileFormat: "mp4", MP4PartSize: MaxVideoCarrierSize, BMPPartSize: MaxBMPCarrierSize + 1},
		{LargeFileFormat: "bmp", MP4PartSize: 10_000_000_000, BMPPartSize: MaxBMPCarrierSize},
		{LargeFileFormat: "bmp", MP4PartSize: MaxVideoCarrierSize, BMPPartSize: 180_000_000},
	} {
		if err := ValidateUploadSettings(invalid); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("invalid stored settings accepted: %+v", invalid)
		}
		if _, err := PolicyFor(invalid); !errors.Is(err, core.ErrInvalid) {
			t.Fatal("invalid inactive budget survived snapshot validation")
		}
	}
}
