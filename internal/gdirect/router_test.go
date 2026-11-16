package gdirect

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"guludrive/core"
)

const otherHost = "lh3.googleusercontent.com"

var testIP = netip.MustParseAddr("142.250.99.99")
var nextIP = netip.MustParseAddr("142.250.99.100")

type fixture struct {
	r            *Router
	server       *httptest.Server
	dns          atomic.Int32
	candidate    atomic.Value
	failSelected atomic.Bool
	mu           sync.Mutex
	snis, dials  []string
	closed       chan string
}

func newFixture(t *testing.T, handler http.HandlerFunc) *fixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1),
		DNSNames:  []string{probeHost, otherHost, "photos.google.com", "photosdata-pa.googleapis.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	f := &fixture{closed: make(chan string, 128)}
	f.candidate.Store(testIP)
	f.server = httptest.NewUnstartedServer(handler)
	f.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			f.closed <- conn.RemoteAddr().String()
		}
	}
	f.server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			f.mu.Lock()
			f.snis = append(f.snis, hello.ServerName)
			f.mu.Unlock()
			return nil, nil
		}}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	f.r, err = New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.r.Close() })
	f.r.normal.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	f.r.normal.TLSClientConfig.RootCAs = roots // Test CA only; production uses system roots.
	f.r.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host != probeHost {
			t.Errorf("unexpected lookup of %q", host)
		}
		f.dns.Add(1)
		return []netip.Addr{f.candidate.Load().(netip.Addr)}, nil
	}
	f.r.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		f.mu.Lock()
		f.dials = append(f.dials, addr)
		f.mu.Unlock()
		host, _, _ := net.SplitHostPort(addr)
		ip, _ := netip.ParseAddr(host)
		if ip.IsValid() {
			if f.failSelected.Load() || (ip != testIP && ip != nextIP) {
				return nil, errors.New("private mock dial diagnostic")
			}
		} else if host != probeHost && host != otherHost && host != "photos.google.com" && host != "photosdata-pa.googleapis.com" && host != "wrong.googleusercontent.com" {
			return nil, errors.New("unmocked destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, f.server.Listener.Addr().String())
	}
	return f
}

func request(t *testing.T, ctx context.Context, method, host, path string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, "https://"+host+path, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func readResponse(t *testing.T, client *http.Client, req *http.Request) {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func refresh(t *testing.T, r *Router, ip netip.Addr) {
	t.Helper()
	status, err := r.Refresh(context.Background())
	if err != nil || status != (Status{Enabled: true, IP: ip.String()}) {
		t.Fatalf("refresh: %+v, %v", status, err)
	}
}

func TestExplicitRefreshAndSharedSelection(t *testing.T) {
	var requests atomic.Int32
	f := newFixture(t, func(w http.ResponseWriter, req *http.Request) {
		host := req.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if req.TLS.ServerName != host {
			t.Error("HTTP Host and real TLS SNI disagree")
		}
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	ctx := WithDirect(context.Background(), true)
	if f.r.Status() != (Status{}) || f.r.Client().Timeout != 0 {
		t.Fatal("unexpected constructor state")
	}
	if _, err := f.r.RoundTrip(request(t, ctx, "GET", probeHost, "/", nil)); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("disabled direct routing must be rejected")
	}
	f.r.SetEnabled(true)
	if _, err := f.r.RoundTrip(request(t, ctx, "GET", probeHost, "/", nil)); !errors.Is(err, core.ErrBusy) {
		t.Fatal("request must not select an IP")
	}
	for _, host := range []string{probeHost, "photosdata-pa.googleapis.com"} {
		readResponse(t, f.r.Client(), request(t, context.Background(), "POST", host, "/", strings.NewReader("rpc")))
	}
	if f.dns.Load() != 0 || f.r.Status().IP != "" {
		t.Fatal("constructor, mode switch or ordinary requests triggered selection")
	}
	before := requests.Load()
	refresh(t, f.r, testIP)
	if requests.Load() != before {
		t.Fatal("TLS probe sent HTTP")
	}
	f.r.normal.CloseIdleConnections()
	f.mu.Lock()
	if len(f.snis) == 0 || f.snis[len(f.snis)-1] != probeHost {
		t.Error("probe did not verify the real video hostname")
	}
	f.dials = nil
	f.mu.Unlock()
	readResponse(t, f.r.Client(), request(t, ctx, "GET", otherHost, "/", nil))
	readResponse(t, f.r.Client(), request(t, WithDirect(ctx, false), "GET", otherHost, "/", nil))
	readResponse(t, f.r.Client(), request(t, ctx, "POST", "photosdata-pa.googleapis.com", "/", strings.NewReader("rpc")))
	readResponse(t, f.r.Client(), request(t, ctx, "GET", otherHost+":444", "/", nil))
	f.mu.Lock()
	got := strings.Join(f.dials, ",")
	f.mu.Unlock()
	if got != testIP.String()+":443,"+otherHost+":443,photosdata-pa.googleapis.com:443,"+otherHost+":444" || f.dns.Load() != 1 {
		t.Fatalf("unexpected routes or implicit selection: %s / %d lookups", got, f.dns.Load())
	}
	fresh, _ := New("")
	defer fresh.Close()
	fresh.SetEnabled(true)
	if fresh.Status() != (Status{Enabled: true}) || fresh.normal.TLSClientConfig.InsecureSkipVerify || fresh.normal.TLSClientConfig.RootCAs != nil {
		t.Fatal("selection persisted or production TLS verification weakened")
	}
	for _, host := range []string{probeHost, otherHost, "googleusercontent.com", "ggpht.com", "photos.google.com"} {
		if !googleHost(host) {
			t.Errorf("missing content host %q", host)
		}
	}
	for _, host := range []string{"accounts.google.com", "photosdata-pa.googleapis.com", "photos.googleapis.com", "evilgoogleusercontent.com", "photos.google.com.evil.test"} {
		if googleHost(host) {
			t.Errorf("non-content host %q allowed", host)
		}
	}
}

func TestTLSVerificationAndFailedRefresh(t *testing.T) {
	f := newFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid TLS reached HTTP") })
	f.r.SetEnabled(true)
	f.r.normal.TLSClientConfig.RootCAs = x509.NewCertPool()
	if status, err := f.r.Refresh(context.Background()); !errors.Is(err, core.ErrBusy) || status.IP != "" {
		t.Fatal("untrusted candidate accepted")
	}
	f = newFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("wrong hostname reached HTTP") })
	f.r.SetEnabled(true)
	refresh(t, f.r, testIP)
	for _, direct := range []bool{false, true} {
		req := request(t, WithDirect(context.Background(), direct), "GET", "wrong.googleusercontent.com", "/", nil)
		if _, err := f.r.Client().Do(req); err == nil {
			t.Fatal("hostname verification bypassed")
		}
	}
	f.failSelected.Store(true)
	status, err := f.r.Refresh(context.Background())
	if !errors.Is(err, core.ErrBusy) || status != (Status{Enabled: true}) || strings.Contains(err.Error(), "private") {
		t.Fatalf("failed refresh retained IP or exposed error: %+v, %v", status, err)
	}
}

func TestBoundedSelection(t *testing.T) {
	r, _ := New("")
	defer r.Close()
	r.SetEnabled(true)
	r.lookup = func(context.Context, string) ([]netip.Addr, error) {
		answers := make([]netip.Addr, 100)
		for i := range answers {
			answers[i] = netip.AddrFrom4([4]byte{142, 250, 100, byte(i + 1)})
		}
		return answers, nil
	}
	var active, peak, calls atomic.Int32
	started, release := make(chan struct{}, probeConcurrency), make(chan struct{})
	r.dial = func(ctx context.Context, _, addr string) (net.Conn, error) {
		ip, _, _ := net.SplitHostPort(addr)
		if !googleIP(netip.MustParseAddr(ip)) {
			t.Error("non-Google candidate")
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		if calls.Add(1) <= probeConcurrency {
			started <- struct{}{}
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, errors.New("mock unavailable")
	}
	done := make(chan error, 1)
	go func() { _, err := r.Refresh(context.Background()); done <- err }()
	for range probeConcurrency {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("no probe")
		}
	}
	if !r.Status().Selecting {
		t.Fatal("selecting not published immediately")
	}
	if _, err := r.Refresh(context.Background()); !errors.Is(err, core.ErrBusy) {
		t.Fatal("concurrent refresh was not rejected")
	}
	close(release)
	if err := <-done; !errors.Is(err, core.ErrBusy) || calls.Load() != maxCandidates || peak.Load() > probeConcurrency || active.Load() != 0 || r.Status().Selecting {
		t.Fatalf("unbounded/leaked probes: %d calls, %d peak, %d active, %v", calls.Load(), peak.Load(), active.Load(), err)
	}
}

type countedBody struct{ reads, closes atomic.Int32 }

func (b *countedBody) Read([]byte) (int, error) { b.reads.Add(1); return 0, io.EOF }
func (b *countedBody) Close() error             { b.closes.Add(1); return nil }

func TestCancelDisableAndCloseDrainProbes(t *testing.T) {
	for _, action := range []string{"cancel", "disable", "close"} {
		t.Run(action, func(t *testing.T) {
			r, _ := New("")
			defer r.Close()
			r.lookup = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{testIP}, nil }
			var active, calls atomic.Int32
			started := make(chan struct{}, probeConcurrency)
			r.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
				active.Add(1)
				defer active.Add(-1)
				calls.Add(1)
				started <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			}
			r.SetEnabled(true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := r.Refresh(ctx); done <- err }()
			for range probeConcurrency {
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("no probe")
				}
			}
			body := &countedBody{}
			_, err := r.RoundTrip(request(t, WithDirect(ctx, true), "POST", probeHost, "/", body))
			if !errors.Is(err, core.ErrBusy) || calls.Load() != probeConcurrency || body.reads.Load() != 0 || body.closes.Load() != 1 {
				t.Fatal("request probed, blocked or consumed body during refresh")
			}
			switch action {
			case "cancel":
				cancel()
			case "disable":
				r.SetEnabled(false)
			case "close":
				_ = r.Close()
			}
			select {
			case err := <-done:
				if !errors.Is(err, core.ErrBusy) || active.Load() != 0 || r.Status().Selecting || r.Status().IP != "" {
					t.Fatal("canceled selection leaked probes/state or raw cancellation")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("canceled selection leaked")
			}
		})
	}
}

func TestDirectFailureDoesNotFallbackOrReplay(t *testing.T) {
	var posts atomic.Int32
	f := newFixture(t, func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		posts.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close() // The mutation arrived but its outcome is unknown.
	})
	f.r.SetEnabled(true)
	refresh(t, f.r, testIP)
	ctx := WithDirect(context.Background(), true)
	req := request(t, ctx, "POST", otherHost, "/", strings.NewReader("payload"))
	if _, err := f.r.Client().Do(req); !errors.Is(err, core.ErrBusy) || posts.Load() != 1 || f.dns.Load() != 1 {
		t.Fatal("failed POST replayed or triggered selection")
	}
	f.failSelected.Store(true)
	f.mu.Lock()
	f.dials = nil
	f.mu.Unlock()
	req = request(t, ctx, "POST", otherHost, "/", strings.NewReader("payload"))
	if _, err := f.r.Client().Do(req); !errors.Is(err, core.ErrBusy) || strings.Contains(err.Error(), "private") {
		t.Fatal("dial failure was not safely returned")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.dials, ",") != testIP.String()+":443" || posts.Load() != 1 || f.dns.Load() != 1 {
		t.Fatal("failed selected dial fell back, replayed, or reselected")
	}
}

func TestRefreshRotatesIdlePoolAndPreservesStreams(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	idleAddr := make(chan string, 1)
	var requests atomic.Int32
	f := newFixture(t, func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if req.URL.Path == "/stream" {
			_, _ = io.WriteString(w, "a")
			w.(http.Flusher).Flush()
			<-release
			_, _ = io.WriteString(w, "b")
		} else {
			if req.URL.Path == "/idle" {
				idleAddr <- req.RemoteAddr
			}
			w.WriteHeader(http.StatusNoContent)
		}
	})
	f.r.SetEnabled(true)
	refresh(t, f.r, testIP)
	ctx := WithDirect(context.Background(), true)
	resp, err := f.r.Client().Do(request(t, ctx, "GET", probeHost, "/stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "a" {
		t.Fatal("stream did not start")
	}
	readResponse(t, f.r.Client(), request(t, ctx, "GET", probeHost, "/idle", nil))
	idle, old := <-idleAddr, f.r.direct
	f.candidate.Store(nextIP)
	refresh(t, f.r, nextIP)
	if f.r.direct == old {
		t.Fatal("pool was not replaced")
	}
	deadline := time.After(2 * time.Second)
closedIdle:
	for {
		select {
		case addr := <-f.closed:
			if addr == idle {
				break closedIdle
			}
		case <-deadline:
			t.Fatal("old idle connection stayed open")
		}
	}
	f.mu.Lock()
	f.dials = nil
	f.mu.Unlock()
	readResponse(t, f.r.Client(), request(t, ctx, "GET", probeHost, "/next", nil))
	f.mu.Lock()
	got := strings.Join(f.dials, ",")
	f.mu.Unlock()
	if got != nextIP.String()+":443" || f.dns.Load() != 2 || requests.Load() != 3 {
		t.Fatal("request used stale selection, triggered probes or was replayed")
	}
	f.r.SetEnabled(false)
	_ = f.r.Close()
	release <- struct{}{}
	remaining, err := io.ReadAll(resp.Body)
	if err != nil || string(remaining) != "b" {
		t.Fatalf("refresh/disable/close interrupted the active stream: %q / %v", remaining, err)
	}
}

func TestNormalProxiesAndExplicitDirectBypass(t *testing.T) {
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "CONNECT" || req.Host != probeHost+":443" {
			t.Error("incorrect proxy destination")
		}
		connects.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", "http://env-proxy.test:8080")
	t.Setenv("https_proxy", "http://env-proxy.test:8080")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	for _, explicit := range []bool{true, false} {
		f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		expected := "env-proxy.test:8080"
		f.r.normal.Proxy = http.ProxyFromEnvironment
		if explicit {
			u, _ := url.Parse(proxy.URL)
			f.r.normal.Proxy = http.ProxyURL(u)
			expected = u.Host
		}
		dial := f.r.dial
		f.r.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == expected {
				return (&net.Dialer{}).DialContext(ctx, network, proxy.Listener.Addr().String())
			}
			return dial(ctx, network, addr)
		}
		f.r.SetEnabled(true)
		req := request(t, context.Background(), "GET", probeHost, "/", nil)
		if _, err := f.r.Client().Do(req); err == nil || f.dns.Load() != 0 {
			t.Fatal("normal request bypassed proxy or triggered selection")
		}
		refresh(t, f.r, testIP)
		readResponse(t, f.r.Client(), request(t, WithDirect(context.Background(), true), "GET", probeHost, "/", nil))
	}
	if connects.Load() != 2 {
		t.Fatal("explicit/environment proxy was bypassed by normal routing")
	}
}
