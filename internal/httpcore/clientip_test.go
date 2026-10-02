package httpcore_test

import (
	"crypto/tls"
	"net"
	"net/http"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/httpcore"
)

// mustResolver builds a resolver from a trusted-proxy list.
func mustResolver(t *testing.T, cidrs ...string) *httpcore.ClientIPResolver {
	t.Helper()

	r, err := httpcore.NewClientIPResolver(cidrs)
	if err != nil {
		t.Fatalf("NewClientIPResolver(%q): %v", cidrs, err)
	}
	return r
}

// request builds a request with exactly the peer address and headers a case
// needs. Hand-building beats httptest.NewRequest here, because the whole point
// of ClientIP is what it does with RemoteAddr and a forged header.
func request(remoteAddr string, xff ...string) *http.Request {
	r := &http.Request{RemoteAddr: remoteAddr, Header: http.Header{}}
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestClientIPTrustsForwardedHeadersOnlyFromATrustedProxy(t *testing.T) {
	tests := []struct {
		name       string
		trusted    []string
		remoteAddr string
		xff        []string
		want       string
	}{
		{
			name:       "attacker controls X-Forwarded-For: with no trusted proxies the header is ignored",
			remoteAddr: "198.51.100.4:52311",
			xff:        []string{"203.0.113.9"},
			want:       "198.51.100.4",
		},
		{
			name:       "attacker forges X-Forwarded-For from a peer that is merely private, not trusted",
			trusted:    []string{"10.0.0.0/8"},
			remoteAddr: "127.0.0.1:52311",
			xff:        []string{"203.0.113.9"},
			want:       "127.0.0.1",
		},
		{
			name:       "a trusted proxy's X-Forwarded-For is believed",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			xff:        []string{"203.0.113.9"},
			want:       "203.0.113.9",
		},
		{
			name:       "attacker controls the left of the chain: the rightmost untrusted hop wins",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			xff:        []string{"1.2.3.4, 203.0.113.9"},
			want:       "203.0.113.9",
		},
		{
			name:       "attacker prepends a trusted proxy to hide behind: the rightmost untrusted hop still wins",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			xff:        []string{"127.0.0.1, 203.0.113.9"},
			want:       "203.0.113.9",
		},
		{
			name:       "garbage to the left of the first untrusted hop is never reached",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			xff:        []string{"not-an-ip, 203.0.113.9"},
			want:       "203.0.113.9",
		},
		{
			name:       "a malformed hop on the right stops the walk and returns the peer",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			xff:        []string{"203.0.113.9, 999.999.999.999"},
			want:       "127.0.0.1",
		},
		{
			name:       "a chain of only trusted proxies falls back to the peer",
			trusted:    []string{"127.0.0.1/32", "::1/128"},
			remoteAddr: "127.0.0.1:52311",
			xff:        []string{"127.0.0.1, ::1"},
			want:       "127.0.0.1",
		},
		{
			name:       "no header at all leaves the peer as the client",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			want:       "127.0.0.1",
		},
		{
			name:       "several X-Forwarded-For headers are flattened into one chain",
			trusted:    []string{"10.0.0.0/8"},
			remoteAddr: "10.0.0.1:52311",
			xff:        []string{"203.0.113.9", "10.0.0.9"},
			want:       "203.0.113.9",
		},
		{
			name:       "a hop carrying a port is still an address",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			xff:        []string{"203.0.113.9:41234"},
			want:       "203.0.113.9",
		},
		{
			name:       "an IPv6 peer in bracketed host:port form",
			trusted:    []string{"::1/128"},
			remoteAddr: "[::1]:52311",
			xff:        []string{"2001:db8::5"},
			want:       "2001:db8::5",
		},
		{
			name:       "an IPv6 hop in the chain",
			trusted:    []string{"::1/128"},
			remoteAddr: "[::1]:52311",
			xff:        []string{"2001:db8::5, ::1"},
			want:       "2001:db8::5",
		},
		{
			name:       "an IPv4-mapped IPv6 peer still matches an IPv4 trusted proxy",
			trusted:    []string{"127.0.0.1"},
			remoteAddr: "[::ffff:127.0.0.1]:52311",
			xff:        []string{"203.0.113.9"},
			want:       "203.0.113.9",
		},
		{
			name:       "an unparseable peer yields the zero address rather than a panic",
			remoteAddr: "garbage",
			xff:        []string{"203.0.113.9"},
			want:       "0.0.0.0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := mustResolver(t, tc.trusted...)
			got := r.ClientIP(request(tc.remoteAddr, tc.xff...))
			if want := net.ParseIP(tc.want); !got.Equal(want) {
				t.Errorf("ClientIP(peer %q, X-Forwarded-For %q) = %v, want %v",
					tc.remoteAddr, tc.xff, got, want)
			}
		})
	}
}

func TestNewClientIPResolverAcceptsABareAddressAsASingleHost(t *testing.T) {
	r := mustResolver(t, "127.0.0.1", "::1")

	if !r.IsTrusted("127.0.0.1:1234") {
		t.Error("a bare 127.0.0.1 did not trust that host")
	}
	if !r.IsTrusted("[::1]:1234") {
		t.Error("a bare ::1 did not trust that host")
	}
	// A bare address is one host, not the network around it.
	if r.IsTrusted("127.0.0.2:1234") {
		t.Error("a bare 127.0.0.1 trusted the whole 127.0.0.0/8")
	}
	if r.IsTrusted("::2") {
		t.Error("a bare ::1 trusted the whole IPv6 space")
	}
}

func TestNewClientIPResolverRejectsEntriesThatAreNeitherIPNorCIDR(t *testing.T) {
	// A typo in the trusted-proxy list must stop the gateway at startup rather
	// than silently trusting nothing (or, worse, something unintended).
	for _, entry := range []string{"not-an-ip", "example.com", "10.0.0.1/33", "300.1.1.1", "127.0.0.1/", "10.0.0.0/-1"} {
		t.Run(entry, func(t *testing.T) {
			if _, err := httpcore.NewClientIPResolver([]string{entry}); err == nil {
				t.Errorf("NewClientIPResolver(%q) = nil error, want rejection", entry)
			}
		})
	}
}

func TestNewClientIPResolverSkipsEmptyEntries(t *testing.T) {
	// A YAML list with a blank line produces an empty string; it must not be
	// mistaken for "trust everything".
	r := mustResolver(t, "", "   ", "127.0.0.1")

	if !r.IsTrusted("127.0.0.1:1234") {
		t.Error("the real entry in the list was not trusted")
	}
	if r.IsTrusted("0.0.0.0:1234") {
		t.Error("a blank entry was treated as a trusted network")
	}

	empty := mustResolver(t)
	if empty.IsTrusted("127.0.0.1:1234") {
		t.Error("an empty trusted-proxy list trusted a loopback peer")
	}
}

func TestIsTrustedRejectsUnparseableAddresses(t *testing.T) {
	r := mustResolver(t, "127.0.0.1/32", "::1/128")

	for _, addr := range []string{"", "garbage", "not:an:address:at:all", "[]"} {
		if r.IsTrusted(addr) {
			t.Errorf("IsTrusted(%q) = true, want false", addr)
		}
	}
}

func TestSchemeHonoursForwardedProtoOnlyFromATrustedProxy(t *testing.T) {
	tests := []struct {
		name       string
		trusted    []string
		remoteAddr string
		proto      []string
		tls        bool
		want       string
	}{
		{
			name:       "no trusted proxy: a forged X-Forwarded-Proto cannot claim https",
			remoteAddr: "198.51.100.4:52311",
			proto:      []string{"https"},
			want:       "http",
		},
		{
			name:       "a trusted proxy reporting https",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			proto:      []string{"https"},
			want:       "https",
		},
		{
			name:       "only the first value of a comma list counts",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			proto:      []string{"https, http"},
			want:       "https",
		},
		{
			name:       "the first value of a comma list, http first",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			proto:      []string{"http, https"},
			want:       "http",
		},
		{
			name:       "case and surrounding space are ignored",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			proto:      []string{"  HTTPS  "},
			want:       "https",
		},
		{
			name:       "an unparseable value is ignored and the transport decides",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			proto:      []string{"ftp"},
			tls:        true,
			want:       "https",
		},
		{
			name:       "an unparseable value over plain http",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			proto:      []string{"ftp"},
			want:       "http",
		},
		{
			name:       "TLS is reported when no forwarded header is present",
			remoteAddr: "198.51.100.4:52311",
			tls:        true,
			want:       "https",
		},
		{
			name:       "a trusted proxy may report plain http even over TLS, since the tunnel is what it terminates",
			trusted:    []string{"127.0.0.1/32"},
			remoteAddr: "127.0.0.1:52311",
			proto:      []string{"http"},
			tls:        true,
			want:       "http",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := mustResolver(t, tc.trusted...)
			req := request(tc.remoteAddr)
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			for _, proto := range tc.proto {
				req.Header.Add("X-Forwarded-Proto", proto)
			}

			if got := r.Scheme(req); got != tc.want {
				t.Errorf("Scheme(peer %q, X-Forwarded-Proto %q, TLS %v) = %q, want %q",
					tc.remoteAddr, tc.proto, tc.tls, got, tc.want)
			}
		})
	}
}
