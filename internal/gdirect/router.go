// Package gdirect provides bounded, opt-in Google content routing. It is an
// independent implementation, not a port of the Google Drive gdirect project.
package gdirect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"guludrive/core"
)

// Status shares the service/UI contract without duplicating the status DTO.
type Status = core.GDirectStatus

type directKey struct{}
type selectionRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// WithDirect opts a content request into the current selected route. False
// explicitly keeps normal routing, even when inherited from a direct context.
// Authentication, RPC and uploads use normal routing unless explicitly opted in;
// non-content destinations always use normal routing.
func WithDirect(ctx context.Context, direct bool) context.Context {
	return context.WithValue(ctx, directKey{}, direct)
}

// Router owns independent normal and direct transports. Only Refresh selects an
// IP; requests never probe, reselect, or fall back after a direct routing failure.
// It has no settings, persistence, credentials or background refresh timers.
type Router struct {
	normal, direct *http.Transport
	lookup         func(context.Context, string) ([]netip.Addr, error)
	dial           func(context.Context, string, string) (net.Conn, error)
	mu             sync.Mutex
	status         Status
	run            *selectionRun
	generation     uint64
	closed         bool
}

var _ http.RoundTripper = (*Router)(nil)

// New is disabled by default and performs no network operations. Explicit
// proxies take precedence over environment proxies for normal traffic. An
// explicitly direct content request bypasses both kinds of HTTP proxy.
func New(proxyURL string) (*Router, error) {
	t := &http.Transport{
		Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:    100, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 2 * time.Minute, MaxResponseHeaderBytes: 1 << 20,
	}
	if proxyURL = strings.TrimSpace(proxyURL); proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return nil, errors.New("gdirect: invalid proxy configuration")
		}
		t.Proxy = http.ProxyURL(u)
	}
	r := &Router{normal: t}
	r.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	r.dial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return r.dial(ctx, network, addr)
	}
	return r, nil
}

// Client has no whole-body timeout, allowing streaming downloads. Photos' auth
// client supplies its own timeout; both clients can share this RoundTripper.
func (r *Router) Client() *http.Client { return &http.Client{Transport: r} }

func (r *Router) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// SetEnabled changes only runtime mode, never starting network activity. The
// caller explicitly Refreshes on startup, on entering GDirect mode, or manually.
// Disabling cancels and drains probes, without interrupting active file streams.
func (r *Router) SetEnabled(enabled bool) {
	r.mu.Lock()
	if r.closed || r.status.Enabled == enabled {
		r.mu.Unlock()
		return
	}
	r.generation++
	r.status = Status{Enabled: enabled}
	old, run := r.direct, r.run
	r.direct = nil
	r.mu.Unlock()
	if run != nil {
		run.cancel()
	}
	if old != nil {
		old.CloseIdleConnections()
	}
	if run != nil {
		<-run.done
	}
}

// Refresh verifies one shared, in-memory content IP against the real video CDN
// TLS hostname. Concurrent refreshes are rejected with ErrBusy. A failed
// selection clears the IP; errors contain only fixed text and shared sentinels.
func (r *Router) Refresh(ctx context.Context) (Status, error) {
	r.mu.Lock()
	if r.closed || !r.status.Enabled {
		status := r.status
		r.mu.Unlock()
		return status, fmt.Errorf("gdirect: direct routing disabled: %w", core.ErrUnsupported)
	}
	if r.run != nil {
		status := r.status
		r.mu.Unlock()
		return status, fmt.Errorf("gdirect: selection in progress: %w", core.ErrBusy)
	}
	probeCtx, cancel := context.WithTimeout(ctx, selectionTimeout)
	run := &selectionRun{cancel: cancel, done: make(chan struct{})}
	r.run, r.status.Selecting = run, true
	generation := r.generation
	r.mu.Unlock()

	ip := r.choose(probeCtx)
	canceled := probeCtx.Err() != nil
	cancel()
	r.mu.Lock()
	var old *http.Transport
	var err error
	if r.closed || !r.status.Enabled || generation != r.generation {
		err = fmt.Errorf("gdirect: selection canceled: %w", core.ErrBusy)
	} else {
		old, r.direct = r.direct, nil
		r.status.IP, r.status.Selecting = "", false
		if ip.IsValid() && !canceled {
			r.direct = r.newDirect(ip)
			r.status.IP = ip.String()
		} else {
			err = fmt.Errorf("gdirect: selection unavailable: %w", core.ErrBusy)
		}
	}
	r.run = nil
	status := r.status
	r.mu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
	close(run.done)
	return status, err
}

// Close cancels and drains selections and closes idle connections. Active
// response bodies remain owned by their callers and original request contexts.
func (r *Router) Close() error {
	r.mu.Lock()
	r.closed = true
	r.generation++
	r.status = Status{}
	old, run := r.direct, r.run
	r.direct = nil
	r.mu.Unlock()
	if run != nil {
		run.cancel()
	}
	r.normal.CloseIdleConnections()
	if old != nil {
		old.CloseIdleConnections()
	}
	if run != nil {
		<-run.done
	}
	return nil
}

func googleHost(host string) bool {
	return host == "photos.google.com" || host == "googleusercontent.com" || host == "ggpht.com" ||
		strings.HasSuffix(host, ".googleusercontent.com") || strings.HasSuffix(host, ".ggpht.com")
}

func (r *Router) RoundTrip(req *http.Request) (*http.Response, error) {
	direct, _ := req.Context().Value(directKey{}).(bool)
	host := strings.ToLower(strings.TrimSuffix(req.URL.Hostname(), "."))
	content := req.URL.Scheme == "https" && (req.URL.Port() == "" || req.URL.Port() == "443") && googleHost(host)
	r.mu.Lock()
	status, transport, closed := r.status, r.direct, r.closed
	r.mu.Unlock()
	if closed {
		return fail(req, fmt.Errorf("gdirect: router closed: %w", core.ErrUnsupported))
	}
	if !direct || !content {
		return r.normal.RoundTrip(req)
	}
	if !status.Enabled {
		return fail(req, fmt.Errorf("gdirect: direct routing disabled: %w", core.ErrUnsupported))
	}
	if status.Selecting || transport == nil {
		return fail(req, fmt.Errorf("gdirect: selected route unavailable; refresh required: %w", core.ErrBusy))
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		// Never expose dial/TLS details, probe again, or replay via normal routing.
		return nil, fmt.Errorf("gdirect: selected route failed; refresh required: %w", core.ErrBusy)
	}
	return resp, nil
}

func fail(req *http.Request, err error) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, err
}

// Each successful Refresh installs a new pool pinned to one immutable IP. The
// URL, HTTP Host, TLS SNI and certificate checks retain the real content host.
// Old pools close only idle connections, allowing their active streams to finish.
func (r *Router) newDirect(ip netip.Addr) *http.Transport {
	t := r.normal.Clone()
	t.Proxy = nil
	t.TLSHandshakeTimeout = selectionTimeout
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil || port != "443" || !googleHost(strings.ToLower(strings.TrimSuffix(host, "."))) {
			return nil, fmt.Errorf("gdirect: unsupported content destination: %w", core.ErrUnsupported)
		}
		ctx, cancel := context.WithTimeout(ctx, selectionTimeout)
		defer cancel()
		return r.dial(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	return t
}
