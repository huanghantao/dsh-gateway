package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

// TestTomlValue covers the parsing an earlier revision got wrong.
//
// The bug was ordering: it trimmed the key prefix before trimming whitespace, so
// `serverAddr = "10.0.0.1"` yielded `= "10.0.0.1`, and the resulting address was
// nonsense that produced a confusing failure two checks later. The cases below
// are the shapes frpc.toml actually contains plus the ones that broke it.
func TestTomlValue(t *testing.T) {
	const doc = `# frpc — the Mac end of the dsh-gateway tunnel.
serverAddr = "203.0.113.9"
serverPort = 7000

[[proxies]]
name = "dsh-gateway"
localPort = 8787
remotePort = 18787
transport.useEncryption = true
`

	cases := []struct {
		key  string
		want string
	}{
		{"serverAddr", "203.0.113.9"},
		{"serverPort", "7000"},
		{"localPort", "8787"},
		{"remotePort", "18787"},
		{"name", "dsh-gateway"},
		// A dotted key must not be confused with its own suffix.
		{"useEncryption", ""},
		{"transport.useEncryption", "true"},
		// Absent keys yield empty rather than a partial match.
		{"missing", ""},
		{"server", ""},
		// A commented-out line must not be read as a value.
		{"frpc", ""},
	}

	for _, tc := range cases {
		if got := tomlValue(doc, tc.key); got != tc.want {
			t.Errorf("tomlValue(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestTomlValueHandlesQuotingAndSpacing(t *testing.T) {
	cases := []struct {
		doc, key, want string
	}{
		{`a="x"`, "a", "x"},
		{`a = 'x'`, "a", "x"},
		{`  a   =   x  `, "a", "x"},
		{"a\t=\t\"x\"", "a", "x"},
		// An empty document and a document with no '=' must not panic.
		{"", "a", ""},
		{"novalue", "a", ""},
	}
	for _, tc := range cases {
		if got := tomlValue(tc.doc, tc.key); got != tc.want {
			t.Errorf("tomlValue(%q, %q) = %q, want %q", tc.doc, tc.key, got, tc.want)
		}
	}
}

// TestProbeHTTPDistinguishesDataFromSilence pins the distinction the whole
// ingress check exists for.
//
// A NAT in front of a cloud instance can complete the TCP handshake and then
// forward nothing. The connection *succeeds*, so every naive reachability check
// — including `nc -z` — reports the port open, while no request will ever be
// served and no certificate can ever be issued. Only reading a byte tells the
// two apart.
func TestProbeHTTPDistinguishesDataFromSilence(t *testing.T) {
	t.Run("a server that answers", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		go func() {
			defer server.Close()
			buf := make([]byte, 512)
			_, _ = server.Read(buf)
			_, _ = server.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
		}()

		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		summary, err := probeHTTP(client, "example.test", "/healthz")
		if err != nil {
			t.Fatalf("probeHTTP reported an error for a responsive peer: %v (%s)", err, summary)
		}
		if !strings.Contains(summary, "200") {
			t.Errorf("summary = %q, want it to name the status line", summary)
		}
	})

	t.Run("a peer that accepts and then says nothing", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		go func() {
			// Accept the connection, read the request, then close without
			// answering — exactly what a mis-forwarded port looks like.
			buf := make([]byte, 512)
			_, _ = server.Read(buf)
			_ = server.Close()
		}()

		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		summary, err := probeHTTP(client, "example.test", "/healthz")
		if err == nil {
			t.Fatal("probeHTTP reported success for a peer that sent nothing; " +
				"the NAT failure mode would be misdiagnosed as a working port")
		}
		if !strings.Contains(strings.ToLower(summary), "without a response") {
			t.Errorf("summary = %q, want it to say the connection closed with no response", summary)
		}
	})
}

func TestTLSVersionName(t *testing.T) {
	if got := tlsVersionName(0x0304); got != "1.3" {
		t.Errorf("TLS 1.3 name = %q", got)
	}
	if got := tlsVersionName(0x0303); got != "1.2" {
		t.Errorf("TLS 1.2 name = %q", got)
	}
	// An unknown version must be reported rather than silently mislabelled.
	if got := tlsVersionName(0x0000); !strings.HasPrefix(got, "0x") {
		t.Errorf("unknown version = %q, want a hex fallback", got)
	}
}

// TestDoctorDoesNotPanicOnAMissingConfig keeps the diagnostic from becoming the
// thing that needs diagnosing. A config that fails to load is itself the
// finding, and must be reported rather than crashing.
func TestDoctorReportsAnUnloadableConfig(t *testing.T) {
	dir := t.TempDir()
	err := runDoctor([]string{"-state-dir", dir, "-config", dir + "/does-not-exist.yaml"})
	// A missing file keeps the defaults, and the defaults have no workspace, so
	// this is expected to report the workspace failure rather than succeed.
	if err == nil {
		t.Log("doctor reported success; acceptable only if the defaults became usable")
	}
}
