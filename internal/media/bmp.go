package media

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"guludrive/core"
)

const (
	bmpHeaderSize = 54
	metadataSize  = 32
	imageWidth    = 1024
	rowStride     = imageWidth * 3 // BGR24 rows are already four-byte aligned.
	metadataMagic = "GPSTBMP!"

	// BMPPayloadOffset follows the header and metadata within the pixel array.
	BMPPayloadOffset = bmpHeaderSize + metadataSize
	// Reserve whole pixel rows, including metadata and final zero padding.
	MaxBMPPayloadSize = (MaxBMPCarrierSize-bmpHeaderSize)/rowStride*rowStride - metadataSize
)

type imagePlan struct {
	payloadSize int64
	height      int64
	pixelSize   int64
	size        int64
	padding     int64
}

func planImage(payloadSize int64) (imagePlan, error) {
	// Bound the input before addition, multiplication or narrowing conversions.
	if payloadSize < 0 || payloadSize > MaxBMPPayloadSize {
		return imagePlan{}, fmt.Errorf("BMP payload size must be between 0 and %d bytes: %w", MaxBMPPayloadSize, core.ErrInvalid)
	}
	height := (metadataSize + payloadSize + rowStride - 1) / rowStride
	pixelSize := height * rowStride
	return imagePlan{payloadSize: payloadSize, height: height, pixelSize: pixelSize,
		size: bmpHeaderSize + pixelSize, padding: pixelSize - metadataSize - payloadSize}, nil
}

// bmpPrefix contains the unchanged bottom-up BI_RGB BGR24 BMP v2 header and
// first 32 pixel bytes: magic, LE32 version, four reserved zeros, LE64 logical
// size and eight reserved zeros. Payload and final padding remain image pixels.
func bmpPrefix(plan imagePlan) []byte {
	raw := make([]byte, BMPPayloadOffset)
	copy(raw, "BM")
	binary.LittleEndian.PutUint32(raw[2:6], uint32(plan.size))
	binary.LittleEndian.PutUint32(raw[10:14], bmpHeaderSize)
	binary.LittleEndian.PutUint32(raw[14:18], 40)
	binary.LittleEndian.PutUint32(raw[18:22], imageWidth)
	binary.LittleEndian.PutUint32(raw[22:26], uint32(plan.height))
	binary.LittleEndian.PutUint16(raw[26:28], 1)
	binary.LittleEndian.PutUint16(raw[28:30], 24)
	binary.LittleEndian.PutUint32(raw[34:38], uint32(plan.pixelSize))
	copy(raw[bmpHeaderSize:bmpHeaderSize+8], metadataMagic)
	binary.LittleEndian.PutUint32(raw[bmpHeaderSize+8:bmpHeaderSize+12], 2)
	binary.LittleEndian.PutUint64(raw[bmpHeaderSize+16:bmpHeaderSize+24], uint64(plan.payloadSize))
	return raw
}

func inspectImage(header []byte, size int64) (carrierPlan, error) {
	if len(header) != BMPPayloadOffset || size > MaxBMPCarrierSize {
		return carrierPlan{}, invalid()
	}
	payloadSize := binary.LittleEndian.Uint64(header[bmpHeaderSize+16 : bmpHeaderSize+24])
	if payloadSize > uint64(MaxBMPPayloadSize) {
		return carrierPlan{}, invalid()
	}
	plan, err := planImage(int64(payloadSize))
	if err != nil || plan.size != size || !bytes.Equal(header, bmpPrefix(plan)) {
		return carrierPlan{}, invalid()
	}
	return carrierPlan{format: BMPFormat, size: size, payloadOffset: BMPPayloadOffset, payloadSize: plan.payloadSize}, nil
}
