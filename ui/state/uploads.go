package state

import (
	"math"
	"strconv"
	"strings"

	"guludrive/ui/api"
	"guludrive/ui/i18n"
)

// ParseUploadSettings converts decimal display units only. The server remains
// authoritative about packing, chunk triggers, staging and atomic publication.
func ParseUploadSettings(format, mp4GB, bmpMB string) (api.UploadSettings, error) {
	if format != "mp4" && format != "bmp" {
		return api.UploadSettings{}, i18n.Err("Choose MP4 or BMP for large files")
	}
	parse := func(raw string, unit, min, max float64) (int64, bool) {
		value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < min || value > max {
			return 0, false
		}
		// Bounds precede multiplication/conversion, so even malicious exponents
		// cannot overflow int64. Sub-byte fractions round to the nearest byte.
		return int64(math.Round(value * unit)), true
	}
	mp4, ok := parse(mp4GB, 1_000_000_000, 1, 9)
	if !ok {
		return api.UploadSettings{}, i18n.Err("Enter an MP4 part size from 1 to 9 GB")
	}
	bmp, ok := parse(bmpMB, 1_000_000, 100, 150)
	if !ok {
		return api.UploadSettings{}, i18n.Err("Enter a BMP part size from 100 to 150 MB")
	}
	return api.UploadSettings{LargeFileFormat: format, MP4PartSize: mp4, BMPPartSize: bmp}, nil
}
