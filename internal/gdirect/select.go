package gdirect

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"sync"
	"time"
)

const (
	maxCandidates    = 32
	probeConcurrency = 8
	selectionTimeout = 3 * time.Second
	probeHost        = "lh3.googleusercontent.com"
)

// These are a deliberately incomplete subset of Google's core address space,
// checked against https://www.gstatic.com/ipranges/goog.json. Only explicit
// selection probes these addresses; no CIDR is swept.
var googlePrefixes = []netip.Prefix{
	netip.MustParsePrefix("64.233.160.0/19"), netip.MustParsePrefix("74.125.0.0/16"),
	netip.MustParsePrefix("108.177.0.0/17"), netip.MustParsePrefix("142.250.0.0/15"),
	netip.MustParsePrefix("172.217.0.0/16"), netip.MustParsePrefix("172.253.0.0/16"),
	netip.MustParsePrefix("173.194.0.0/16"), netip.MustParsePrefix("192.178.0.0/15"),
	netip.MustParsePrefix("209.85.128.0/17"), netip.MustParsePrefix("216.58.192.0/19"),
	netip.MustParsePrefix("216.239.32.0/19"), netip.MustParsePrefix("2001:4860::/32"),
	netip.MustParsePrefix("2404:6800::/32"), netip.MustParsePrefix("2607:f8b0::/32"),
	netip.MustParsePrefix("2800:3f0::/32"), netip.MustParsePrefix("2a00:1450::/32"),
}

// Eight fixed GFE seeds, independently selected within the core prefixes above;
// availability is not assumed. Every seed must pass real-host TLS verification.
var fallbackIPs = []netip.Addr{
	netip.MustParseAddr("142.250.72.14"), netip.MustParseAddr("142.250.190.14"),
	netip.MustParseAddr("142.251.32.14"), netip.MustParseAddr("172.217.0.46"),
	netip.MustParseAddr("172.217.14.206"), netip.MustParseAddr("216.58.214.14"),
	netip.MustParseAddr("74.125.24.100"), netip.MustParseAddr("172.253.62.113"),
}

func googleIP(ip netip.Addr) bool {
	for _, prefix := range googlePrefixes {
		if prefix.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}

func (r *Router) candidates(ctx context.Context) []netip.Addr {
	dnsCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	answers, _ := r.lookup(dnsCtx, probeHost)
	cancel()
	ips := make([]netip.Addr, 0, maxCandidates)
	seen := make(map[netip.Addr]bool)
	add := func(ip netip.Addr) {
		ip = ip.Unmap()
		if googleIP(ip) && !seen[ip] && len(ips) < maxCandidates {
			seen[ip] = true
			ips = append(ips, ip)
		}
	}
	for i, ip := range answers {
		if i >= maxCandidates-len(fallbackIPs) {
			break
		}
		add(ip)
	}
	for _, ip := range fallbackIPs {
		add(ip)
	}
	return ips
}

func (r *Router) choose(ctx context.Context) netip.Addr {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan netip.Addr, maxCandidates)
	for _, ip := range r.candidates(ctx) {
		jobs <- ip
	}
	close(jobs)
	var wg sync.WaitGroup
	var once sync.Once
	var selected netip.Addr
	for range probeConcurrency {
		wg.Go(func() {
			for ip := range jobs {
				if ctx.Err() != nil {
					return
				}
				if r.probe(ctx, ip) {
					once.Do(func() { selected = ip; cancel() })
					return
				}
			}
		})
	}
	wg.Wait() // All losing TCP/TLS probes are closed before returning.
	return selected
}

func (r *Router) probe(ctx context.Context, ip netip.Addr) bool {
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	conn, err := r.dial(ctx, "tcp", net.JoinHostPort(ip.String(), "443"))
	if err != nil {
		return false
	}
	defer conn.Close()
	config := r.normal.TLSClientConfig.Clone()
	config.ServerName = probeHost
	return tls.Client(conn, config).HandshakeContext(ctx) == nil
}
