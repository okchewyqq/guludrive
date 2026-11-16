package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"guludrive/core"
	"guludrive/internal/database"
)

func TestProxyScriptValidation(t *testing.T) {
	for input, want := range map[string]string{
		"http://public.example.invalid:8080":  "http://public.example.invalid:8080",
		"http://public.example.invalid:8080/": "http://public.example.invalid:8080",
		"https://public.example.invalid:8443": "https://public.example.invalid:8443",
		"HTTP://PUBLIC.example.invalid:80":    "http://public.example.invalid",
		"https://8.8.8.8:8443":                "https://8.8.8.8:8443",
		"http://[2606:4700:4700::1111]:8080":  "http://[2606:4700:4700::1111]:8080",
	} {
		if got := publicManifestOrigin(input); got != want {
			t.Fatalf("public origin canonicalization: got %q, want %q", got, want)
		}
		if !validWorkerURL(input + "/proxy") {
			t.Fatal("public HTTP(S) Worker URL rejected")
		}
	}
	for _, origin := range []string{"", "http://localhost:8080", "https://a.localhost.", "https://10.0.0.1", "http://127.1", "http://0x7f000001", "http://[::1]", "http://[fd00::1]", "http://[::ffff:127.0.0.1]", "http://192.168.1.1", "https://host.local", "http://host..invalid", "http://bad_host.invalid", "http://user@public.example.invalid", "http://public.example.invalid/path", "http://public.example.invalid#", "http://public.example.invalid?", "http://public.example.invalid:0", "http://public.example.invalid:65536"} {
		if _, err := generateProxyScript(origin, "fake"); err == nil {
			t.Fatalf("unsafe origin accepted: %q", origin)
		}
	}
	for _, password := range []string{"", "bad\x00password", "bad\rpassword", "bad\npassword", string([]byte{0xff}), strings.Repeat("x", 4097)} {
		if _, err := generateProxyScript("http://public.example.invalid:8080", password); err == nil {
			t.Fatal("invalid password accepted")
		}
	}
	for _, password := range []string{"x", " 密 ", strings.Repeat("x", 4096)} {
		if !validProxyPassword(password) {
			t.Fatal("valid raw UTF-8 password rejected")
		}
	}
}

func TestProxyScriptStandalone(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the standalone Worker smoke test")
	}
	origin := "http://public.example.invalid:8080"
	password := `"; globalThis.injected = true; // 中文 </script> \` + "\u2028\u2029"
	script, err := generateProxyScript(origin, password)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "generated.mjs")
	if err = os.WriteFile(file, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, "--check", file).CombinedOutput(); err != nil {
		t.Fatalf("generated script syntax: %v: %s", err, output)
	}
	config, _ := json.Marshal(map[string]string{"origin": origin, "password": password, "file": file})
	runner := `import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { pathToFileURL } from "node:url";
const {origin, password, file} = ` + string(config) + `;
const {default: worker} = await import(pathToFileURL(file).href);
assert.equal(globalThis.injected, undefined);
Date.now = () => 1793491200000;
let calls = 0;
globalThis.fetch = async (target, options) => {
  calls++; const url = new URL(target);
  assert.equal(url.origin, origin); assert.equal(url.pathname, "/download-manifest");
  assert.equal(url.searchParams.get("meta"), "1");
  assert.equal(options.redirect, "manual");
  const body = JSON.stringify({v:2,name:"fake",type:"application/octet-stream",size:0,etag:'"fake"',modified:1793491199});
  const auth = createHmac("sha256", password).update(url.searchParams.get("p"), "utf8").update("\n").update(body).digest("base64url");
  return new Response(body, {headers:{"X-GuluDrive-Manifest-Auth":auth}});
};
const p = Buffer.from(JSON.stringify({v:2,mode:"worker",manifest:origin+"/download-manifest?p=RkFLRQ&auth="+"A".repeat(43),exp:1793492100})).toString("base64url");
const auth = createHmac("sha256", password).update(p, "utf8").digest("base64url");
const response = await worker.fetch(new Request("https://worker.example.invalid/?p="+p+"&auth="+auth,{method:"HEAD"}));
assert.equal(response.status, 200); assert.equal(calls, 1);
assert.equal(response.headers.get("Content-Length"), "0");
`
	cmd := exec.Command(node, "--input-type=module")
	cmd.Stdin = strings.NewReader(runner)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("standalone generated Worker: %v: %s", err, output)
	}
}

func TestProxyScriptDraftIsolationAndSettings(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	d, err := loadDownloads(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, downloads: d, security: newSecurity(Config{DataDir: dir}, administrator{}), gdirect: &smokeRouter{}}
	if d.settings.workerSecret != "" || d.settings.publicOrigin != "" {
		t.Fatal("new settings must not generate a proxy password or origin")
	}
	snapshot := func() map[string]string {
		t.Helper()
		rows, err := db.Query(`SELECT key,value FROM settings ORDER BY key`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		result := map[string]string{}
		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err != nil {
				t.Fatal(err)
			}
			result[key] = value
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := snapshot()
	request := httptest.NewRequest(http.MethodPost, "/api/proxy-script", strings.NewReader(`{"publicOrigin":"http://draft.example.invalid:8080","password":"draft-fake"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.proxyScript(response, request) // Authentication/CSRF is covered at the registered route boundary.
	var output struct {
		Script string `json:"script"`
	}
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(response.Body.Bytes(), &output) != nil || output.Script == "" {
		t.Fatal("draft script export failed")
	}
	if !reflect.DeepEqual(before, snapshot()) || s.security.publicOrigin() != "" || d.settings.workerSecret != "" {
		t.Fatal("script generation changed effective or persisted settings")
	}
	settings, _, err := s.commitDownloadSettings(ctx, "proxy", "http://worker.example.invalid:8081/", "x", "HTTP://PUBLIC.example.invalid:8080")
	if err != nil {
		t.Fatal(err)
	}
	view := s.downloadSettingsView(settings)
	if view.PublicOrigin != "http://public.example.invalid:8080" || !view.PublicOriginReady || !view.WorkerConfigured {
		t.Fatal("public HTTP custom-port settings not ready")
	}
	if _, _, err = s.commitDownloadSettings(ctx, "proxy", settings.workerURL, "", settings.publicOrigin); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadDownloads(ctx, db)
	if err != nil || reloaded.settings.workerSecret != "x" || reloaded.settings.publicOrigin != view.PublicOrigin || reloaded.settings.mode != "proxy" {
		t.Fatal("settings or preserved blank-input password did not persist")
	}
	before = snapshot()
	if _, _, err = s.commitDownloadSettings(ctx, "proxy", settings.workerURL, "", "http://127.0.0.1"); err == nil || !reflect.DeepEqual(before, snapshot()) || s.security.publicOrigin() != view.PublicOrigin {
		t.Fatal("invalid settings changed persisted or effective state")
	}
}

func TestProxyManifestResponseAuthentication(t *testing.T) {
	const password = " 假代理密码 "
	entry := core.Entry{ID: 7, Version: 3, Path: "/bucket/file.bin", Name: "file.bin", Size: 3, ModTime: time.Unix(1700000000, 0)}
	filesystem := &smokeFS{files: map[string]smokeFile{entry.Path: {entry: entry, data: []byte("abc")}}}
	s := &Server{fs: filesystem, downloads: &downloads{key: []byte("FAKE-scoped-download-signing-key"), settings: downloadSettings{workerSecret: password}}}
	query, err := downloadParameters(serverDownload{V: 1, Mode: "manifest", Path: entry.Path, ID: entry.ID, Version: entry.Version, Exp: time.Now().Add(time.Minute).Unix()}, s.downloads.key)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, "meta", http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			values := query
			if method == "meta" {
				values = query.Clone()
				values.Set("meta", "1")
			}
			verb := http.MethodGet
			if method == http.MethodHead {
				verb = http.MethodHead
			}
			response := httptest.NewRecorder()
			s.downloadManifest(response, httptest.NewRequest(verb, "/download-manifest?"+values.Encode(), nil))
			if response.Code != http.StatusOK {
				t.Fatalf("manifest response status %d", response.Code)
			}
			body := response.Body.Bytes()
			if method == http.MethodHead {
				if len(body) != 0 {
					t.Fatal("HEAD emitted metadata bytes")
				}
				body, err = encodeManifest(entry, nil, true)
				if err != nil {
					t.Fatal(err)
				}
			}
			mac := hmac.New(sha256.New, []byte(password))
			_, _ = mac.Write([]byte(query.Get("p") + "\n"))
			_, _ = mac.Write(body)
			signature, err := base64.RawURLEncoding.Strict().DecodeString(response.Header().Get("X-GuluDrive-Manifest-Auth"))
			if err != nil || !hmac.Equal(signature, mac.Sum(nil)) || !json.Valid(body) {
				t.Fatal("manifest MAC did not bind exact p, newline and raw JSON bytes")
			}
		})
	}
	prepared := filesystem.prepares.Load()
	for _, password := range []string{"", "bad\npassword", "bad\x00password", string([]byte{0xff})} {
		s.downloads.mu.Lock()
		s.downloads.settings.workerSecret = password
		s.downloads.mu.Unlock()
		response := httptest.NewRecorder()
		s.downloadManifest(response, httptest.NewRequest(http.MethodGet, "/download-manifest?"+query.Encode(), nil))
		if response.Code != http.StatusBadGateway || response.Header().Get("X-GuluDrive-Manifest-Auth") != "" || strings.Contains(response.Body.String(), "googleusercontent.com") || filesystem.prepares.Load() != prepared {
			t.Fatal("invalid proxy password did not reject manifest delivery before preparation")
		}
	}
	if filesystem.opens.Load() != 0 {
		t.Fatal("metadata signing opened carrier content")
	}
}
