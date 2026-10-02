package httpcore

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ClientIPResolver determines the true origin of a request.
//
// X-Forwarded-For is attacker-controlled unless the immediate peer is a proxy we
// trust. The resolver therefore walks the forwarded chain right-to-left, skipping
// addresses that are themselves trusted proxies, and stops at the first
// untrusted address. If the immediate peer is not trusted, the forwarded headers
// are ignored entirely.
type ClientIPResolver struct {
	trusted []*net.IPNet
}

// NewClientIPResolver compiles the trusted-proxy CIDR list.
func NewClientIPResolver(cidrs []string) (*ClientIPResolver, error) {
	r := &ClientIPResolver{}
	for _, raw := range cidrs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// Accept a bare address as a /32 or /128 so operators can list a single host.
		if !strings.Contains(raw, "/") {
			ip := net.ParseIP(raw)
			if ip == nil {
				return nil, fmt.Errorf("httpcore: trusted proxy %q is not an IP or CIDR", raw)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			raw = fmt.Sprintf("%s/%d", raw, bits)
		}
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("httpcore: trusted proxy %q is not a CIDR: %w", raw, err)
		}
		r.trusted = append(r.trusted, network)
	}
	return r, nil
}

// IsTrusted reports whether addr (host or host:port) is a trusted proxy.
func (r *ClientIPResolver) IsTrusted(addr string) bool {
	ip := parseAddr(addr)
	if ip == nil {
		return false
	}
	for _, n := range r.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP returns the best available client address, without a port. It never
// returns nil-derived values; an unparseable peer yields the zero IP.
func (r *ClientIPResolver) ClientIP(req *http.Request) net.IP {
	peer := parseAddr(req.RemoteAddr)
	if peer == nil {
		return net.IPv4zero
	}
	// Only believe forwarding headers when they come from a proxy we operate.
	if !r.IsTrusted(req.RemoteAddr) {
		return peer
	}

	chain := forwardedChain(req)
	// Walk right to left: the rightmost entry was appended by the closest proxy.
	for i := len(chain) - 1; i >= 0; i-- {
		ip := parseAddr(chain[i])
		if ip == nil {
			// A malformed entry means the chain cannot be trusted further left.
			return peer
		}
		if !r.IsTrusted(chain[i]) {
			return ip
		}
	}
	// Every hop was a trusted proxy; the peer itself is the client.
	return peer
}

// Scheme reports the effective request scheme, honouring X-Forwarded-Proto only
// from trusted proxies. The session cookie's Secure attribute and the pairing
// link depend on this.
func (r *ClientIPResolver) Scheme(req *http.Request) string {
	if r.IsTrusted(req.RemoteAddr) {
		if proto := req.Header.Get("X-Forwarded-Proto"); proto != "" {
			// A comma-separated list may appear when several proxies append.
			first, _, _ := strings.Cut(proto, ",")
			first = strings.ToLower(strings.TrimSpace(first))
			if first == "http" || first == "https" {
				return first
			}
		}
	}
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

// forwardedChain flattens the X-Forwarded-For header into individual addresses.
func forwardedChain(req *http.Request) []string {
	var chain []string
	for _, header := range req.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(header, ",") {
			if part = strings.TrimSpace(part); part != "" {
				chain = append(chain, part)
			}
		}
	}
	return chain
}

// parseAddr extracts an IP from "host", "host:port", or "[v6]:port".
func parseAddr(addr string) net.IP {
	if addr == "" {
		return nil
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(strings.Trim(addr, "[]"))
}
