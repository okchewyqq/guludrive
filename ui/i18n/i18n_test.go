package i18n_test

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"testing"

	"guludrive/ui/i18n"
	"guludrive/ui/preferences"
)

func TestCatalogParity(t *testing.T) {
	catalog := func(language string) map[string]string {
		t.Helper()
		raw, err := os.ReadFile("locales/" + language + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]string
		if err := json.Unmarshal(raw, &values); err != nil {
			t.Fatal(err)
		}
		return values
	}
	en, zh := catalog("en"), catalog("zh")
	if len(en) != len(zh) || len(en) == 0 {
		t.Fatal("catalog key counts differ")
	}
	formats := regexp.MustCompile(`%[+#.0-9-]*[sdqv]`)
	for key, value := range en {
		if value == "" || zh[key] == "" || !slices.Equal(formats.FindAllString(value, -1), formats.FindAllString(zh[key], -1)) {
			t.Errorf("translation mismatch: %q", key)
		}
	}
	if i18n.T(i18n.Parse(""), "Files") != "文件" || i18n.T(i18n.English, "Files") != "Files" {
		t.Fatal("default Chinese or English lookup failed")
	}
}

func TestLanguagePreferencesRejectSecrets(t *testing.T) {
	for _, language := range []i18n.Locale{i18n.System, i18n.Chinese, i18n.English} {
		choices := preferences.Choices{Language: language, Theme: preferences.Dark}
		raw, err := preferences.Encode(choices)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]string
		_ = json.Unmarshal(raw, &fields)
		if len(fields) != 2 || fields["language"] != string(language) || fields["theme"] != "dark" {
			t.Fatalf("unsafe preference schema: %s", raw)
		}
		if saved, err := preferences.Decode(raw); err != nil || saved != choices {
			t.Fatal("language round-trip failed", err)
		}
	}
	for _, raw := range []string{`{"language":"en","theme":"light","script":"sensitive"}`, `{"language":"system","theme":"invalid"}`, `{"language":"en","theme":"light","publicOrigin":"http://public.example"}`, `{"language":"en","password":"synthetic"}`, `{"language":"en","currentPassword":"synthetic"}`, `{"language":"en","newPassword":"synthetic"}`, `{"language":"en","confirmPassword":"synthetic"}`, `{"language":"en","username":"synthetic"}`, `{"language":"zh","token":"synthetic"}`, `{"language":"en","cookie":"synthetic"}`, `{"language":"en","workerSecret":"synthetic"}`, `{"language":"en","workerURL":"https://worker.example"}`, `{"language":"en","ip":"203.0.113.20"}`, `{"language":"en","mode":"gdirect"}`, `{"language":"en","largeFileFormat":"mp4"}`, `{"language":"en","mp4PartSize":1000000000}`, `{"language":"en","bmpPartSize":180000000}`, `{"language":"invalid"}`} {
		if _, err := preferences.Decode([]byte(raw)); err == nil {
			t.Fatal("unsafe preferences accepted")
		}
	}
	store := preferences.FileStore{Directory: t.TempDir()}
	if err := store.Save(preferences.Choices{Language: i18n.English, Theme: preferences.Light}); err != nil {
		t.Fatal(err)
	}
	if language, err := store.Load(); err != nil || language.Language != i18n.English || language.Theme != preferences.Light {
		t.Fatal("saved language not restored", err)
	}
}
