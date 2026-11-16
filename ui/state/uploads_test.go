package state

import "testing"

func TestUploadSettingsDisplayUnits(t *testing.T) {
	for _, input := range []struct {
		format, mp4, bmp   string
		mp4Bytes, bmpBytes int64
	}{
		{"mp4", "9", "150", 9_000_000_000, 150_000_000},
		{"bmp", " 1.25 ", "149.5", 1_250_000_000, 149_500_000},
		{"mp4", "1", "100", 1_000_000_000, 100_000_000},
	} {
		got, err := ParseUploadSettings(input.format, input.mp4, input.bmp)
		if err != nil || got.LargeFileFormat != input.format || got.MP4PartSize != input.mp4Bytes || got.BMPPartSize != input.bmpBytes {
			t.Fatal("decimal units conversion failed", err)
		}
	}
	for _, input := range [][3]string{
		{"auto", "9", "150"}, {"mp4", "NaN", "150"}, {"bmp", "+Inf", "150"}, {"mp4", "1e999", "150"},
		{"mp4", "0.99", "150"}, {"mp4", "9.001", "150"}, {"bmp", "1", "99"}, {"bmp", "1", "151"}, {"mp4", "1", "NaN"},
	} {
		if _, err := ParseUploadSettings(input[0], input[1], input[2]); err == nil {
			t.Fatal("non-finite/out-of-range part size accepted")
		}
	}
}
