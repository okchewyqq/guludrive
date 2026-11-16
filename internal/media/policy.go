package media

import (
	"fmt"
	"unsafe"

	"guludrive/core"
)

const (
	minVideoPartSize int64 = 1_000_000_000
	minBMPPartSize   int64 = 100_000_000
)

// DefaultUploadSettings prefers BMP, including for all small files.
// Part budgets include framing and are the actual splitting thresholds.
func DefaultUploadSettings() core.UploadSettings {
	return core.UploadSettings{LargeFileFormat: "bmp", MP4PartSize: 6_000_000_000, BMPPartSize: 150_000_000}
}

// ValidateUploadSettings validates both stored budgets, including the currently
// inactive kind, so switching kinds cannot activate invalid persisted settings.
func ValidateUploadSettings(settings core.UploadSettings) error {
	if settings.LargeFileFormat != "mp4" && settings.LargeFileFormat != "bmp" {
		return fmt.Errorf("unknown large-file carrier kind: %w", core.ErrInvalid)
	}
	if _, err := partCapacity("mp4", settings.MP4PartSize); err != nil {
		return err
	}
	_, err := partCapacity("bmp", settings.BMPPartSize)
	return err
}

// PolicyFor snapshots only the selected carrier kind and complete-part budget.
func PolicyFor(settings core.UploadSettings) (core.UploadPolicy, error) {
	if err := ValidateUploadSettings(settings); err != nil {
		return core.UploadPolicy{}, err
	}
	budget := settings.MP4PartSize
	if settings.LargeFileFormat == "bmp" {
		budget = settings.BMPPartSize
	}
	return core.UploadPolicy{Format: settings.LargeFileFormat, PartSize: budget}, nil
}

func partCapacity(kind string, budget int64) (int64, error) {
	switch kind {
	case "bmp":
		if budget < minBMPPartSize || budget > MaxBMPCarrierSize {
			break
		}
		return (budget-bmpHeaderSize)/rowStride*rowStride - metadataSize, nil
	case "mp4":
		if budget < minVideoPartSize || budget > MaxVideoCarrierSize {
			break
		}
		return budget - VideoPayloadOffset, nil
	}
	return 0, fmt.Errorf("invalid carrier kind or complete-part budget: %w", core.ErrInvalid)
}

// partCount avoids a ceil-division addition overflow and checks both native-int
// conversion and the byte size of the resulting slice before allocation.
func partCount(size, capacity int64) (int, error) {
	if size < 0 || capacity <= 0 {
		return 0, core.ErrInvalid
	}
	count := size / capacity
	if size%capacity != 0 {
		count++
	}
	if count == 0 {
		count = 1
	}
	maxInt := uint64(^uint(0) >> 1)
	if uint64(count) > maxInt/uint64(unsafe.Sizeof(core.PartPlan{})) {
		return 0, fmt.Errorf("part plan cannot fit a native slice: %w", core.ErrInvalid)
	}
	return int(count), nil
}

// Plan has no total logical-file policy cap. BMP policy always uses its selected
// budget. MP4 policy retains one hard-limit BMP for a small source that fits;
// otherwise it uses MP4 at the selected budget. Sources split as soon as they
// exceed that budget's payload capacity, without waiting for the hard cap.
func (p *Packer) Plan(size int64, policy core.UploadPolicy) ([]core.PartPlan, error) {
	capacity, err := partCapacity(policy.Format, policy.PartSize)
	if err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, core.ErrInvalid
	}
	kind, budget := policy.Format, policy.PartSize
	if kind == "mp4" && size <= MaxBMPPayloadSize {
		kind, budget, capacity = "bmp", MaxBMPCarrierSize, MaxBMPPayloadSize
	}
	count, err := partCount(size, capacity)
	if err != nil {
		return nil, err
	}
	parts := make([]core.PartPlan, count)
	remaining := size
	for index := range parts {
		length := min(remaining, capacity)
		part := core.PartPlan{Index: index, Offset: size - remaining, Length: length, Kind: kind, Budget: budget}
		if _, err := planPart(part); err != nil {
			return nil, err
		}
		parts[index] = part
		remaining -= length
	}
	return parts, nil
}

func planPart(part core.PartPlan) (carrierPlan, error) {
	capacity, err := partCapacity(part.Kind, part.Budget)
	if err != nil {
		return carrierPlan{}, err
	}
	if part.Index < 0 || part.Offset < 0 || part.Length < 0 || part.Length > capacity || part.Length > int64(^uint64(0)>>1)-part.Offset {
		return carrierPlan{}, core.ErrInvalid
	}
	var plan carrierPlan
	if part.Kind == "bmp" {
		image, err := planImage(part.Length)
		if err != nil {
			return carrierPlan{}, err
		}
		plan = carrierPlan{format: BMPFormat, prefix: bmpPrefix(image), size: image.size,
			payloadOffset: BMPPayloadOffset, payloadSize: part.Length, padding: image.padding}
	} else {
		plan, err = planVideo(part.Length)
		if err != nil {
			return carrierPlan{}, err
		}
	}
	if plan.size > part.Budget {
		return carrierPlan{}, core.ErrInvalid
	}
	return plan, nil
}
