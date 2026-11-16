package webui

import (
	"bytes"
	"io/fs"
	"testing"
)

func TestAssets(t *testing.T) {
	assets := Assets()
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(index, []byte(`<script type="module"`)) {
		t.Fatal("embedded index has no built module script")
	}
	for _, pattern := range []string{
		"assets/*.js", "assets/*.css",
		"assets/MiSans-Regular-*.woff2",
		"assets/MiSans-Medium-*.woff2",
		"assets/MiSans-Bold-*.woff2",
		"legal/NOTICE.txt", "legal/*.pdf",
	} {
		matches, err := fs.Glob(assets, pattern)
		if err != nil || len(matches) == 0 {
			t.Fatalf("missing embedded asset %q: %v", pattern, err)
		}
		for _, name := range matches {
			info, err := fs.Stat(assets, name)
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				t.Fatalf("embedded asset %q is not a nonempty file", name)
			}
		}
	}
}
