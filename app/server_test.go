package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-webdav"
	"golang.org/x/crypto/bcrypt"

	"guludrive/core"
	"guludrive/internal/database"
	"guludrive/internal/media"
	"guludrive/internal/photos"
	"guludrive/internal/webui"
)

func TestSessionAndBasicDAV(t *testing.T) {
	filesystem := &smokeFS{files: map[string]smokeFile{
		"/":       {entry: core.Entry{ID: 1, Path: "/", IsDir: true}},
		"/bucket": {entry: core.Entry{ID: 2, Path: "/bucket", Name: "bucket", IsDir: true}},
	}, nextID: 3}
	cfg := Config{DataDir: t.TempDir(), MaxFileSize: 64}
	s, err := newTestServer(t, cfg, filesystem, smokeAccounts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		server.Close()
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	do := func(method, target string, body []byte, status int, headers map[string]string, basic bool) (http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+target, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		if basic {
			req.SetBasicAuth("admin", "test-password")
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal("request failed")
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != status {
			t.Fatalf("%s %s: status %d, want %d; read error %v", method, target, res.StatusCode, status, err)
		}
		return res.Header, data
	}
	_, body := do("GET", "/", nil, 200, nil, false)
	if !strings.Contains(string(body), "GuluDrive") {
		t.Fatal("root must serve the embedded application, not DAV data")
	}
	jsonHeaders := map[string]string{"Content-Type": "application/json", "X-GuluDrive-Request": "1"}
	login := []byte(`{"username":"admin","password":"test-password"}`)
	do("GET", "/api/status", nil, 401, nil, false)
	do("POST", "/api/session", login, 403, map[string]string{"Content-Type": "application/json"}, false)
	do("POST", "/api/session", login, 403, map[string]string{"Content-Type": "application/json", "X-GuluDrive-Request": "1", "Origin": "https://outside.invalid"}, false)
	headers, body := do("POST", "/api/session", login, 200, jsonHeaders, false)
	if strings.TrimSpace(string(body)) != `{"ok":true}` || !strings.Contains(headers.Get("Set-Cookie"), "HttpOnly") || !strings.Contains(headers.Get("Set-Cookie"), "SameSite=Strict") {
		t.Fatal("login must return only success and a protected cookie")
	}
	oldCookie := (&http.Response{Header: headers}).Cookies()[0]
	headers, _ = do("POST", "/api/session", login, 200, jsonHeaders, false)
	if (&http.Response{Header: headers}).Cookies()[0].Value == oldCookie.Value {
		t.Fatal("login did not renew session token")
	}
	for _, header := range []string{"Cookie", "Authorization"} {
		req, _ := http.NewRequest("GET", server.URL+"/api/status", nil)
		value := oldCookie.String()
		if header == "Authorization" {
			value = "Bearer " + (&http.Response{Header: headers}).Cookies()[0].Value
		}
		req.Header.Set(header, value)
		res, err := http.DefaultClient.Do(req) // No cookie jar: old cookies/bearers must fail.
		if err != nil {
			t.Fatal("authorization check failed")
		}
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatal("old session or bearer unexpectedly authorized")
		}
	}
	_, body = do("GET", "/api/status", nil, 200, nil, false)
	if !strings.Contains(string(body), `"googleReady":false`) || strings.Contains(string(body), `"stats"`) {
		t.Fatal("status must use the reduced shared model")
	}
	secure := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "https://localhost/api/session", bytes.NewReader(login))
	for key, value := range jsonHeaders {
		req.Header.Set(key, value)
	}
	s.Handler().ServeHTTP(secure, req)
	if secure.Code != 200 || !secure.Result().Cookies()[0].Secure {
		t.Fatal("direct TLS session cookie must be Secure")
	}
	do("GET", "/dav/bucket/file.bin", nil, 401, nil, false)
	payload := []byte{0, 1, 2, 3, 4, 255, '<', 'h', 't', 'm', 'l', '>'}
	headers, _ = do("PUT", "/dav/bucket/file.bin", payload, 201, nil, true)
	etag := headers.Get("ETag")
	for _, target := range []string{"/dav/bucket/file.bin", "/api/file?path=%2Fbucket%2Ffile.bin"} {
		headers, body = do("HEAD", target, nil, 200, nil, true)
		if headers.Get("Content-Length") != "12" || headers.Get("ETag") != etag || headers.Get("Content-Type") == "" || headers.Get("Last-Modified") == "" || len(body) != 0 || filesystem.opens.Load() != 0 {
			t.Fatal("HEAD must return metadata without opening the remote payload")
		}
		do("HEAD", target, nil, 304, map[string]string{"If-None-Match": etag}, true)
		do("HEAD", target, nil, 304, map[string]string{"If-Modified-Since": headers.Get("Last-Modified")}, true)
		do("HEAD", target, nil, 412, map[string]string{"If-Match": `"stale"`}, true)
	}
	if filesystem.opens.Load() != 0 {
		t.Fatal("conditional HEAD opened the remote payload")
	}
	headers, body = do("GET", "/dav/bucket/file.bin", nil, 200, nil, true)
	if !bytes.Equal(body, payload) || !strings.Contains(headers.Get("Content-Security-Policy"), "sandbox") || !strings.HasPrefix(headers.Get("Content-Disposition"), "attachment") {
		t.Fatal("DAV download lost payload or data isolation")
	}
	_, body = do("GET", "/dav/bucket/file.bin", nil, 206, map[string]string{"Range": "bytes=1-3"}, true)
	if !bytes.Equal(body, payload[1:4]) {
		t.Fatal("Range did not use the seekable logical payload")
	}
	do("PUT", "/dav/bucket/file.bin", []byte("replacement"), 412, map[string]string{"If-Match": `"stale"`}, true)
	do("PUT", "/dav/bucket/too-large", bytes.Repeat([]byte("x"), 65), 413, nil, true)
	headers, _ = do("OPTIONS", "/dav/bucket/file.bin", nil, 204, nil, true)
	if headers.Get("DAV") != "1, 3" || strings.Contains(headers.Get("Allow"), "LOCK") {
		t.Fatal("DAV must advertise library class 1/3, not locking")
	}
	_, body = do("PROPFIND", "/dav/bucket/", nil, 207, map[string]string{"Depth": "1"}, true)
	if !strings.Contains(string(body), "/dav/bucket/") || !strings.Contains(string(body), "/dav/bucket/file.bin") {
		t.Fatal("PROPFIND hrefs lost the mount")
	}
	_, body = do("PROPFIND", "/dav", nil, 207, map[string]string{"Depth": "1"}, true)
	if !strings.Contains(string(body), "/dav/") || !strings.Contains(string(body), "/dav/bucket/") {
		t.Fatal("root PROPFIND lost the mount or bucket")
	}
	do("LOCK", "/dav/bucket/file.bin", nil, 405, nil, true)
	do("MOVE", "/dav/bucket/file.bin", nil, 400, map[string]string{"Destination": "https://outside.invalid/dav/bucket/moved.bin"}, true)
	do("MOVE", "/dav/bucket/file.bin", nil, 412, map[string]string{"Destination": "/dav/bucket/moved.bin", "If-Match": `"stale"`}, true)
	do("MOVE", "/dav/bucket/file.bin", nil, 201, map[string]string{"Destination": server.URL + "/dav/bucket/moved.bin", "If-Match": etag}, true)
	do("GET", "/dav/bucket/file.bin", nil, 404, nil, true)
	do("PUT", "/dav/bucket/existing.bin", []byte("old"), 201, nil, true)
	do("MOVE", "/dav/bucket/existing.bin", nil, 412, map[string]string{"Destination": "/dav/bucket/moved.bin", "Overwrite": "F"}, true)
	do("MOVE", "/dav/bucket/moved.bin", nil, 204, map[string]string{"Destination": "/dav/bucket/existing.bin", "Overwrite": "T"}, true)
	do("MOVE", "/dav/bucket/existing.bin", nil, 201, map[string]string{"Destination": "/dav/bucket/moved.bin"}, true)
	headers, body = do("GET", "/dav/bucket/moved.bin", nil, 200, nil, true)
	if !bytes.Equal(body, payload) {
		t.Fatal("MOVE changed payload bytes")
	}
	commits, aborts := filesystem.commits.Load(), filesystem.aborts.Load()
	if _, _, err := s.putFile(context.Background(), "/bucket/moved.bin", failedBody{}, core.WriteOptions{ExpectedLength: -1}, webdav.CreateOptions{}); err == nil {
		t.Fatal("failed upload unexpectedly published")
	}
	short := httptest.NewRequest("PUT", "/dav/bucket/moved.bin", strings.NewReader("partial"))
	short.ContentLength = 20
	short.SetBasicAuth("admin", "test-password")
	result := httptest.NewRecorder()
	s.Handler().ServeHTTP(result, short)
	if result.Code != 400 || filesystem.commits.Load() != commits || filesystem.aborts.Load() != aborts+2 {
		t.Fatal("failed or incomplete upload must abort without committing")
	}
	_, body = do("GET", "/dav/bucket/moved.bin", nil, 200, nil, true)
	if !bytes.Equal(body, payload) {
		t.Fatal("failed upload overwrote old data")
	}
	do("DELETE", "/dav/bucket/moved.bin", nil, 204, map[string]string{"If-Match": headers.Get("ETag")}, true)
	do("GET", "/dav/bucket/moved.bin", nil, 404, nil, true)
	do("DELETE", "/api/session", nil, 200, map[string]string{"X-GuluDrive-Request": "1"}, false)
	do("GET", "/api/status", nil, 401, nil, false)
}

func TestGoogleFailureKeepsSession(t *testing.T) {
	const token = "oauth2_4/offline-import-fixture"
	var upstreamStatus atomic.Int32
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		status := int(upstreamStatus.Load())
		w.WriteHeader(status)
		if status == http.StatusUnauthorized {
			_, _ = io.WriteString(w, "Error=BadAuthentication\n")
		} else {
			_, _ = io.WriteString(w, "Error=ServiceUnavailable\n")
		}
		_, _ = io.WriteString(w, "Info="+token+" offline-master fixture@example.com https://signed.invalid/\n")
	}))
	defer google.Close()
	cfg := Config{DataDir: t.TempDir(), MaxFileSize: 64}
	db, err := database.Open(context.Background(), cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := photos.OpenAccounts(db, photos.AuthOptions{HTTPClient: google.Client(), Endpoints: photos.AuthEndpoints{Exchange: google.URL, Bearer: google.URL, Validate: google.URL}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := newTestServer(t, cfg, &smokeFS{}, accounts, db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	do := func(method, route, body string, cookie *http.Cookie, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, route, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-GuluDrive-Request", "1")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: HTTP %d, want %d", method, route, w.Code, status)
		}
		return w
	}
	do("GET", "/api/status", "", nil, 401)
	login := do("POST", "/api/session", `{"username":"admin","password":"test-password"}`, nil, 200)
	cookie := login.Result().Cookies()[0]
	for _, status := range []int{401, 500} {
		upstreamStatus.Store(int32(status))
		want, reason := 422, "BadAuthentication"
		if status == 500 {
			want, reason = 502, "ServiceUnavailable"
		}
		failure := do("POST", "/api/accounts/google", `{"oauth_token":"`+token+`"}`, cookie, want)
		for _, secret := range []string{token, "offline-master", "fixture@example.com", "signed.invalid"} {
			if strings.Contains(failure.Body.String(), secret) {
				t.Fatal("API exposed raw authentication data")
			}
		}
		var diagnostic map[string]any
		if json.Unmarshal(failure.Body.Bytes(), &diagnostic) != nil || diagnostic["code"] != "google_auth_failed" || diagnostic["stage"] != "exchange_master" || diagnostic["upstreamStatus"] != float64(status) || diagnostic["reason"] != reason {
			t.Fatal("safe Google diagnostics were lost")
		}
		do("GET", "/api/status", "", cookie, 200) // The exact original session cookie still works.
	}
	do("POST", "/api/accounts/google", `{"oauth_token":"short"}`, cookie, 400)
	do("GET", "/api/status", "", cookie, 200)

	failure := httptest.NewRecorder()
	apiFailure(failure, core.ErrUnauthorized)
	var diagnostic map[string]any
	if json.Unmarshal(failure.Body.Bytes(), &diagnostic) != nil || failure.Code != 502 || diagnostic["code"] != "google_auth_failed" || diagnostic["stage"] != nil || errorStatus(core.ErrUnauthorized) != 401 {
		t.Fatal("backend authentication must not expire JSON API sessions or change DAV status mapping")
	}
	failure = httptest.NewRecorder()
	apiFailure(failure, core.ErrUncertain)
	if json.Unmarshal(failure.Body.Bytes(), &diagnostic) != nil || failure.Code != 503 || diagnostic["code"] != "outcome_uncertain" || diagnostic["retryable"] != false {
		t.Fatal("uncertain outcomes must remain non-retryable 503 errors")
	}
}

func TestRemoteFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		upstream, status    int
		stage, reason, code string
		cause               error
	}{
		{400, 502, "http_response", "rejected", "remote_operation_failed", nil},
		{200, 503, "decode_album_id", "missing_album_id", "outcome_uncertain", core.ErrUncertain},
		{401, 502, "http_response", "rejected", "google_auth_failed", core.ErrUnauthorized},
	} {
		remote := &core.RemoteError{Operation: "create_album", Stage: tc.stage, UpstreamStatus: tc.upstream, Reason: tc.reason, Cause: errors.Join(tc.cause, errors.New("secret token fixture@example.com https://signed.invalid/"))}
		if tc.upstream == 400 {
			remote.BodyFormat, remote.RPCCode, remote.ErrorReason, remote.ViolationFields = "protobuf", 3, "SERVICE_DISABLED", []string{"4"}
		}
		w := httptest.NewRecorder()
		apiFailure(w, remote)
		var diagnostic map[string]any
		if json.Unmarshal(w.Body.Bytes(), &diagnostic) != nil || w.Code != tc.status || diagnostic["code"] != tc.code || diagnostic["operation"] != "create_album" || diagnostic["stage"] != tc.stage || diagnostic["upstreamStatus"] != float64(tc.upstream) || diagnostic["reason"] != tc.reason {
			t.Fatalf("structured remote diagnostic: HTTP %d %s", w.Code, w.Body.String())
		}
		if tc.upstream == 400 {
			fields, ok := diagnostic["violationFields"].([]any)
			if diagnostic["bodyFormat"] != "protobuf" || diagnostic["rpcCode"] != float64(3) || diagnostic["errorReason"] != "SERVICE_DISABLED" || !ok || len(fields) != 1 || fields[0] != "4" {
				t.Fatal("API lost safe RPC diagnostics")
			}
		}
		if tc.status == 503 && diagnostic["retryable"] != false {
			t.Fatal("uncertain operations must not be automatically retried")
		}
		for _, secret := range []string{"secret", "fixture@example.com", "signed.invalid", "private journal"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("API leaked raw provider data or replaced diagnostics with private-journal advice")
			}
		}
	}
	w := httptest.NewRecorder()
	apiFailure(w, &core.RemoteError{Operation: "secret operation", Stage: "secret stage", UpstreamStatus: 9999, Reason: "secret reason", BodyFormat: "secret format", RPCCode: 99, ErrorReason: "OFFLINE_BEARER_SECRET", ViolationFields: []string{"fixture@example.com"}})
	var diagnostic map[string]any
	if json.Unmarshal(w.Body.Bytes(), &diagnostic) != nil || w.Code != 502 || diagnostic["operation"] != "unknown" || diagnostic["stage"] != "unknown" || diagnostic["upstreamStatus"] != float64(0) || diagnostic["reason"] != "unknown" || diagnostic["bodyFormat"] != nil || diagnostic["rpcCode"] != nil || diagnostic["errorReason"] != nil || diagnostic["violationFields"] != nil || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("API diagnostic allowlist was bypassed")
	}
}

func TestOpenEmptyGoogleAndInstanceLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-created")
	if s, err := Open(context.Background(), Config{DataDir: root, MaxFileSize: -1}); s != nil || !errors.Is(err, core.ErrInvalid) {
		t.Fatal("negative file limit was not rejected before initialization", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("invalid file size configuration created service state")
	}
	cfg := Config{DataDir: t.TempDir(), Proxy: "http://127.0.0.1:1"}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if s.maxFile != math.MaxInt64 {
		t.Fatal("zero configuration did not remove the configured logical file cap")
	}
	accounts, err := s.accounts.Accounts(context.Background())
	if err != nil || len(accounts) != 0 {
		t.Fatal("empty Google service must start without a network login")
	}
	if err := s.fs.Mkdir(context.Background(), "/needs-account"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("bucket creation must use the injected account metadata service", err)
	}
	other, err := Open(context.Background(), cfg)
	if other != nil {
		_ = other.Close(context.Background())
	}
	if !errors.Is(err, core.ErrBusy) {
		t.Fatal("second instance was not rejected")
	}
	if err = s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(cfg.DataDir, "instance.lock")); err != nil {
		t.Fatal("instance lockfile was removed")
	}
}

func TestPendingBucketContractAndAccountGuard(t *testing.T) {
	filesystem := &smokeFS{buckets: []core.Bucket{{ID: 2, Name: "pending", Album: core.AlbumRef{AccountID: "account-a"}}}}
	s, err := newTestServer(t, Config{}, filesystem, smokeAccounts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if err := s.removeAccount(context.Background(), "account-a"); !errors.Is(err, core.ErrConflict) {
		t.Fatal("an unbound bucket must still protect its pinned account", err)
	}
	w := httptest.NewRecorder()
	s.api(w, httptest.NewRequest("GET", "/api/buckets", nil))
	var dto struct {
		Buckets []core.Bucket `json:"buckets"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &dto) != nil || len(dto.Buckets) != 1 || dto.Buckets[0].Album.ID != "" || dto.Buckets[0].Album.AccountID != "account-a" || !strings.Contains(w.Body.String(), `"id":""`) {
		t.Fatal("pending bindings must use the unchanged shared bucket DTO")
	}
	filesystem.buckets = nil
	if _, err = s.db.Exec(`INSERT INTO objects(account,remote,dedup,sha1,size,format,off,length,unused_at,state,parts) VALUES('account-a','fixture','','',1,'guludrive-bmp-v2',0,1,0,'trash_uncertain','{"version":1,"logicalSize":1,"parts":[]}')`); err != nil {
		t.Fatal(err)
	}
	if err = s.removeAccount(context.Background(), "account-a"); !errors.Is(err, core.ErrConflict) {
		t.Fatal("unreferenced remote objects lost their account protection", err)
	}
	if _, err = s.db.Exec(`DELETE FROM objects; INSERT INTO jobs(id,kind,path,state,created,updated,remote_account) VALUES('fixture','write','/gone','uncertain',0,0,'account-a')`); err != nil {
		t.Fatal(err)
	}
	if err = s.removeAccount(context.Background(), "account-a"); !errors.Is(err, core.ErrConflict) {
		t.Fatal("uncertain remote receipts lost their account protection", err)
	}
	if _, err = s.db.Exec(`UPDATE jobs SET state='published'`); err != nil {
		t.Fatal(err)
	}
	if err = s.removeAccount(context.Background(), "account-a"); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("terminal history should allow the account service to decide removal", err)
	}
}

func TestPrivateRootExecutableLocation(t *testing.T) {
	if os.Getenv("GULUDRIVE_TEST_EXECUTABLE_ROOT") == "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		want, err := canonicalPath(filepath.Dir(executable))
		if err != nil {
			t.Fatal(err)
		}
		root, lock, err := privateRoot("")
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if root != want {
			t.Fatalf("default data root = %q, want executable directory %q", root, want)
		}
		cwd, err := os.Getwd()
		if err != nil || root == cwd {
			t.Fatal("test requires a working directory distinct from the executable", err)
		}
		explicit := filepath.Join(cwd, "chosen", "state")
		selected, explicitLock, err := privateRoot(explicit)
		if err != nil {
			t.Fatal(err)
		}
		defer explicitLock.Close()
		want, err = canonicalPath(explicit)
		if err != nil || selected != want {
			t.Fatal("explicit data directory did not override the executable directory", err)
		}
		return
	}
	base := t.TempDir()
	appDir, workDir := filepath.Join(base, "app"), filepath.Join(base, "working")
	for _, dir := range []string{appDir, workDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	copyPath := filepath.Join(appDir, "storage-test"+filepath.Ext(executable))
	destination, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(destination, source)
	if err = errors.Join(copyErr, destination.Close()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APPDATA", filepath.Join(base, "config"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	cmd := exec.Command(copyPath, "-test.run=^TestPrivateRootExecutableLocation$")
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "GULUDRIVE_TEST_EXECUTABLE_ROOT=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("executable-relative storage smoke: %v\n%s", err, output)
	}
}

func TestEmbeddedStatic(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	s, err := newTestServer(t, Config{DataDir: root}, &smokeFS{}, smokeAccounts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	assets := webui.Assets()
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil || !bytes.Contains(index, []byte("GuluDrive")) || !bytes.Contains(index, []byte(`<div id="app"></div>`)) {
		t.Fatal("embedded build must contain the real GuluDrive application", err)
	}
	request := func(handler http.Handler, method, target, byteRange string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, target, nil)
		if byteRange != "" {
			r.Header.Set("Range", byteRange)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: HTTP %d, want %d", method, target, w.Code, want)
		}
		return w
	}
	w := request(s.Handler(), "GET", "/", "", http.StatusOK)
	if !bytes.Equal(w.Body.Bytes(), index) || strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") || w.Header().Get("Content-Disposition") != "" {
		t.Fatal("trusted UI was replaced or given download isolation")
	}
	for _, header := range []string{"X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options"} {
		if w.Header().Get(header) == "" {
			t.Fatal("static response lost security header", header)
		}
	}
	head := request(s.Handler(), "HEAD", "/", "", http.StatusOK)
	if head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(len(index)) {
		t.Fatal("embedded HEAD lost its content metadata")
	}
	ranged := request(s.Handler(), "GET", "/", "bytes=0-15", http.StatusPartialContent)
	if !bytes.Equal(ranged.Body.Bytes(), index[:16]) {
		t.Fatal("embedded Range lost its byte boundary")
	}
	var assetCount int
	if err := fs.WalkDir(assets, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && name != "index.html" {
			assetCount++
			request(s.Handler(), "GET", (&url.URL{Path: "/" + name}).String(), "", http.StatusOK)
		}
		return nil
	}); err != nil || assetCount == 0 {
		t.Fatal("embedded assets are missing", err)
	}
	for _, target := range []string{"/store.db", "/store.db-wal", "/store.db-shm", "/instance.lock", "/temp/path", "/spool/path", "/missing/deep", "/assets/", "/legal/"} {
		request(s.Handler(), "GET", target, "", http.StatusNotFound)
	}
	for _, target := range []string{"/../store.db", "/temp/../store.db", "/spool/../store.db", "/%2e%2e/instance.lock", "/%5cstore.db"} {
		request(staticHandler(assets), "GET", target, "", http.StatusNotFound)
	}
	request(s.Handler(), "POST", "/", "", http.StatusMethodNotAllowed)
}

func TestFirstRunHTTPSProxyOrigin(t *testing.T) {
	s, err := Open(context.Background(), Config{DataDir: t.TempDir(), Proxy: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	const credentials = `{"username":"owner","password":"proxy-test-password"}`
	request := func(method, path string, cookie *http.Cookie) *http.Request {
		r := httptest.NewRequest(method, "http://drive.example"+path, strings.NewReader(credentials))
		r.Header.Set("Origin", "https://drive.example")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-GuluDrive-Request", "1")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		return r
	}
	send := func(r *http.Request, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: status %d, want %d", r.Method, r.URL.Path, w.Code, want)
		}
		return w
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Origin", "https://outside.example") },
		func(r *http.Request) { r.Header.Set("Origin", "https://drive.example:8443") },
		func(r *http.Request) { r.Header.Add("Origin", "https://drive.example") },
		func(r *http.Request) { r.Header.Set("Origin", "https://drive.example/path") },
		func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
	} {
		r := request("POST", "/api/setup", nil)
		mutate(r)
		r.Header.Set("Forwarded", "proto=https;host=drive.example")
		r.Header.Set("X-Forwarded-Proto", "https")
		send(r, 403)
	}
	send(request("GET", "/api/status", nil), 401)
	setup := send(request("POST", "/api/setup", nil), 200)
	if strings.TrimSpace(setup.Body.String()) != `{"initialized":true}` || len(setup.Result().Cookies()) != 0 {
		t.Fatal("HTTPS proxy setup must initialize without logging in")
	}
	login := send(request("POST", "/api/session", nil), 200)
	if len(login.Result().Cookies()) != 1 || !login.Result().Cookies()[0].Secure {
		t.Fatal("same-authority HTTPS login must use the secure cookie snapshot")
	}
	cookie := login.Result().Cookies()[0]
	read := request("GET", "/api/status", cookie)
	read.Header.Del("Origin")
	send(read, 200) // Origin-less GET uses the shared SCS store, not another login.
	if s.security.publicOrigin() != "" {
		t.Fatal("request scheme inference must not persist a public origin")
	}
	s.security.setPublicOrigin("https://drive.example")
	downgrade := request("POST", "/api/session", nil)
	downgrade.Header.Set("Origin", "http://drive.example")
	send(downgrade, 403)
	local := request("POST", "/api/session", nil)
	local.Host = "127.0.0.1:8787"
	local.Header.Set("Origin", "http://127.0.0.1:8787")
	plain := send(local, 200)
	if len(plain.Result().Cookies()) != 1 || plain.Result().Cookies()[0].Secure {
		t.Fatal("external proxy configuration changed the local HTTP session")
	}
}

func TestAdministratorPersistenceAndDatabaseOwnership(t *testing.T) {
	ctx := context.Background()
	t.Chdir(t.TempDir())
	root := t.TempDir()
	cfg := Config{DataDir: root, Proxy: "http://127.0.0.1:1", PublicOrigin: "https://public.example.invalid", MaxFileSize: 64}
	s, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal("empty database must start pending setup", err)
	}
	defer s.Close(ctx)
	send := func(method, route, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://127.0.0.1:8787"+route, strings.NewReader(body))
		r.Header.Set("Origin", "http://127.0.0.1:8787")
		r.Header.Set("Forwarded", "proto=https;host=public.example.invalid")
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Host", "public.example.invalid")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-GuluDrive-Request", "1")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: status %d, want %d", method, route, w.Code, want)
		}
		return w
	}
	status := send("GET", "/api/setup", "", 200)
	if strings.TrimSpace(status.Body.String()) != `{"initialized":false}` || status.Header().Get("Set-Cookie") != "" || status.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("setup status must reveal only initialization state")
	}
	send("GET", "/api/status", "", 401)
	send("POST", "/api/proxy-script", "{}", 401)
	send("GET", "/dav/", "", 401)
	send("POST", "/api/session", `{"username":"owner","password":"first-test-password"}`, 401)
	setupBody := `{"username":"owner","password":"first-test-password"}`
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("X-GuluDrive-Request") },
		func(r *http.Request) { r.Header.Set("Origin", "https://public.example.invalid") },
		func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/setup", strings.NewReader(setupBody))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-GuluDrive-Request", "1")
		mutate(r)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("setup bypassed the login request protections", w.Code)
		}
	}
	for _, body := range []string{
		`{"username":"owner:invalid","password":"first-test-password"}`,
		`{"username":"","password":"first-test-password"}`,
		`{"username":"owner","password":""}`,
		`{"username":"owner","password":"line\nbreak"}`,
	} {
		send("POST", "/api/setup", body, 400)
	}
	send("POST", "/api/setup", strings.Repeat("x", maxJSON+1), 413)
	s.security.mu.Lock()
	s.security.window, s.security.attempts = time.Now(), 20
	s.security.mu.Unlock()
	if w := send("POST", "/api/setup", setupBody, 429); w.Header().Get("Retry-After") != "60" {
		t.Fatal("setup must share the login rate limit")
	}
	s.security.mu.Lock()
	s.security.attempts = 0
	s.security.mu.Unlock()
	if _, err = s.db.Exec(`CREATE TRIGGER fail_initial_admin BEFORE INSERT ON settings WHEN NEW.key='admin.username' BEGIN SELECT RAISE(ABORT, 'offline fixture'); END`); err != nil {
		t.Fatal(err)
	}
	send("POST", "/api/setup", setupBody, 500)
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM settings WHERE key IN (?, ?)", adminUsernameKey, adminPasswordKey).Scan(&count); err != nil || count != 0 || s.security.initialized() {
		t.Fatal("failed setup published partial credentials", err)
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_initial_admin"); err != nil {
		t.Fatal(err)
	}
	results := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- administratorRequest(s.Handler(), "POST", "/api/setup", setupBody, nil)
		}()
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for w := range results {
		counts[w.Code]++
		if w.Header().Get("Set-Cookie") != "" || (w.Code == 200 && strings.TrimSpace(w.Body.String()) != `{"initialized":true}`) {
			t.Fatal("setup must not log in automatically or expose credentials")
		}
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatal("racing setup must have exactly one winner", counts)
	}
	send("GET", "/api/status", "", 401)
	send("POST", "/api/setup", `{"username":"intruder","password":"not-saved"}`, 409)
	send("POST", "/api/session", `{"username":"owner","password":"wrong"}`, 401)
	login := send("POST", "/api/session", setupBody, 200)
	if len(login.Result().Cookies()) != 1 || login.Result().Cookies()[0].Secure {
		t.Fatal("external HTTPS origin forced Secure cookies onto local HTTP")
	}
	cookie := login.Result().Cookies()[0]
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/status", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("normal local session authentication failed", w.Code)
	}
	csrf := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/proxy-script", strings.NewReader("{}"))
	csrf.Header.Set("Content-Type", "application/json")
	csrf.AddCookie(cookie)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, csrf)
	if w.Code != 403 {
		t.Fatal("authenticated script generation bypassed the CSRF guard", w.Code)
	}
	proxied := httptest.NewRequest("POST", "http://public.example.invalid/api/session", strings.NewReader(setupBody))
	proxied.Header.Set("Origin", "https://public.example.invalid")
	proxied.Header.Set("Content-Type", "application/json")
	proxied.Header.Set("X-GuluDrive-Request", "1")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, proxied)
	if w.Code != 200 || !w.Result().Cookies()[0].Secure {
		t.Fatal("host-matched HTTPS proxy must use the immutable TLS manager", w.Code)
	}
	var username, hash string
	if err = s.db.QueryRow(`SELECT (SELECT value FROM settings WHERE key=?), (SELECT value FROM settings WHERE key=?)`, adminUsernameKey, adminPasswordKey).Scan(&username, &hash); err != nil || username != "owner" {
		t.Fatal("administrator identity was not saved atomically", err)
	}
	if cost, err := bcrypt.Cost([]byte(hash)); err != nil || cost != bcrypt.DefaultCost || bcrypt.CompareHashAndPassword([]byte(hash), []byte("first-test-password")) != nil {
		t.Fatal("administrator verifier is not salted bcrypt")
	}
	// The filesystem borrows this exact DB; the app owns its final close.
	if err = s.fs.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.db.PingContext(ctx); err != nil {
		t.Fatal("filesystem closed its borrowed database", err)
	}
	if err = s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.db.PingContext(ctx); err == nil {
		t.Fatal("app left its shared database open")
	}
	other, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal("app did not release its instance lock", err)
	}
	defer other.Close(ctx)
	saved := other.security.administrator()
	if saved.username != username || string(saved.passwordHash) != hash || !other.security.credentials("owner", "first-test-password") {
		t.Fatal("restart changed the existing administrator")
	}
	if w := administratorRequest(other.Handler(), "POST", "/api/setup", setupBody, nil); w.Code != 409 {
		t.Fatal("existing configured database allowed setup again", w.Code)
	}
	if err = other.Close(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE settings SET value='malformed' WHERE key=?", adminPasswordKey); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if broken, err := Open(ctx, cfg); err == nil {
		_ = broken.Close(ctx)
		t.Fatal("malformed stored verifier must remain a safe startup error")
	}
	for _, legacy := range []string{"admin-password", "backend", "auth", "vfs", "key", "accounts.enc"} {
		if _, err = os.Lstat(filepath.Join(root, legacy)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("runtime created a separate credential or catalog path", legacy)
		}
	}
}

func TestAdministratorProfileChanges(t *testing.T) {
	ctx := context.Background()
	cfg := Config{DataDir: t.TempDir()}
	s, err := newTestServer(t, cfg, &smokeFS{}, smokeAccounts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	send := func(method, target, body string, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := administratorRequest(s.Handler(), method, target, body, cookie)
		if w.Code != want {
			t.Fatalf("%s %s: status %d, want %d", method, target, w.Code, want)
		}
		return w
	}
	login := func(username, password string, want int) *http.Cookie {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"username": username, "password": password})
		w := send("POST", "/api/session", string(body), nil, want)
		if want != http.StatusOK {
			return nil
		}
		cookies := w.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatal("login did not set one session cookie")
		}
		return cookies[0]
	}
	profile := func(cookie *http.Cookie, username string) {
		t.Helper()
		w := send("GET", "/api/admin", "", cookie, 200)
		var public map[string]any
		if json.Unmarshal(w.Body.Bytes(), &public) != nil || len(public) != 1 || public["username"] != username {
			t.Fatal("administrator profile must expose only the live username")
		}
	}
	basic := func(username, password string, want int) {
		t.Helper()
		r := httptest.NewRequest("OPTIONS", "/dav/", nil)
		r.SetBasicAuth(username, password)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("DAV Basic status %d, want %d", w.Code, want)
		}
	}
	persisted := func() administrator {
		t.Helper()
		var name, hash string
		if err := s.db.QueryRow(`SELECT (SELECT value FROM settings WHERE key=?), (SELECT value FROM settings WHERE key=?)`, adminUsernameKey, adminPasswordKey).Scan(&name, &hash); err != nil {
			t.Fatal(err)
		}
		return administrator{username: name, passwordHash: []byte(hash)}
	}
	send("GET", "/api/admin", "", nil, 401)
	send("PATCH", "/api/admin", `{"currentPassword":"test-password","username":"intruder"}`, nil, 401)
	first, second := login("admin", "test-password", 200), login("admin", "test-password", 200)
	profile(first, "admin")
	wrong := send("PATCH", "/api/admin", `{"currentPassword":"wrong-test-password","username":"owner"}`, first, 403)
	if !strings.Contains(wrong.Body.String(), `"code":"invalid_current_password"`) || wrong.Header().Get("Set-Cookie") != "" || strings.Contains(wrong.Body.String(), "wrong-test-password") {
		t.Fatal("wrong current password must return a safe code without expiring the session")
	}
	profile(first, "admin")
	for _, body := range []string{
		`{"currentPassword":"test-password","username":"bad:name"}`,
		`{"currentPassword":"test-password","username":""}`,
		`{"currentPassword":"test-password","username":null}`,
		`{"currentPassword":"test-password","newPassword":null}`,
		`{"currentPassword":"test-password","username":"admin","newPassword":""}`,
		`{"currentPassword":"test-password","newPassword":"line\nbreak"}`,
	} {
		send("PATCH", "/api/admin", body, first, 400)
	}
	csrf := httptest.NewRequest("PATCH", "/api/admin", strings.NewReader(`{"currentPassword":"test-password","username":"owner"}`))
	csrf.Header.Set("Content-Type", "application/json")
	csrf.AddCookie(first)
	blocked := httptest.NewRecorder()
	s.Handler().ServeHTTP(blocked, csrf)
	if blocked.Code != 403 {
		t.Fatal("administrator updates bypassed the existing CSRF guard")
	}
	initial := persisted()
	changed := send("PATCH", "/api/admin", `{"currentPassword":"test-password","username":"owner","newPassword":""}`, first, 200)
	if strings.TrimSpace(changed.Body.String()) != `{"username":"owner","relogin":true}` || len(changed.Result().Cookies()) != 1 || changed.Result().Cookies()[0].MaxAge >= 0 {
		t.Fatal("successful change must require relogin and expire the caller cookie")
	}
	if got := persisted(); got.username != "owner" || !bytes.Equal(got.passwordHash, initial.passwordHash) {
		t.Fatal("username-only change did not retain the exact verifier")
	}
	send("GET", "/api/admin", "", first, 401)
	send("GET", "/api/admin", "", second, 401)
	login("admin", "test-password", 401)
	basic("admin", "test-password", 401)
	basic("owner", "test-password", 204)
	first, second = login("owner", "test-password", 200), login("owner", "test-password", 200)
	profile(first, "owner")

	// Fail the second setting write so rollback must also undo the username.
	before := persisted()
	beforeEpoch, _ := s.security.authenticate("owner", "test-password")
	if _, err := s.db.Exec(`CREATE TRIGGER fail_admin_password BEFORE UPDATE ON settings WHEN NEW.key='admin.password_hash' BEGIN SELECT RAISE(ABORT,'mock failure'); END`); err != nil {
		t.Fatal(err)
	}
	failed := send("PATCH", "/api/admin", `{"currentPassword":"test-password","username":"must-rollback","newPassword":"not-saved"}`, first, 500)
	after := persisted()
	afterEpoch, valid := s.security.authenticate("owner", "test-password")
	if failed.Header().Get("Set-Cookie") != "" || after.username != before.username || !bytes.Equal(after.passwordHash, before.passwordHash) || !valid || afterEpoch != beforeEpoch || !bytes.Equal(s.security.administrator().passwordHash, before.passwordHash) {
		t.Fatal("failed SQL changed credentials, epoch, caller cookie, or one persisted setting")
	}
	profile(first, "owner")
	profile(second, "owner")
	if _, err := s.db.Exec(`DROP TRIGGER fail_admin_password`); err != nil {
		t.Fatal(err)
	}
	changed = send("PATCH", "/api/admin", `{"currentPassword":"test-password","username":"new-owner","newPassword":"123456"}`, first, 200)
	if strings.TrimSpace(changed.Body.String()) != `{"username":"new-owner","relogin":true}` {
		t.Fatal("combined update returned the wrong public contract")
	}
	after = persisted()
	if after.username != "new-owner" || bytes.Equal(after.passwordHash, before.passwordHash) || bcrypt.CompareHashAndPassword(after.passwordHash, []byte("123456")) != nil {
		t.Fatal("combined update did not atomically save the name and salted bcrypt verifier")
	}
	send("GET", "/api/admin", "", first, 401)
	send("GET", "/api/admin", "", second, 401)
	basic("owner", "test-password", 401)
	basic("new-owner", "123456", 204)
	first = login("new-owner", "123456", 200)
	profile(first, "new-owner")
	send("PATCH", "/api/admin", `{"currentPassword":"123456","newPassword":"next-password"}`, first, 200)
	send("GET", "/api/admin", "", first, 401)
	first = login("new-owner", "next-password", 200)
	profile(first, "new-owner")
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	saved, err := loadAdministrator(ctx, db)
	if err != nil || saved.username != "new-owner" || bcrypt.CompareHashAndPassword(saved.passwordHash, []byte("next-password")) != nil {
		t.Fatal("normal startup did not preserve interactive credential changes", err)
	}
}

func TestAdministratorConcurrentAuthentication(t *testing.T) {
	s, err := newTestServer(t, Config{}, &smokeFS{}, smokeAccounts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	loginBody := `{"username":"admin","password":"test-password"}`
	loggedIn := administratorRequest(s.Handler(), "POST", "/api/session", loginBody, nil)
	if loggedIn.Code != 200 || len(loggedIn.Result().Cookies()) != 1 {
		t.Fatal("initial login failed")
	}
	cookie := loggedIn.Result().Cookies()[0]
	oldEpoch, _ := s.security.authenticate("admin", "test-password")
	var wg sync.WaitGroup
	cookies := make(chan *http.Cookie, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 2 {
			epoch, valid := s.security.authenticate("admin", "test-password")
			if valid && epoch != oldEpoch {
				t.Error("old credential check captured a new epoch")
			}
			w := administratorRequest(s.Handler(), "GET", "/api/admin", "", cookie)
			if w.Code != 200 && w.Code != 401 {
				t.Error("concurrent profile request failed")
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 2 {
			w := administratorRequest(s.Handler(), "POST", "/api/session", loginBody, nil)
			if w.Code == 200 && len(w.Result().Cookies()) == 1 {
				cookies <- w.Result().Cookies()[0]
			} else if w.Code != 401 {
				t.Error("concurrent login failed")
			}
			r := httptest.NewRequest("OPTIONS", "/dav/", nil)
			r.SetBasicAuth("admin", "test-password")
			w = httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 204 && w.Code != 401 {
				t.Error("concurrent DAV credential read failed")
			}
		}
	}()
	changed := administratorRequest(s.Handler(), "PATCH", "/api/admin", `{"currentPassword":"test-password","username":"owner","newPassword":"123456"}`, cookie)
	wg.Wait()
	close(cookies)
	if changed.Code != 200 {
		t.Fatal("concurrent credential update failed")
	}
	for candidate := range cookies {
		if w := administratorRequest(s.Handler(), "GET", "/api/admin", "", candidate); w.Code != 401 {
			t.Error("racing old-password login received a privileged new-epoch session")
		}
	}
	if w := administratorRequest(s.Handler(), "GET", "/api/admin", "", cookie); w.Code != 401 {
		t.Fatal("old session survived concurrent update")
	}
	if !s.security.credentials("owner", "123456") || s.security.credentials("admin", "test-password") {
		t.Fatal("concurrent update did not publish a coherent credential pair")
	}
}

func administratorRequest(handler http.Handler, method, target, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://guludrive.test"+target, strings.NewReader(body))
	if method != "GET" && method != "HEAD" {
		r.Header.Set("X-GuluDrive-Request", "1")
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestDownloadWorkerWire(t *testing.T) {
	// Pin the compact v2 JSON. The integration smoke separately verifies HMAC
	// over the literal unpadded base64url p using raw UTF-8 secret bytes.
	const secret = "GuluDrive-FAKE-test-secret-0123456789abcdef"
	payload := workerDownload{V: 2, Mode: "worker", Manifest: "https://drive.example.invalid/download-manifest?p=opaque&auth=scoped", Exp: 1793491500}
	parameters, err := downloadParameters(payload, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	const expectedJSON = `{"v":2,"mode":"worker","manifest":"https://drive.example.invalid/download-manifest?p=opaque\u0026auth=scoped","exp":1793491500}`
	if parameters.Get("p") != base64.RawURLEncoding.EncodeToString([]byte(expectedJSON)) {
		t.Fatal("Worker JSON/HMAC wire contract changed")
	}
}

// This reader keeps Open/Seek metadata-only, like the production VFS. Only
// Read can produce the upstream failure, including n > 0 with an error.
type downloadReadFS struct {
	*smokeFS
	readErr    error
	firstBytes int
	reads      atomic.Int64
}

type downloadReadHandle struct {
	io.ReadSeeker
	owner      *downloadReadFS
	readErr    error
	firstBytes int
}

func (f *downloadReadFS) Open(ctx context.Context, p string) (io.ReadSeekCloser, core.Entry, error) {
	entry, err := f.Stat(ctx, p)
	if err != nil {
		return nil, entry, err
	}
	return &downloadReadHandle{ReadSeeker: io.NewSectionReader(bytes.NewReader(f.files[p].data), 0, entry.Size), owner: f, readErr: f.readErr, firstBytes: f.firstBytes}, entry, nil
}

func (r *downloadReadHandle) Read(p []byte) (int, error) {
	r.owner.reads.Add(1)
	if r.readErr != nil {
		if r.firstBytes == 0 {
			return 0, r.readErr
		}
		n, _ := r.ReadSeeker.Read(p[:min(len(p), r.firstBytes)])
		r.firstBytes = 0
		return n, r.readErr
	}
	return r.ReadSeeker.Read(p)
}

func (r *downloadReadHandle) Close() error { return nil }

func TestDownloadFirstReadBoundary(t *testing.T) {
	const filePath = "/bucket/file.bin"
	payload := []byte("download-payload")
	entry := core.Entry{ID: 7, Version: 3, Path: filePath, Name: "file.bin", Size: int64(len(payload)), ModTime: time.Unix(1700000000, 0).UTC(), ContentType: "application/octet-stream"}
	filesystem := &downloadReadFS{smokeFS: &smokeFS{files: map[string]smokeFile{filePath: {entry: entry, data: payload}}}}
	s, err := newTestServer(t, Config{}, filesystem, smokeAccounts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	login := administratorRequest(s.Handler(), "POST", "/api/session", `{"username":"admin","password":"test-password"}`, nil)
	if login.Code != 200 {
		t.Fatal("test login failed")
	}
	cookie := login.Result().Cookies()[0]
	parameters, err := downloadParameters(serverDownload{V: 1, Mode: "server", Route: "local", Path: filePath, ID: entry.ID, Version: entry.Version, Exp: time.Now().Add(time.Minute).Unix()}, s.downloads.key)
	if err != nil {
		t.Fatal(err)
	}
	remote := &core.RemoteError{Operation: "download", Stage: "http_response", UpstreamStatus: 200, Reason: "rejected"}
	for _, target := range []string{"/api/file?path=%2Fbucket%2Ffile.bin", "/download?" + parameters.Encode(), "/dav/bucket/file.bin"} {
		t.Run(strings.Split(target, "?")[0], func(t *testing.T) {
			for _, tc := range []struct {
				name, method, byteRange, condition, value string
				carrierKind                               string
				readErr                                   error
				firstBytes                                int
				size                                      int64
				status                                    int
				body                                      string
				noRead                                    bool
			}{
				{name: "full failure", method: "GET", readErr: remote, size: 10993899, status: 502},
				{name: "range failure", method: "GET", byteRange: "bytes=1-3", readErr: remote, size: 10993899, status: 502},
				{name: "multipart failure", method: "GET", byteRange: "bytes=1-3,8-10", readErr: remote, size: 10993899, status: 502},
				{name: "range success", method: "GET", byteRange: "bytes=1-3", size: entry.Size, status: 206, body: "own"},
				{name: "multipart success", method: "GET", byteRange: "bytes=1-3,8-10", size: entry.Size, status: 206},
				{name: "partial n and error", method: "GET", byteRange: "bytes=1-7", readErr: remote, firstBytes: 3, size: entry.Size, status: 206, body: "own"},
				{name: "cancelled read", method: "GET", byteRange: "bytes=1-3", readErr: context.Canceled, size: entry.Size, status: 408},
				{name: "head", method: "HEAD", readErr: remote, size: entry.Size, status: 200, noRead: true},
				{name: "not modified", method: "GET", condition: "If-None-Match", value: entry.ETag(), readErr: remote, size: entry.Size, status: 304, noRead: true},
				{name: "precondition", method: "GET", condition: "If-Match", value: `"stale"`, readErr: remote, size: entry.Size, status: 412, noRead: true},
				{name: "unsatisfiable", method: "GET", byteRange: "bytes=100-101", readErr: remote, size: entry.Size, status: 416, noRead: true},
				{name: "empty", method: "GET", readErr: remote, size: 0, status: 200, noRead: true},
				{name: "mp4 range ignored", method: "GET", byteRange: "bytes=1-3", condition: "If-Range", value: `"stale"`, carrierKind: "mp4", size: entry.Size, status: 200, body: string(payload)},
				{name: "mp4 multipart ignored", method: "GET", byteRange: "bytes=1-3,8-10", carrierKind: "mp4", size: entry.Size, status: 200, body: string(payload)},
				{name: "mp4 unsatisfiable ignored", method: "GET", byteRange: "bytes=100-101", carrierKind: "mp4", size: entry.Size, status: 200, body: string(payload)},
				{name: "mp4 head range ignored", method: "HEAD", byteRange: "bytes=1-3", carrierKind: "mp4", size: entry.Size, status: 200, noRead: true},
				{name: "mp4 first read failure", method: "GET", byteRange: "bytes=1-3", carrierKind: "mp4", readErr: remote, size: entry.Size, status: 502},
				{name: "mp4 late failure", method: "GET", byteRange: "bytes=1-3", carrierKind: "mp4", readErr: remote, firstBytes: 3, size: entry.Size, status: 200, body: "dow"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					filesystem.readErr, filesystem.firstBytes = tc.readErr, tc.firstBytes
					filesystem.files[filePath] = smokeFile{entry: entry, data: payload}
					file := filesystem.files[filePath]
					file.entry.Size = tc.size
					file.entry.CarrierKind = tc.carrierKind
					filesystem.files[filePath] = file
					filesystem.reads.Store(0)
					r := httptest.NewRequest(tc.method, "http://guludrive.test"+target, nil)
					r.AddCookie(cookie)
					r.SetBasicAuth("admin", "test-password")
					if tc.byteRange != "" {
						r.Header.Set("Range", tc.byteRange)
					}
					if tc.condition != "" {
						r.Header.Set(tc.condition, tc.value)
					}
					w := httptest.NewRecorder()
					s.Handler().ServeHTTP(w, r)
					h := w.Result().Header // The committed headers, not mutable recorder state.
					if w.Code != tc.status {
						t.Fatalf("status %d, want %d; body %q", w.Code, tc.status, w.Body.String())
					}
					if !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
						t.Fatal("data isolation lost")
					}
					if tc.status == 502 || tc.status == 408 {
						for _, key := range []string{"Content-Range", "Content-Disposition", "ETag", "Last-Modified", "Accept-Ranges", "Content-Length"} {
							if h.Get(key) != "" {
								t.Fatalf("failure retained file header %s", key)
							}
						}
						if !json.Valid(w.Body.Bytes()) || strings.Contains(w.Body.String(), "--") {
							t.Fatal("error leaked multipart preamble or invalid JSON")
						}
					}
					if tc.body != "" && w.Body.String() != tc.body {
						t.Fatalf("payload %q, want %q", w.Body.String(), tc.body)
					}
					if tc.name == "range success" && (h.Get("Content-Range") != "bytes 1-3/16" || h.Get("Content-Length") != "3" || h.Get("Accept-Ranges") != "bytes") {
						t.Fatal("standard Range metadata lost")
					}
					if tc.carrierKind == "mp4" && tc.status == http.StatusOK && (h.Get("Accept-Ranges") != "none" || h.Get("Content-Range") != "" || h.Get("Content-Length") != strconv.FormatInt(tc.size, 10)) {
						t.Fatalf("MP4 range metadata: Accept-Ranges=%q Content-Range=%q Content-Length=%q", h.Get("Accept-Ranges"), h.Get("Content-Range"), h.Get("Content-Length"))
					}
					if tc.name == "partial n and error" && h.Get("Content-Length") != "7" {
						t.Fatal("late error must remain a partial, not rewritten successful full response")
					}
					if tc.name == "multipart success" {
						kind, params, err := mime.ParseMediaType(h.Get("Content-Type"))
						if err != nil || kind != "multipart/byteranges" {
							t.Fatal("missing multipart response")
						}
						parts := multipart.NewReader(w.Body, params["boundary"])
						for _, want := range []string{"own", "-pa"} {
							part, err := parts.NextPart()
							if err != nil {
								t.Fatal(err)
							}
							body, err := io.ReadAll(part)
							if err != nil || string(body) != want {
								t.Fatalf("multipart payload %q, want %q", body, want)
							}
						}
						if _, err := parts.NextPart(); err != io.EOF {
							t.Fatal("multipart did not terminate cleanly", err)
						}
					}
					if tc.noRead && filesystem.reads.Load() != 0 {
						t.Fatal("metadata-only response performed an upstream read")
					}
					if (tc.method == "HEAD" || tc.name == "empty") && w.Body.Len() != 0 {
						t.Fatal("metadata-only response emitted a body")
					}
				})
			}
		})
	}
}

func TestDownloadLinksAndSettings(t *testing.T) {
	const filePath = "/bucket/file.bin"
	const secret = "GuluDrive-FAKE-test-secret-0123456789abcdef"
	payload := []byte("download-payload")
	entry := core.Entry{ID: 7, Version: 3, Path: filePath, Name: "file.bin", Size: int64(len(payload)), ModTime: time.Unix(1700000000, 0).UTC(), ContentType: "application/octet-stream"}
	filesystem := &smokeFS{files: map[string]smokeFile{filePath: {entry: entry, data: payload}}}
	s, err := newTestServer(t, Config{PublicOrigin: "https://guludrive.example.invalid"}, filesystem, smokeAccounts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	var cookie *http.Cookie
	send := func(method, target, body string, authenticated bool, want int, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		if strings.HasPrefix(target, "/") {
			target = "http://guludrive.example.invalid" + target
		}
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		if method != "GET" && method != "HEAD" {
			r.Header.Set("X-GuluDrive-Request", "1")
		}
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if authenticated && cookie != nil {
			r.AddCookie(cookie)
		}
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: status %d, want %d", method, w.Code, want)
		}
		if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), base64.RawURLEncoding.EncodeToString(s.downloads.key)) {
			t.Fatal("settings or link response disclosed a signing secret")
		}
		return w
	}
	send("GET", "/api/download-settings", "", false, 401, nil)
	login := send("POST", "/api/session", `{"username":"admin","password":"test-password"}`, false, 200, nil)
	cookie = login.Result().Cookies()[0]
	send("PUT", "/api/download-settings", `{"mode":"local","publicOrigin":"https://guludrive.example.invalid","workerURL":""}`, true, 403, map[string]string{"X-GuluDrive-Request": ""})
	settings := send("GET", "/api/download-settings", "", true, 200, nil)
	if strings.TrimSpace(settings.Body.String()) != `{"mode":"local","workerURL":"","workerConfigured":false,"publicOrigin":"https://guludrive.example.invalid","publicOriginReady":true}` {
		t.Fatal("default settings must expose only the frozen public view")
	}
	var link struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expiresAt"`
		Mode      string    `json:"mode"`
	}
	created := send("POST", "/api/download-links", `{"path":"/bucket/file.bin"}`, true, 200, nil)
	if json.Unmarshal(created.Body.Bytes(), &link) != nil || link.Mode != "local" || !strings.HasPrefix(link.URL, "https://guludrive.example.invalid/download?") || time.Until(link.ExpiresAt) < 14*time.Minute || time.Until(link.ExpiresAt) > 15*time.Minute || filesystem.prepares.Load() != 0 {
		t.Fatal("local links must be short-lived and must not prepare a Google URL")
	}
	localURL, err := url.Parse(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	localRaw, err := base64.RawURLEncoding.DecodeString(localURL.Query().Get("p"))
	if err != nil || bytes.Contains(localRaw, []byte("https://")) || len(localURL.Query()) != 2 {
		t.Fatal("local capabilities must not contain upstream URLs or other credentials")
	}
	head := send("HEAD", link.URL, "", false, 200, nil)
	if head.Header().Get("Content-Length") != strconv.Itoa(len(payload)) || head.Body.Len() != 0 || filesystem.opens.Load() != 0 {
		t.Fatal("signed HEAD must remain metadata-only without a session cookie")
	}
	partial := send("GET", link.URL, "", false, 206, map[string]string{"Range": "bytes=1-3"})
	if !bytes.Equal(partial.Body.Bytes(), payload[1:4]) || partial.Header().Get("Content-Range") != "bytes 1-3/16" || !strings.Contains(partial.Header().Get("Content-Security-Policy"), "sandbox") || !strings.HasPrefix(partial.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("signed Range lost logical metadata or download isolation")
	}
	full := send("GET", link.URL, "", false, 200, map[string]string{"Range": "bytes=1-3", "If-Range": `"stale"`})
	if !bytes.Equal(full.Body.Bytes(), payload) {
		t.Fatal("signed If-Range must use ServeContent's standard behavior")
	}
	bad := *localURL
	query := bad.Query()
	query.Set("auth", strings.Repeat("A", 43))
	bad.RawQuery = query.Encode()
	send("GET", bad.String(), "", false, 403, nil)
	query = localURL.Query()
	query.Set("p", query.Get("p")+"A")
	bad.RawQuery = query.Encode()
	send("GET", bad.String(), "", false, 403, nil)
	expired, err := downloadParameters(serverDownload{V: 1, Mode: "server", Route: "local", Path: filePath, ID: entry.ID, Version: entry.Version, Exp: time.Now().Unix() - 1}, s.downloads.key)
	if err != nil {
		t.Fatal(err)
	}
	bad.RawQuery = expired.Encode()
	send("HEAD", bad.String(), "", false, 403, nil)
	send("POST", link.URL, "", false, 405, nil)
	send("DELETE", "/api/files?path=%2Fbucket%2Ffile.bin&"+localURL.RawQuery, "", false, 401, nil)
	filesystem.mu.Lock()
	changed := filesystem.files[filePath]
	changed.entry.Version++
	filesystem.files[filePath] = changed
	filesystem.mu.Unlock()
	send("GET", link.URL, "", false, 403, nil)
	filesystem.mu.Lock()
	changed.entry.ID++
	changed.entry.Version = entry.Version
	filesystem.files[filePath] = changed
	filesystem.mu.Unlock()
	send("HEAD", link.URL, "", false, 403, nil)
	filesystem.mu.Lock()
	filesystem.files[filePath] = smokeFile{entry: entry, data: payload}
	filesystem.mu.Unlock()

	send("PUT", "/api/download-settings", `{"mode":"proxy","publicOrigin":"https://guludrive.example.invalid","workerURL":"https://downloads.example.invalid/?bad=1","workerSecret":"`+secret+`"}`, true, 400, nil)
	send("PUT", "/api/download-settings", `{"mode":"proxy","publicOrigin":"https://guludrive.example.invalid","workerURL":"https://downloads.example.invalid/","workerSecret":"bad\npassword"}`, true, 400, nil)
	updated := send("PUT", "/api/download-settings", `{"mode":"proxy","publicOrigin":"https://guludrive.example.invalid","workerURL":"https://downloads.example.invalid/","workerSecret":"`+secret+`"}`, true, 200, nil)
	var public map[string]any
	if json.Unmarshal(updated.Body.Bytes(), &public) != nil || len(public) != 5 || public["publicOrigin"] != "https://guludrive.example.invalid" || public["mode"] != "proxy" || public["workerConfigured"] != true || public["publicOriginReady"] != true {
		t.Fatal("settings must never echo a Worker secret or accept workerConfigured as authoritative")
	}
	// A blank input retains the exact saved proxy-password bytes.
	send("PUT", "/api/download-settings", `{"mode":"proxy","publicOrigin":"https://guludrive.example.invalid","workerURL":"https://downloads.example.invalid/","workerSecret":""}`, true, 200, nil)
	reloaded, err := loadDownloads(context.Background(), s.db)
	if err != nil || reloaded.settings.workerSecret != secret || !bytes.Equal(reloaded.key, s.downloads.key) || reloaded.settings.mode != "proxy" {
		t.Fatal("download state or its signing key was not persisted in the shared database", err)
	}
	status := send("GET", "/api/status", "", true, 200, nil)
	var runtime struct {
		GDirect core.GDirectStatus `json:"gdirect"`
	}
	if json.Unmarshal(status.Body.Bytes(), &runtime) != nil || runtime.GDirect.Enabled || runtime.GDirect.IP != "" || runtime.GDirect.Selecting {
		t.Fatal("status must report the router's actual peer, never a fabricated Worker IP")
	}
	workerLink := send("POST", "/api/download-links", `{"path":"/bucket/file.bin"}`, true, 200, nil)
	if json.Unmarshal(workerLink.Body.Bytes(), &link) != nil || link.Mode != "proxy" || !strings.HasPrefix(link.URL, "https://downloads.example.invalid/?") || filesystem.prepares.Load() != 0 {
		t.Fatal("Worker link issuance must remain metadata-only")
	}
	workerURL, err := url.Parse(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	workerP := workerURL.Query().Get("p")
	workerRaw, err := base64.RawURLEncoding.DecodeString(workerP)
	if err != nil {
		t.Fatal(err)
	}
	var source workerDownload
	var fields map[string]any
	if json.Unmarshal(workerRaw, &source) != nil || json.Unmarshal(workerRaw, &fields) != nil || len(fields) != 4 || source.V != 2 || source.Mode != "worker" || source.Exp != link.ExpiresAt.Unix() || !strings.HasPrefix(source.Manifest, "https://guludrive.example.invalid/download-manifest?") || strings.Contains(string(workerRaw), "googleusercontent") {
		t.Fatal("Worker v2 must contain only the scoped public manifest capability")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(workerP))
	if workerURL.Query().Get("auth") != base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("Worker signature must use raw UTF-8 secret bytes over literal p")
	}
	send("GET", "/download?"+workerURL.RawQuery, "", false, 403, nil)
	opensBeforeManifest := filesystem.opens.Load()
	manifestURL, err := url.Parse(source.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	metadata := send("GET", source.Manifest+"&meta=1", "", false, 200, nil)
	var manifest struct {
		workerManifest
		Segments []workerSegment `json:"segments"`
	}
	if json.Unmarshal(metadata.Body.Bytes(), &manifest) != nil || manifest.V != 2 || manifest.Size != entry.Size || manifest.ETag != entry.ETag() || len(manifest.Segments) != 0 || filesystem.prepares.Load() != 0 {
		t.Fatal("metadata-only manifest resolved Google URLs or lost immutable metadata")
	}
	if head := send("HEAD", source.Manifest, "", false, 200, nil); head.Body.Len() != 0 || filesystem.prepares.Load() != 0 {
		t.Fatal("manifest HEAD performed preparation or emitted a body")
	}
	send("GET", "/download?"+manifestURL.RawQuery, "", false, 403, nil)
	send("GET", "/download-manifest?"+localURL.RawQuery, "", false, 403, nil)
	send("GET", source.Manifest+"&meta=1&meta=1", "", false, 403, nil)
	send("GET", source.Manifest+"&extra=1", "", false, 403, nil)
	send("GET", source.Manifest+"&p=duplicate", "", false, 403, nil)
	send("POST", source.Manifest, "", false, 405, nil)
	stale := entry
	stale.Version++
	filesystem.prepareEntry = &stale
	send("GET", source.Manifest, "", false, 403, nil)
	filesystem.prepareEntry = nil
	filesystem.segments = []core.DownloadSegment{
		{Offset: 0, Size: 7, URL: "https://video-downloads.googleusercontent.com/part1", PayloadOffset: 12, CarrierSize: 19},
		{Offset: 7, Size: 9, URL: "https://video-downloads.googleusercontent.com/part2", PayloadOffset: 4, CarrierSize: 13},
	}
	prepared := send("GET", source.Manifest, "", false, 200, nil)
	if json.Unmarshal(prepared.Body.Bytes(), &manifest) != nil || len(manifest.Segments) != 2 || manifest.Segments[1].Offset != 7 || manifest.Segments[1].Size != 9 || manifest.Segments[1].PayloadOffset != 4 || manifest.Segments[1].CarrierSize != 13 || manifest.Segments[1].URL != filesystem.segments[1].URL || prepared.Header().Get("Cache-Control") != "no-store" || !strings.Contains(prepared.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("manifest did not expose the validated contiguous segment wire")
	}
	filesystem.segments[1].Offset++
	invalidManifest := send("GET", source.Manifest, "", false, 502, nil)
	if !strings.Contains(invalidManifest.Body.String(), `"code":"proxy_manifest_unavailable"`) || strings.Contains(invalidManifest.Body.String(), "googleusercontent") {
		t.Fatal("invalid manifest leaked provider data or fell back locally")
	}
	filesystem.segments[1].Offset--
	filesystem.segments[1].CarrierSize--
	send("GET", source.Manifest, "", false, 502, nil)
	filesystem.segments[1].CarrierSize++
	filesystem.prepareErr = errors.New("private provider credentials https://signed.invalid/secret")
	failedManifest := send("GET", source.Manifest, "", false, 502, nil)
	if strings.Contains(failedManifest.Body.String(), "credentials") || strings.Contains(failedManifest.Body.String(), "signed.invalid") {
		t.Fatal("manifest exposed raw provider failure")
	}
	filesystem.prepareErr = nil
	expiredManifest, err := downloadParameters(serverDownload{V: 1, Mode: "manifest", Path: filePath, ID: entry.ID, Version: entry.Version, Exp: time.Now().Unix() - 1}, s.downloads.key)
	if err != nil {
		t.Fatal(err)
	}
	send("GET", "/download-manifest?"+expiredManifest.Encode(), "", false, 403, nil)
	manifestPrepares := filesystem.prepares.Load()
	if filesystem.opens.Load() != opensBeforeManifest {
		t.Fatal("manifest read logical payload bytes")
	}

	// Force the last settings write to fail after earlier writes. The database,
	// public view and router must all remain at their last committed values.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_download_setting BEFORE UPDATE ON settings WHEN NEW.key='gdirect.worker_secret' BEGIN SELECT RAISE(ABORT,'mock failure'); END`); err != nil {
		t.Fatal(err)
	}
	send("PUT", "/api/download-settings", `{"mode":"local","publicOrigin":"https://guludrive.example.invalid","workerURL":""}`, true, 500, nil)
	var savedMode string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key='download.mode'`).Scan(&savedMode); err != nil || savedMode != "proxy" || s.gdirect.Status().Enabled {
		t.Fatal("failed settings transaction partially changed persistent or runtime state", err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_download_setting`); err != nil {
		t.Fatal(err)
	}
	stillConfigured := send("GET", "/api/download-settings", "", true, 200, nil)
	if !strings.Contains(stillConfigured.Body.String(), `"workerConfigured":true`) {
		t.Fatal("failed settings transaction changed the public view")
	}
	// Configured Worker state must not turn local mode into a hidden proxy.
	send("PUT", "/api/download-settings", `{"mode":"local","publicOrigin":"https://guludrive.example.invalid","workerURL":"https://downloads.example.invalid/"}`, true, 200, nil)
	local := send("POST", "/api/download-links", `{"path":"/bucket/file.bin"}`, true, 200, nil)
	if json.Unmarshal(local.Body.Bytes(), &link) != nil || link.Mode != "local" || filesystem.prepares.Load() != manifestPrepares {
		t.Fatal("local mode must ignore retained Worker configuration")
	}
	send("PUT", "/api/download-settings", `{"mode":"proxy","publicOrigin":"https://guludrive.example.invalid","workerURL":""}`, true, 400, nil)
	send("PUT", "/api/download-settings", `{"mode":"worker","publicOrigin":"https://guludrive.example.invalid","workerURL":""}`, true, 400, nil)
	send("PUT", "/api/download-settings", `{"publicOrigin":"https://guludrive.example.invalid","workerURL":""}`, true, 400, nil)
	send("PUT", "/api/download-settings", `{"mode":"local"}`, true, 400, nil)
	send("PUT", "/api/download-settings", `{"enabled":true,"publicOrigin":"https://guludrive.example.invalid","workerURL":""}`, true, 400, nil)
	send("POST", "/api/download-links", `{"path":"/bucket/file.bin","mode":"proxy"}`, true, 400, nil)
	send("POST", "/api/gdirect/refresh", "", true, 409, nil)
	send("POST", "/api/gdirect/refresh", "", false, 401, nil)
	send("POST", "/api/gdirect/refresh", "", true, 403, map[string]string{"X-GuluDrive-Request": ""})

	router := s.gdirect.(*smokeRouter)
	router.refresh = func(ctx context.Context) (string, error) {
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var persisted string
		if err := s.db.QueryRowContext(probeCtx, `SELECT value FROM settings WHERE key='download.mode'`).Scan(&persisted); err != nil || persisted != "gdirect" {
			t.Error("selection ran before the settings transaction released SQL", err)
		}
		view := send("GET", "/api/download-settings", "", true, 200, nil)
		if !strings.Contains(view.Body.String(), `"mode":"gdirect"`) {
			t.Error("settings must remain observable while selecting")
		}
		return "", nil // No reachable candidate is still a successful scan.
	}
	send("PUT", "/api/download-settings", `{"mode":"gdirect","publicOrigin":"https://guludrive.example.invalid","workerURL":"https://downloads.example.invalid/"}`, true, 200, nil)
	if router.scans != 1 || !router.Status().Enabled {
		t.Fatal("selecting GDirect must scan once after committing the choice")
	}
	send("POST", "/api/download-links", `{"path":"/bucket/file.bin"}`, true, 503, nil)
	send("PUT", "/api/download-settings", `{"mode":"gdirect","publicOrigin":"https://guludrive.example.invalid","workerURL":"https://downloads.example.invalid/"}`, true, 200, nil)
	if router.scans != 1 {
		t.Fatal("unchanged mode must not silently rescan")
	}
	// Manual refresh bypasses an occupied namespace gate, not authentication.
	s.gate <- struct{}{}
	router.refresh = func(context.Context) (string, error) { return "", core.ErrBusy }
	refreshed := send("POST", "/api/gdirect/refresh", "", true, 200, nil)
	<-s.gate
	if strings.TrimSpace(refreshed.Body.String()) != `{"enabled":true,"ip":"","selecting":false}` {
		t.Fatal("refresh must return status directly, including empty-IP success")
	}
	router.refresh = func(context.Context) (string, error) { return "192.0.2.1", nil }
	send("POST", "/api/gdirect/refresh", "", true, 200, nil)
	var storedIPs int
	if err := s.db.QueryRow(`SELECT count(*) FROM settings WHERE value='192.0.2.1'`).Scan(&storedIPs); err != nil || storedIPs != 0 {
		t.Fatal("selected IP must never be persisted", err)
	}
	generated := send("POST", "/api/download-links", `{"path":"/bucket/file.bin"}`, true, 200, nil)
	if json.Unmarshal(generated.Body.Bytes(), &link) != nil || link.Mode != "gdirect" || filesystem.prepares.Load() != manifestPrepares {
		t.Fatal("GDirect must issue server capabilities even with Worker configured")
	}
	directURL := link.URL
	directReq := httptest.NewRequest("GET", directURL, nil)
	if signed, ok := s.serverDownload(directReq); !ok || signed.Route != "gdirect" {
		t.Fatal("GDirect route must be signed into the server capability")
	}
	send("GET", localURL.String(), "", false, 200, nil)
	send("GET", directURL, "", false, 200, nil)
	send("PUT", "/api/download-settings", `{"mode":"local","publicOrigin":"https://guludrive.example.invalid","workerURL":""}`, true, 200, nil)
	notReady := send("GET", directURL, "", false, 503, nil)
	if !strings.Contains(notReady.Body.String(), `"code":"gdirect_not_ready"`) {
		t.Fatal("old direct capabilities must fail explicitly when disabled")
	}
	send("GET", localURL.String(), "", false, 200, nil)

	unsignedRoute := serverDownload{V: 1, Mode: "server", Path: filePath, ID: entry.ID, Version: entry.Version, Exp: time.Now().Add(time.Minute).Unix()}
	for _, route := range []string{"", "proxy"} {
		unsignedRoute.Route = route
		parameters, err := downloadParameters(unsignedRoute, s.downloads.key)
		if err != nil {
			t.Fatal(err)
		}
		send("GET", "http://guludrive.example.invalid/download?"+parameters.Encode(), "", false, 403, nil)
	}
}

func TestUploadSettingsAndUnlimitedInput(t *testing.T) {
	filesystem := &smokeFS{files: map[string]smokeFile{"/bucket": {entry: core.Entry{ID: 1, Path: "/bucket", Name: "bucket", IsDir: true}}}, nextID: 2}
	s, err := newTestServer(t, Config{}, filesystem, smokeAccounts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if w := administratorRequest(s.Handler(), "GET", "/api/upload-settings", "", nil); w.Code != 401 {
		t.Fatal("upload settings bypassed session guard")
	}
	login := administratorRequest(s.Handler(), "POST", "/api/session", `{"username":"admin","password":"test-password"}`, nil)
	cookie := login.Result().Cookies()[0]
	request := func(method, target, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := administratorRequest(s.Handler(), method, target, body, cookie)
		if w.Code != want {
			t.Fatalf("%s %s: got %d, want %d: %s", method, target, w.Code, want, w.Body.String())
		}
		return w
	}
	var settings core.UploadSettings
	defaults := request("GET", "/api/upload-settings", "", 200)
	if json.Unmarshal(defaults.Body.Bytes(), &settings) != nil || settings != media.DefaultUploadSettings() {
		t.Fatal("missing preferences must use media defaults")
	}
	r := httptest.NewRequest("PUT", "http://guludrive.test/api/upload-settings", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(cookie)
	csrf := httptest.NewRecorder()
	s.Handler().ServeHTTP(csrf, r)
	if csrf.Code != 403 {
		t.Fatal("upload settings bypassed CSRF guard")
	}
	request("PUT", "/api/upload-settings", `{"largeFileFormat":"mp4","mp4PartSize":1,"bmpPartSize":100000000}`, 400)
	desired := core.UploadSettings{LargeFileFormat: "bmp", MP4PartSize: 1_000_000_000, BMPPartSize: 100_000_000}
	request("PUT", "/api/upload-settings", `{"largeFileFormat":"bmp","mp4PartSize":1000000000,"bmpPartSize":100000000}`, 200)
	reloaded := &uploadSettings{db: s.db}
	if settings, err := reloaded.Get(context.Background()); err != nil || settings != desired {
		t.Fatal("upload preferences were not persisted", err)
	}
	if policy, err := reloaded.Policy(context.Background()); err != nil || policy != (core.UploadPolicy{Format: "bmp", PartSize: 100_000_000}) {
		t.Fatal("store policy did not use the chosen complete-carrier budget", err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_upload_setting BEFORE UPDATE ON settings WHEN NEW.key='upload.bmp_part_size' BEGIN SELECT RAISE(ABORT,'mock failure'); END`); err != nil {
		t.Fatal(err)
	}
	request("PUT", "/api/upload-settings", `{"largeFileFormat":"mp4","mp4PartSize":2000000000,"bmpPartSize":150000000}`, 500)
	if settings, err := reloaded.Get(context.Background()); err != nil || settings != desired {
		t.Fatal("failed upload preferences transaction partially committed", err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_upload_setting; UPDATE settings SET value='malformed-private-value' WHERE key='upload.bmp_part_size'`); err != nil {
		t.Fatal(err)
	}
	failure := request("GET", "/api/upload-settings", "", 500)
	if strings.Contains(failure.Body.String(), "malformed-private-value") {
		t.Fatal("persisted settings error leaked raw data")
	}
	var malformed string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key='upload.bmp_part_size'`).Scan(&malformed); err != nil || malformed != "malformed-private-value" {
		t.Fatal("malformed persisted preferences were silently reset", err)
	}
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key='upload.bmp_part_size'`); err != nil {
		t.Fatal(err)
	}
	if settings, err := reloaded.Get(context.Background()); err != nil || settings.LargeFileFormat != "bmp" || settings.BMPPartSize != media.DefaultUploadSettings().BMPPartSize {
		t.Fatal("only missing preferences should use defaults", err)
	}

	// Zero and explicit MaxInt64 must never wrap a reader with max+1. Unknown
	// length uploads exercise both the shared copier and each transport wrapper.
	if s.maxFile != math.MaxInt64 {
		t.Fatal("zero config must disable the HTTP upload limiter")
	}
	for _, limit := range []int64{s.maxFile, media.MaxPayloadSize + 1} {
		s.maxFile = limit
		for _, api := range []bool{true, false} {
			target := "/dav/bucket/stream.bin"
			if api {
				target = "/api/files?path=%2Fbucket%2Fstream.bin"
			}
			r := httptest.NewRequest("PUT", "http://guludrive.test"+target, strings.NewReader("complete-stream"))
			r.ContentLength = -1
			r.SetBasicAuth("admin", "test-password")
			r.AddCookie(cookie)
			r.Header.Set("X-GuluDrive-Request", "1")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 200 && w.Code != 201 && w.Code != 204 {
				t.Fatalf("unlimited upload failed: %d %s", w.Code, w.Body.String())
			}
			if !bytes.Equal(filesystem.files["/bucket/stream.bin"].data, []byte("complete-stream")) {
				t.Fatal("unlimited input was truncated by a wrapping limit")
			}
		}
	}
	s.maxFile = 3
	request("PUT", "/api/files?path=%2Fbucket%2Fstream.bin", "long", 413)
	for _, target := range []string{"/api/files?path=%2Fbucket%2Fstream.bin", "/dav/bucket/stream.bin"} {
		r := httptest.NewRequest("PUT", "http://guludrive.test"+target, strings.NewReader("long"))
		r.ContentLength = -1
		r.AddCookie(cookie)
		r.SetBasicAuth("admin", "test-password")
		r.Header.Set("X-GuluDrive-Request", "1")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 413 {
			t.Fatalf("unknown-length input bypassed the configured cap: %d", w.Code)
		}
	}
	if !bytes.Equal(filesystem.files["/bucket/stream.bin"].data, []byte("complete-stream")) {
		t.Fatal("configured limit failure overwrote the old file")
	}
}

func TestProxyOriginAndManifestBounds(t *testing.T) {
	for _, origin := range []string{"", "https://localhost", "https://test.localhost.", "https://127.0.0.1", "https://127.1", "https://0x7f000001", "https://10.1.2.3", "https://[::1]", "https://[::]", "https://[fc00::1]"} {
		if publicManifestOrigin(origin) != "" {
			t.Fatalf("unsafe public origin accepted: %s", origin)
		}
	}
	if publicManifestOrigin("https://public.example.invalid:443") != "https://public.example.invalid" {
		t.Fatal("public HTTPS default port was rejected")
	}
	const p = "/bucket/file.bin"
	entry := core.Entry{ID: 4, Version: 1, Path: p, Name: "file.bin", Size: maxWorkerInteger + 1, ModTime: time.Unix(1700000000, 0).UTC()}
	filesystem := &smokeFS{files: map[string]smokeFile{p: {entry: entry}}}
	s, err := newTestServer(t, Config{}, filesystem, smokeAccounts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	login := administratorRequest(s.Handler(), "POST", "/api/session", `{"username":"admin","password":"test-password"}`, nil)
	cookie := login.Result().Cookies()[0]
	configured := administratorRequest(s.Handler(), "PUT", "/api/download-settings", `{"mode":"proxy","publicOrigin":"","workerURL":"https://worker.example.invalid/","workerSecret":"offline-secret-012345678901234567890123456789"}`, cookie)
	if configured.Code != 200 || !strings.Contains(configured.Body.String(), `"publicOriginReady":false`) {
		t.Fatal("missing public origin must remain visible independently of saved Worker configuration")
	}
	noOrigin := administratorRequest(s.Handler(), "POST", "/api/download-links", `{"path":"/bucket/file.bin"}`, cookie)
	if noOrigin.Code != 422 || !strings.Contains(noOrigin.Body.String(), `"code":"proxy_origin_required"`) || filesystem.prepares.Load() != 0 {
		t.Fatal("missing public origin did not fail proxy issuance explicitly")
	}
	s.security.setPublicOrigin("https://public.example.invalid")
	unsupported := administratorRequest(s.Handler(), "POST", "/api/download-links", `{"path":"/bucket/file.bin"}`, cookie)
	if unsupported.Code != 422 || !strings.Contains(unsupported.Body.String(), `"code":"proxy_unsupported_metadata"`) {
		t.Fatal("Worker unsupported integers did not fail explicitly")
	}
	if _, _, err := s.commitDownloadSettings(context.Background(), "local", "https://worker.example.invalid/", "", ""); err != nil {
		t.Fatal(err)
	}
	local := administratorRequest(s.Handler(), "POST", "/api/download-links", `{"path":"/bucket/file.bin"}`, cookie)
	if local.Code != 200 || !strings.Contains(local.Body.String(), `"mode":"local"`) || filesystem.prepares.Load() != 0 {
		t.Fatal("Worker numeric bounds incorrectly limited local files")
	}
	entry.Size = 2200
	segments := make([]core.DownloadSegment, int(entry.Size))
	longURL := "https://video-downloads.googleusercontent.com/" + strings.Repeat("x", 8000)
	for i := range segments {
		segments[i] = core.DownloadSegment{Offset: int64(i), Size: 1, URL: longURL, PayloadOffset: 1, CarrierSize: 2}
	}
	if raw, err := encodeManifest(entry, segments, false); raw != nil || !errors.Is(err, errProxyManifestSize) {
		t.Fatal("metadata over 16 MiB was truncated or silently accepted", err)
	} else {
		w := httptest.NewRecorder()
		manifestFailure(w, err)
		if w.Code != 422 || !strings.Contains(w.Body.String(), `"code":"proxy_manifest_too_large"`) {
			t.Fatal("manifest resource limit did not produce an explicit failure")
		}
	}
	entry.Size = 2
	segments = []core.DownloadSegment{{Offset: 0, Size: 2, URL: "javascript:control", PayloadOffset: 1, CarrierSize: 3}}
	if _, err := encodeManifest(entry, segments, false); !errors.Is(err, errProxyManifest) {
		t.Fatal("control URL was emitted")
	}
}

func TestDownloadModeStartup(t *testing.T) {
	s, err := newTestServer(t, Config{}, &smokeFS{}, smokeAccounts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	const secret = "GuluDrive-FAKE-test-secret-0123456789abcdef"
	for _, item := range []struct{ key, value string }{{"gdirect.enabled", "true"}, {workerURLKey, "https://downloads.example.invalid/"}, {workerSecretKey, secret}} {
		if _, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?)`, item.key, item.value); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := loadDownloads(context.Background(), s.db)
	if err != nil || loaded.settings.mode != "local" || !loaded.settings.view().WorkerConfigured || loaded.settings.workerSecret != secret || !bytes.Equal(loaded.key, s.downloads.key) {
		t.Fatal("a missing mode must default to local without importing the old toggle or losing secrets", err)
	}
	s.downloads = loaded
	var oldToggle string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key='gdirect.enabled'`).Scan(&oldToggle); err != nil || oldToggle != "true" {
		t.Fatal("reading settings must not rewrite old state", err)
	}
	// Only a successful settings commit removes the old toggle.
	loggedIn := administratorRequest(s.Handler(), "POST", "/api/session", `{"username":"admin","password":"test-password"}`, nil)
	cookie := loggedIn.Result().Cookies()[0]
	if w := administratorRequest(s.Handler(), "PUT", "/api/download-settings", `{"mode":"local","publicOrigin":"","workerURL":""}`, cookie); w.Code != 200 {
		t.Fatal("cannot commit explicit local mode")
	}
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key='gdirect.enabled'`).Scan(&oldToggle); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("committed settings must remove the unused old toggle", err)
	}
	if _, err := s.db.Exec(`UPDATE settings SET value='gdirect' WHERE key='download.mode'`); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadDownloads(context.Background(), s.db)
	if err != nil || loaded.settings.mode != "gdirect" {
		t.Fatal("explicit mode must survive a fresh settings load", err)
	}
	s.downloads = loaded
	router := s.gdirect.(*smokeRouter)
	router.SetEnabled(true)
	started, stopped := make(chan struct{}), make(chan struct{})
	router.refresh = func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return "", ctx.Err()
	}
	s.startGDirect() // Must return before selection finishes.
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup did not start a fresh asynchronous selection")
	}
	if status := router.Status(); !status.Enabled || status.IP != "" || !status.Selecting {
		t.Fatal("startup must expose selecting, not restore a persisted IP")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal("shutdown did not cancel and drain startup selection", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("startup selection outlived shutdown")
	}
}

func newTestServer(t *testing.T, cfg Config, filesystem core.FileSystem, accounts core.Accounts, db *sql.DB) (*Server, error) {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	if db == nil {
		var err error
		db, err = database.Open(context.Background(), cfg.DataDir)
		if err != nil {
			return nil, err
		}
	}
	admin, err := loadAdministrator(context.Background(), db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s, err := newServer(context.Background(), cfg, filesystem, accounts, admin)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	downloads, err := loadDownloads(context.Background(), db)
	if err != nil {
		_ = s.Close(context.Background())
		_ = db.Close()
		return nil, err
	}
	router := &smokeRouter{}
	router.SetEnabled(downloads.settings.mode == "gdirect")
	s.db, s.downloads, s.gdirect, s.uploads = db, downloads, router, &uploadSettings{db: db}
	if !s.security.initialized() {
		if err = s.initializeAdministrator(context.Background(), "admin", "test-password"); err != nil {
			_ = s.Close(context.Background())
			return nil, err
		}
	}
	return s, nil
}

type smokeRouter struct {
	mu      sync.Mutex
	status  core.GDirectStatus
	scans   int
	refresh func(context.Context) (string, error)
}

func (r *smokeRouter) SetEnabled(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.Enabled != enabled {
		r.status = core.GDirectStatus{Enabled: enabled}
	}
}

func (r *smokeRouter) Status() core.GDirectStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *smokeRouter) Refresh(ctx context.Context) (core.GDirectStatus, error) {
	r.mu.Lock()
	r.scans++
	r.status.Selecting = true
	refresh := r.refresh
	r.mu.Unlock()
	var ip string
	var err error
	if refresh != nil {
		ip, err = refresh(ctx)
	}
	r.mu.Lock()
	if r.status.Enabled {
		r.status.IP = ip
	}
	r.status.Selecting = false
	status := r.status
	r.mu.Unlock()
	return status, err
}

// Only the operations exercised by this transport smoke test are implemented.
// The embedded interface makes unexpected extra service calls fail loudly.
type smokeFS struct {
	core.FileSystem
	mu                               sync.Mutex
	files                            map[string]smokeFile
	buckets                          []core.Bucket
	nextID                           int64
	segments                         []core.DownloadSegment
	prepareEntry                     *core.Entry
	prepareErr                       error
	opens, prepares, commits, aborts atomic.Int64
}
type smokeFile struct {
	entry core.Entry
	data  []byte
}

func (f *smokeFS) ListBuckets(context.Context) ([]core.Bucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Bucket(nil), f.buckets...), nil
}
func (f *smokeFS) Stat(_ context.Context, p string) (core.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if value, ok := f.files[p]; ok {
		return value.entry, nil
	}
	return core.Entry{}, core.ErrNotFound
}
func (f *smokeFS) ReadDir(_ context.Context, p string) ([]core.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var entries []core.Entry
	for name, value := range f.files {
		if name != p && path.Dir(name) == p {
			entries = append(entries, value.entry)
		}
	}
	return entries, nil
}
func (f *smokeFS) Open(ctx context.Context, p string) (io.ReadSeekCloser, core.Entry, error) {
	f.opens.Add(1)
	entry, err := f.Stat(ctx, p)
	if err != nil {
		return nil, entry, err
	}
	if entry.IsDir {
		return nil, entry, core.ErrIsDir
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return smokeReader{bytes.NewReader(f.files[p].data)}, entry, nil
}
func (f *smokeFS) PrepareDownload(ctx context.Context, p string) (core.DownloadSource, error) {
	f.prepares.Add(1)
	entry, err := f.Stat(ctx, p)
	if err != nil {
		return core.DownloadSource{}, err
	}
	if entry.IsDir {
		return core.DownloadSource{}, core.ErrIsDir
	}
	if f.prepareErr != nil {
		return core.DownloadSource{}, f.prepareErr
	}
	if f.prepareEntry != nil {
		entry = *f.prepareEntry
	}
	segments := f.segments
	if segments == nil {
		segments = []core.DownloadSegment{{Offset: 0, Size: entry.Size, URL: "https://video-downloads.googleusercontent.com/synthetic-carrier?fixture=1", PayloadOffset: 12, CarrierSize: 12 + entry.Size}}
	}
	return core.DownloadSource{Entry: entry, Segments: segments}, nil
}
func (f *smokeFS) BeginWrite(_ context.Context, p string, opts core.WriteOptions) (core.WriteSession, error) {
	return &smokeWrite{fs: f, path: p, opts: opts}, nil
}
func (f *smokeFS) Move(_ context.Context, src, dst string, overwrite bool) (core.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.files[src]
	if !ok {
		return core.Entry{}, core.ErrNotFound
	}
	if _, exists := f.files[dst]; exists && !overwrite {
		return core.Entry{}, core.ErrExists
	}
	delete(f.files, src)
	value.entry.Path, value.entry.Name = dst, path.Base(dst)
	value.entry.Version++
	f.files[dst] = value
	return value.entry, nil
}
func (f *smokeFS) Remove(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, p)
	return nil
}
func (f *smokeFS) Close() error { return nil }

type smokeReader struct{ *bytes.Reader }

func (smokeReader) Close() error { return nil }

type smokeWrite struct {
	bytes.Buffer
	fs   *smokeFS
	path string
	opts core.WriteOptions
}

func (w *smokeWrite) Abort() error { w.fs.aborts.Add(1); return nil }
func (w *smokeWrite) Commit(ctx context.Context) (core.Entry, error) {
	w.fs.commits.Add(1)
	if err := ctx.Err(); err != nil {
		return core.Entry{}, err
	}
	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	old, exists := w.fs.files[w.path]
	if (w.opts.IfNoneMatch && exists) || (w.opts.ExpectedID != nil && (!exists || old.entry.ID != *w.opts.ExpectedID || old.entry.Version != *w.opts.ExpectedVersion)) {
		return core.Entry{}, core.ErrPrecondition
	}
	entry := core.Entry{ID: w.fs.nextID, Path: w.path, Name: path.Base(w.path), Size: int64(w.Len()), Version: 1, ModTime: time.Now(), ContentType: w.opts.ContentType}
	w.fs.nextID++
	w.fs.files[w.path] = smokeFile{entry: entry, data: bytes.Clone(w.Bytes())}
	return entry, nil
}

type smokeAccounts struct{}

func (smokeAccounts) Accounts(context.Context) ([]core.Account, error) { return nil, nil }
func (smokeAccounts) Login(context.Context, string) (core.Account, error) {
	return core.Account{}, core.ErrInvalid
}
func (smokeAccounts) Select(context.Context, string) error { return core.ErrInvalid }
func (smokeAccounts) Remove(context.Context, string) error { return core.ErrInvalid }

type failedBody struct{}

func (failedBody) Read(p []byte) (int, error) {
	return copy(p, "partial"), errors.New("test read failure")
}
func (failedBody) Close() error { return nil }
