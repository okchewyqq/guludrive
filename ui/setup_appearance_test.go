package workspace

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"guludrive/ui/api"
)

type adminTestTransport func(*http.Request) (*http.Response, error)

func (f adminTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func applyAdminCallback(t *testing.T, callbacks <-chan func()) {
	t.Helper()
	select {
	case f := <-callbacks:
		f()
	case <-time.After(5 * time.Second):
		t.Fatal("native HTTP callback did not arrive")
	}
}

func TestNativeSetupLifecycle(t *testing.T) {
	routes := []string{}
	c, err := api.New("http://guludrive.test", &http.Client{Transport: adminTestTransport(func(r *http.Request) (*http.Response, error) {
		routes = append(routes, r.Method+" "+r.URL.Path)
		w := httptest.NewRecorder()
		body := `{}`
		switch r.URL.Path {
		case "/api/setup":
			body = `{"initialized":false}`
			if r.Method == "POST" {
				var in map[string]string
				json.NewDecoder(r.Body).Decode(&in)
				if len(in) != 2 || in["username"] != "admin" || in["password"] != "first-fake-password" {
					t.Error("setup sent confirmation or wrong credentials")
				}
				body = `{"initialized":true}`
			}
		case "/api/session":
			body = `{"ok":true}`
		case "/api/admin":
			body = `{"username":"admin"}`
		case "/api/download-settings":
			body = `{"mode":"local","workerURL":"","workerConfigured":false,"publicOrigin":"","publicOriginReady":false}`
		case "/api/accounts":
			body = `{"accounts":[]}`
		case "/api/buckets":
			body = `{"buckets":[]}`
		case "/api/files":
			body = `{"entry":{"path":"/"},"entries":[]}`
		case "/api/status":
		default:
			t.Error("unexpected setup request")
		}
		io.WriteString(w, body)
		return w.Result(), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	a := newUI(Options{})
	defer a.cancel()
	a.client = c
	callbacks := make(chan func(), 4)
	a.dispatch = func(f func()) { callbacks <- f }
	a.checkSetup()
	applyAdminCallback(t, callbacks)
	if !a.setupChecked || a.initialized || a.connected || len(routes) != 1 {
		t.Fatal("setup check performed login or skipped initialization")
	}
	a.password, a.confirmPassword = "first-fake-password", "wrong"
	a.connect()
	if a.busy || a.password != "" || a.confirmPassword != "" || len(routes) != 1 {
		t.Fatal("confirmation mismatch reached HTTP or retained secret")
	}
	a.password, a.confirmPassword = "first-fake-password", "first-fake-password"
	a.connect()
	a.connect()
	if a.password != "" || a.confirmPassword != "" {
		t.Fatal("setup password editors survived submission")
	}
	applyAdminCallback(t, callbacks)
	if !a.initialized || !a.connected || !reflect.DeepEqual(routes[:3], []string{"GET /api/setup", "POST /api/setup", "POST /api/session"}) {
		t.Fatal("setup did not immediately login", routes)
	}
	a.clearSession()
	a.setupChecked = false
	a.client = c
	a.checkSetup()
	a.clearSession()
	applyAdminCallback(t, callbacks)
	if a.setupChecked || a.client != nil || a.connected {
		t.Fatal("stale setup callback restored session state")
	}
}
