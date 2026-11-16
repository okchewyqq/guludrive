package media

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"fmt"

	"guludrive/core"
)

// template.mp4 is project-generated, not an upstream binary. It is a plain
// two-second 320x320 H.264/yuv420p video without audio. Regenerate offline with:
// ffmpeg -f lavfi -i color=c=0x4d667a:s=320x320:r=12:d=2 -map_metadata -1 -an
//
//	-c:v libx264 -pix_fmt yuv420p -preset veryslow -crf 35 -movflags +faststart
//	-fflags +bitexact -flags:v +bitexact template.mp4
//
// FFmpeg is not a runtime or Go build dependency.
//
//go:embed template.mp4
var videoTemplate []byte

const (
	videoBoxHeaderSize = 32 // size32, type, largesize64, 16-byte user type.
	videoMetadataSize  = 32
	videoMetadataMagic = "GPSTMP4!"
)

// Private UUID 49d8a7d8-6154-4a56-9a25-0f0cb735779a identifies this framing.
var videoUserType = [16]byte{0x49, 0xd8, 0xa7, 0xd8, 0x61, 0x54, 0x4a, 0x56, 0x9a, 0x25, 0x0f, 0x0c, 0xb7, 0x35, 0x77, 0x9a}

// VideoPayloadOffset follows the unchanged playable prefix, UUID header and
// versioned metadata; the final UUID box contains the untransformed payload.
var VideoPayloadOffset int64 = int64(len(videoTemplate)) + videoBoxHeaderSize + videoMetadataSize

func validateVideoTemplate() error {
	// Validate only this small generated prefix's top-level extents, not MP4
	// tracks, codecs or a general-purpose atom tree. No payload scan is needed.
	if len(videoTemplate) < 32 || len(videoTemplate) > 64*1024 || string(videoTemplate[4:8]) != "ftyp" {
		return invalid()
	}
	seenMoov, seenMedia := false, false
	for offset := 0; offset < len(videoTemplate); {
		if len(videoTemplate)-offset < 8 {
			return invalid()
		}
		size := uint64(binary.BigEndian.Uint32(videoTemplate[offset : offset+4]))
		if size < 8 || size > uint64(len(videoTemplate)-offset) {
			return invalid()
		}
		switch string(videoTemplate[offset+4 : offset+8]) {
		case "ftyp":
			if offset != 0 || size < 16 || (size-16)%4 != 0 || string(videoTemplate[offset+8:offset+12]) != "isom" {
				return invalid()
			}
		case "moov":
			if seenMoov {
				return invalid()
			}
			seenMoov = true
		case "mdat":
			if seenMedia {
				return invalid()
			}
			seenMedia = true
		case "free":
		default:
			return invalid()
		}
		offset += int(size)
	}
	if !seenMoov || !seenMedia {
		return invalid()
	}
	return nil
}

func planVideo(payloadSize int64) (carrierPlan, error) {
	// Bound before arithmetic: the complete carrier, not just the payload,
	// must fit the decimal 9 GB limit, including the full 64-bit box header.
	if payloadSize < 0 || payloadSize > MaxPayloadSize {
		return carrierPlan{}, fmt.Errorf("video payload size must be between 0 and %d bytes: %w", MaxPayloadSize, core.ErrInvalid)
	}
	raw := make([]byte, int(VideoPayloadOffset))
	copy(raw, videoTemplate)
	box := raw[len(videoTemplate):]
	binary.BigEndian.PutUint32(box[:4], 1) // Always extended size, even below 4 GiB.
	copy(box[4:8], "uuid")
	binary.BigEndian.PutUint64(box[8:16], uint64(videoBoxHeaderSize+videoMetadataSize)+uint64(payloadSize))
	copy(box[16:32], videoUserType[:])
	metadata := box[videoBoxHeaderSize:]
	copy(metadata[:8], videoMetadataMagic)
	binary.BigEndian.PutUint32(metadata[8:12], 2)
	// Four reserved zeros, BE64 logical size, then eight reserved zeros.
	binary.BigEndian.PutUint64(metadata[16:24], uint64(payloadSize))
	return carrierPlan{format: VideoFormat, prefix: raw, size: VideoPayloadOffset + payloadSize,
		payloadOffset: VideoPayloadOffset, payloadSize: payloadSize}, nil
}

func inspectVideo(header []byte, size int64) (carrierPlan, error) {
	if int64(len(header)) != VideoPayloadOffset || size > MaxVideoCarrierSize {
		return carrierPlan{}, invalid()
	}
	offset := len(videoTemplate) + videoBoxHeaderSize
	payloadSize := binary.BigEndian.Uint64(header[offset+16 : offset+24])
	if payloadSize > uint64(MaxPayloadSize) {
		return carrierPlan{}, invalid()
	}
	plan, err := planVideo(int64(payloadSize))
	if err != nil || plan.size != size || !bytes.Equal(header, plan.prefix) {
		return carrierPlan{}, invalid()
	}
	return plan, nil
}
