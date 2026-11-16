// Package fonts embeds unmodified official MiSans faces for native clients.
// See NOTICE.txt and the accompanying license PDF for attribution and terms.
package fonts

import _ "embed"

// MiSans contains MiSans Regular (the font's original weight class is 330).
//
//go:embed MiSans-Regular.ttf
var MiSans []byte

// MiSansMedium contains MiSans Medium (the font's original weight class is 380).
//
//go:embed MiSans-Medium.ttf
var MiSansMedium []byte

// MiSansBold contains MiSans Bold (the font's original weight class is 630).
//
//go:embed MiSans-Bold.ttf
var MiSansBold []byte
