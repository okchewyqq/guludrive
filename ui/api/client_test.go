package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"guludrive/core"
	"guludrive/ui/i18n"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Test the wire contract without a listener or network connection.
func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	c, err := New("http://guludrive.test", &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		response := w.Result()
		response.Request = r
		return response, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCookieSessionAndFiles(t *testing.T) {
	const filename = "/资料/空 格 #?% +文件.txt"
	payload := []byte{0, 1, 0xff, 0xfe, '\n'}
	var copies, moves atomic.Int32
	var workerConfigured bool
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "" {
			t.Error("session must not use Authorization")
		}
		if r.Method != "GET" && r.Header.Get("X-GuluDrive-Request") != "1" {
			t.Error("missing request protection header")
		}
		if r.URL.Path == "/api/session" && r.Method == "POST" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "synthetic", Path: "/", HttpOnly: true})
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "synthetic" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
			return
		}
		switch r.URL.Path {
		case "/api/session":
			if r.Method != "DELETE" {
				t.Error("unexpected session method")
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Path: "/", MaxAge: -1, HttpOnly: true})
			w.WriteHeader(http.StatusNoContent)
		case "/api/status":
			_, _ = io.WriteString(w, `{"version":"test","googleReady":true,"gdirect":{"enabled":true,"ip":"203.0.113.20","selecting":false}}`)
		case "/api/files":
			if r.URL.Query().Get("path") != filename {
				t.Error("UTF-8 path changed")
			}
			if r.Method == "GET" {
				_, _ = io.WriteString(w, `{"entry":{"id":9007199254740993,"version":7,"path":"/资料"},"entries":[]}`)
				return
			}
			if r.Header.Get("If-Match") != `"9007199254740993-7"` {
				t.Error("missing shared entry validator")
			}
			got, _ := io.ReadAll(r.Body)
			if !bytes.Equal(got, payload) {
				t.Error("upload bytes changed")
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/download-links":
			var body struct {
				Path string `json:"path"`
			}
			if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Path != filename {
				t.Error("invalid download link request")
			}
			_, _ = io.WriteString(w, `{"url":"http://guludrive.test/api/download?token=synthetic-capability","expiresAt":"2099-01-01T00:00:00Z","mode":"local"}`)
		case "/api/download-settings":
			if r.Method == "PUT" {
				var input DownloadSettingsUpdate
				if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&input) != nil || input.Mode != DownloadProxy || input.WorkerURL != "https://worker.example" {
					t.Error("invalid settings request")
				}
				if input.WorkerSecret != "" {
					workerConfigured = true
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			fmt.Fprintf(w, `{"mode":"proxy","workerURL":"https://worker.example","workerConfigured":%t,"publicOrigin":"http://origin.example:8787","publicOriginReady":true}`, workerConfigured)
		case "/api/copy":
			copies.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"private backend prose","code":"outcome_uncertain","operation":"commit_upload","stage":"read_response","upstreamStatus":502,"reason":"network_error"}`)
		case "/api/move":
			moves.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"private backend prose","code":"remote_operation_failed","operation":"create_album","stage":"decode_album_id","upstreamStatus":200,"reason":"missing_album_id"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	ctx := context.Background()
	if err := c.Login(ctx, "admin", "synthetic-password"); err != nil {
		t.Fatal(err)
	}
	if status, err := c.Status(ctx); err != nil || !status.GoogleReady || status.Version != "test" || status.GDirect.IP != "203.0.113.20" {
		t.Fatalf("status: %+v, %v", status, err)
	}
	listing, err := c.Files(ctx, filename)
	if err != nil || listing.Entry.ID != 9007199254740993 {
		t.Fatalf("listing: %+v, %v", listing, err)
	}
	if err := c.Upload(ctx, filename, bytes.NewReader(payload), int64(len(payload)), Preconditions{IfMatch: listing.Entry.ETag()}); err != nil {
		t.Fatal(err)
	}
	link, err := c.CreateDownloadLink(ctx, filename)
	if err != nil || link.Mode != DownloadLocal || link.URL != "http://guludrive.test/api/download?token=synthetic-capability" || link.ExpiresAt.IsZero() {
		t.Fatal("download link contract failed")
	}
	for _, secret := range []string{strings.Repeat("密", 12), ""} {
		settings, err := c.UpdateDownloadSettings(ctx, DownloadSettingsUpdate{Mode: DownloadProxy, WorkerURL: "https://worker.example", WorkerSecret: secret})
		if err != nil || !settings.WorkerConfigured || settings.Mode != DownloadProxy {
			t.Fatal("settings or retained secret contract failed")
		}
	}
	if err := c.Copy(ctx, Transfer{Source: filename, Destination: "/资料/copy.txt"}); !errors.Is(err, core.ErrUncertain) || Unauthorized(err) || !strings.Contains(err.Error(), "Commit upload") || !strings.Contains(err.Error(), "Read response") || !strings.Contains(err.Error(), "HTTP 502") || !strings.Contains(err.Error(), "Do not retry automatically") || strings.Contains(err.Error(), "private backend prose") {
		t.Fatalf("uncertain outcome diagnostics lost: %v", err)
	}
	if err := c.Move(ctx, Transfer{Source: filename, Destination: "/资料/move.txt"}); err == nil || errors.Is(err, core.ErrUncertain) || Unauthorized(err) || !strings.Contains(i18n.ErrorText(i18n.Chinese, err), "创建存储桶") || !strings.Contains(err.Error(), "Read album ID") || !strings.Contains(err.Error(), "HTTP 200") || strings.Contains(err.Error(), "private backend prose") {
		t.Fatalf("definite remote failure diagnostics lost: %v", err)
	}
	if copies.Load() != 1 || moves.Load() != 1 {
		t.Fatal("remote failures were retried automatically")
	}
	if err := c.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(ctx); !Unauthorized(err) {
		t.Fatalf("logout did not expire the cookie: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Status(ctx); err != context.Canceled {
		t.Fatalf("cancellation lost: %v", err)
	}
	if err := c.Login(ctx, "admin", "synthetic-password"); err != context.Canceled {
		t.Fatalf("secret redaction lost cancellation: %v", err)
	}
}
