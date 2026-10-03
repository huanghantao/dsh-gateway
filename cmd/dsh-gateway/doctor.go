package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/agenthost/hostlink"
	"github.com/huanghantao/dsh-gateway/internal/agenthost/hostwire"
	"github.com/huanghantao/dsh-gateway/internal/config"
)

// doctorResult is one check's outcome.
type doctorResult struct {
	status status
	title  string
	detail string
	hint   string
}

type status int

const (
	statusOK status = iota
	statusWarn
	statusFail
	statusSkip
)

func (s status) marker() string {
	switch s {
	case statusOK:
		return "OK  "
	case statusWarn:
		return "WARN"
	case statusFail:
		return "FAIL"
	default:
		return "SKIP"
	}
}

// runDoctor walks the whole request path and reports where it breaks.
//
// This exists because the failures that actually happen during setup are the
// non-obvious ones. A port can complete a TCP handshake and still carry no data,
// so `nc -z` reports it open while the certificate can never be issued. A
// service can log to a file the operator is not reading. Neither shows up as an
// error anywhere; both cost an afternoon. The doctor turns each into one line
// with the specific thing to change.
func runDoctor(args []string) error {
	var (
		configPath string
		stateDir   string
		publicURL  string
		timeout    time.Duration
	)

	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.StringVar(&configPath, "config", "", "path to config.yaml (default: <stateDir>/config.yaml)")
	fs.StringVar(&stateDir, "state-dir", "", "state directory (default ~/.dsh-gateway)")
	fs.StringVar(&publicURL, "public-url", "", "override the public URL to probe")
	fs.DurationVar(&timeout, "timeout", 10*time.Second, "per-check network timeout")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: dsh-gateway doctor [flags]\n\n")
		fmt.Fprint(os.Stderr, "Checks the configuration, the local gateway, the tunnel, and the public\n"+
			"HTTPS ingress, and reports the first thing that is wrong.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cfg, cfgPath, err := resolveConfig(runFlags{
		configPath: configPath,
		stateDir:   stateDir,
		publicURL:  publicURL,
	})
	if err != nil {
		// A configuration that will not load IS the diagnosis; the error already
		// names the file and every problem in it.
		fmt.Printf("%s configuration\n        %v\n", statusFail.marker(), err)
		return nil
	}

	fmt.Printf("dsh-gateway doctor  (config %s)\n\n", cfgPath)

	// Ordered along the request path, so the first failure reported is also the
	// first one to fix.
	results := []doctorResult{
		checkConfig(cfg, cfgPath),
		checkStateDir(cfg),
		checkAgentHost(ctx, cfg, timeout),
		checkLocalGateway(ctx, cfg, timeout),
		checkTunnelConfig(cfg),
		checkTunnelControl(ctx, cfg, timeout),
		checkIngress(ctx, cfg, timeout),
	}

	failed, warned := 0, 0
	for _, r := range results {
		fmt.Printf("  %s  %s\n", r.status.marker(), r.title)
		if r.detail != "" {
			fmt.Printf("          %s\n", r.detail)
		}
		if r.hint != "" {
			fmt.Printf("        → %s\n", r.hint)
		}
		switch r.status {
		case statusFail:
			failed++
		case statusWarn:
			warned++
		case statusOK, statusSkip:
			// Neither counts against the summary.
		}
	}

	fmt.Println()
	switch {
	case failed > 0:
		fmt.Printf("%d check(s) failed. Fix the first failure above; later ones are often its consequence.\n", failed)
		return errors.New("doctor found failures")
	case warned > 0:
		fmt.Printf("Everything essential works. %d warning(s) above are worth reading.\n", warned)
	default:
		fmt.Println("All checks passed.")
	}
	return nil
}

// --- individual checks ------------------------------------------------------

func checkConfig(cfg config.Config, path string) doctorResult {
	caps := cfg.DesktopUI.Enabled
	detail := fmt.Sprintf("listen %s, public URL %s, %d workspace(s)",
		cfg.Listen, orNone(cfg.PublicURL), len(cfg.Workspaces))
	if caps {
		detail += ", desktop GUI proxy enabled"
	}

	r := doctorResult{status: statusOK, title: "configuration is valid", detail: detail}
	if cfg.PublicURL == "" {
		r.status = statusFail
		r.hint = fmt.Sprintf("set publicURL in %s (or pass -public-url); without it the pairing "+
			"link points at a loopback address and the session cookie is not marked Secure", path)
	}
	if len(cfg.Auth.TrustedProxies) == 0 {
		r.status = statusWarn
		r.hint = "auth.trustedProxies is empty, so X-Forwarded-Proto from your proxy is not believed; " +
			"cookies will not be marked Secure even though the deployment is HTTPS"
	}
	return r
}

// checkAgentHost asks whether the process that holds the agent is there.
//
// It is the first thing to be wrong when the phone stops working, because it is
// the one component with a lifetime of its own: a gateway that is up while its
// host is down reports every session as idle and refuses to start a turn, and
// from the phone that looks like an agent that has stopped thinking.
func checkAgentHost(ctx context.Context, cfg config.Config, timeout time.Duration) doctorResult {
	socket := cfg.SocketPath()
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := hostlink.Dial(probeCtx, socket, 0)
	if err != nil {
		return doctorResult{
			status: statusFail,
			title:  "the agent host is not answering",
			detail: fmt.Sprintf("%s: %v", socket, err),
			hint: "start it, or look at why it stopped:" +
				"\n          launchctl print gui/$(id -u)/dev.dsh-gateway.agent-host | head -30" +
				"\n          tail -40 ~/Library/Logs/dsh-gateway/agent-host.log" +
				"\n          dsh-agent-host status",
		}
	}
	defer func() { _ = conn.Close() }()

	wire := hostwire.NewConn(conn)
	wire.SetErrorClassifier(func(err error) *hostwire.Error { return hostwire.ErrorOf(err) })
	wire.Start()

	// A read-only probe. `doctor` is run precisely when something is wrong, and
	// a diagnostic that disconnects the gateway it is diagnosing would be worse
	// than no diagnostic at all.
	var hello hostwire.HelloResult
	if err := wire.Call(probeCtx, hostwire.MethodHello, hostwire.Hello{
		Protocol: hostwire.Protocol, Caller: "doctor",
	}, &hello); err != nil {
		return doctorResult{
			status: statusFail,
			title:  "the agent host accepted a connection but did not answer",
			detail: err.Error(),
			hint: "a host that listens and does not speak is wedged rather than absent:" +
				"\n          tail -40 ~/Library/Logs/dsh-gateway/agent-host.log",
		}
	}

	var status hostwire.StatusResult
	if err := wire.Call(probeCtx, hostwire.MethodStatus, nil, &status); err != nil {
		return doctorResult{status: statusWarn, title: "the agent host answered but not its status",
			detail: err.Error()}
	}

	r := doctorResult{
		status: statusOK,
		title:  "the agent host is serving",
		detail: fmt.Sprintf("harness %s, %d session(s) held, %d turn(s) running",
			status.State, status.SessionsHeld, status.TurnsRunning),
	}
	if status.Draining {
		r.status = statusWarn
		r.hint = "the host is draining for a restart; turns in flight are being ended"
	}
	if status.State != hostwire.StateReady {
		r.status = statusWarn
		r.hint = "the host is up but its child is not ready; the log says why:" +
			"\n          tail -40 ~/Library/Logs/dsh-gateway/agent-host.log"
	}
	return r
}

func checkStateDir(cfg config.Config) doctorResult {
	info, err := os.Stat(cfg.StateDir)
	if err != nil {
		return doctorResult{
			status: statusWarn, title: "state directory does not exist yet",
			detail: cfg.StateDir,
			hint:   "it is created on first run; nothing to do if the gateway has never started",
		}
	}
	if !info.IsDir() {
		return doctorResult{status: statusFail, title: "state directory is not a directory", detail: cfg.StateDir}
	}

	devices := 0
	if raw, err := os.ReadFile(filepath.Join(cfg.StateDir, "devices.json")); err == nil {
		var doc struct {
			Devices []struct {
				Revoked bool `json:"revoked"`
			} `json:"devices"`
		}
		if json.Unmarshal(raw, &doc) == nil {
			for _, d := range doc.Devices {
				if !d.Revoked {
					devices++
				}
			}
		}
	}

	r := doctorResult{
		status: statusOK, title: "state directory is usable",
		detail: fmt.Sprintf("%s, %d active device(s)", cfg.StateDir, devices),
	}
	if info.Mode().Perm() != 0o700 {
		r.status = statusWarn
		r.detail += fmt.Sprintf(", mode %04o", info.Mode().Perm())
		r.hint = fmt.Sprintf("chmod 700 %s; it holds the pairing secret and the device records", cfg.StateDir)
	}
	if devices == 0 {
		r.status = statusWarn
		r.hint = "no device is paired yet; run `dsh-gateway pair` and scan the code with your phone"
	}
	return r
}

func checkLocalGateway(ctx context.Context, cfg config.Config, timeout time.Duration) doctorResult {
	base := "http://" + cfg.Listen

	health, err := httpGet(ctx, base+"/healthz", timeout)
	if err != nil {
		return doctorResult{
			status: statusFail, title: "the gateway is not answering on " + cfg.Listen,
			detail: err.Error(),
			hint:   notRunningHint(),
		}
	}

	ready, err := httpGet(ctx, base+"/readyz", timeout)
	if err != nil {
		return doctorResult{status: statusWarn, title: "gateway is up but /readyz did not answer", detail: err.Error()}
	}

	var state struct {
		Status  string `json:"status"`
		Harness string `json:"harness"`
		Detail  string `json:"detail"`
		Dropped uint64 `json:"droppedEvents"`
	}
	_ = json.Unmarshal([]byte(ready), &state)

	r := doctorResult{
		status: statusOK, title: "gateway is answering",
		detail: fmt.Sprintf("%s, harness %s", strings.TrimSpace(health), state.Harness),
	}
	if state.Status != "ready" {
		r.status = statusFail
		r.hint = "the harness child has not finished its handshake; " + state.Detail +
			". Usually this means `dsh --profile acp` cannot start or has no provider configured — " +
			"see the ACP profile section of README.md"
	}
	if state.Dropped > 0 {
		r.status = statusWarn
		r.hint = fmt.Sprintf("%d event(s) were dropped for a client that fell behind; harmless unless it keeps growing", state.Dropped)
	}
	return r
}

func checkTunnelConfig(cfg config.Config) doctorResult {
	path := filepath.Join(cfg.StateDir, "frpc.toml")
	// path is the operator's own state directory.
	raw, err := os.ReadFile(path) //nolint:gosec // operator-configured path
	if err != nil {
		return doctorResult{
			status: statusWarn, title: "no frpc.toml found",
			detail: path,
			hint:   "expected only if you run the tunnel another way; see deploy/mac/install.sh",
		}
	}

	serverAddr := tomlValue(string(raw), "serverAddr")
	serverPort := tomlValue(string(raw), "serverPort")
	if serverAddr == "" {
		return doctorResult{status: statusFail, title: "frpc.toml has no serverAddr", detail: path}
	}
	return doctorResult{
		status: statusOK, title: "tunnel configuration found",
		detail: fmt.Sprintf("%s:%s", serverAddr, serverPort),
	}
}

func checkTunnelControl(ctx context.Context, cfg config.Config, timeout time.Duration) doctorResult {
	path := filepath.Join(cfg.StateDir, "frpc.toml")
	raw, err := os.ReadFile(path) //nolint:gosec // operator-configured path
	if err != nil {
		return doctorResult{status: statusSkip, title: "tunnel control port not checked"}
	}

	host := tomlValue(string(raw), "serverAddr")
	if host == "" {
		return doctorResult{status: statusSkip, title: "tunnel control port not checked"}
	}
	port := tomlValue(string(raw), "serverPort")
	if port == "" {
		port = "7000"
	}
	addr := net.JoinHostPort(host, port)

	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return doctorResult{
			status: statusFail, title: "cannot reach the frp control port at " + addr,
			detail: err.Error(),
			hint:   "check that frps is running on the server (systemctl status frps) and that its port is allowed by both ufw and your provider's firewall",
		}
	}
	_ = conn.Close()
	return doctorResult{status: statusOK, title: "frp control port is reachable", detail: addr}
}

// checkIngress is the check that earns this command's keep.
//
// It distinguishes a port that is genuinely open from one where a NAT completes
// the TCP handshake and then forwards nothing. Both look "open" to nc and to a
// naive health check, but only the first can ever serve a certificate — and the
// second is what a misconfigured cloud port mapping looks like.
func checkIngress(ctx context.Context, cfg config.Config, timeout time.Duration) doctorResult {
	if cfg.PublicURL == "" {
		return doctorResult{status: statusSkip, title: "public ingress not checked (no publicURL)"}
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Hostname() == "" {
		return doctorResult{status: statusFail, title: "publicURL cannot be parsed", detail: cfg.PublicURL}
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(host, port)

	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return doctorResult{
			status: statusFail, title: "the public address does not accept connections",
			detail: fmt.Sprintf("%s: %v", addr, err),
			hint: "open the port on your VPS provider's firewall (a cloud security group or the port " +
				"mapping in front of the instance). ufw inside the VM cannot do this for you.",
		}
	}
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)

	var exchangeErr error
	var summary string
	if u.Scheme == "https" {
		summary, exchangeErr = probeTLS(ctx, conn, host)
	} else {
		summary, exchangeErr = probeHTTP(conn, u.Host, u.Path)
	}

	if exchangeErr != nil {
		return doctorResult{
			status: statusFail,
			title:  "the port accepts a TCP connection but carries no data",
			detail: fmt.Sprintf("%s: %s", addr, summary),
			hint: "this is the NAT signature: something in front of the instance completes the " +
				"handshake and then forwards nothing, so `nc -z` reports the port open while nothing " +
				"can ever be served. Add an explicit port mapping or security-group rule for " + port +
				" on your provider's side — 22 and the frp control port are evidently mapped already.",
		}
	}
	return doctorResult{
		status: statusOK,
		title:  "public ingress works",
		detail: fmt.Sprintf("%s: %s", addr, summary),
	}
}

// probeTLS completes a handshake and one request, reporting what it saw.
func probeTLS(ctx context.Context, conn net.Conn, host string) (string, error) {
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		// Verification is deliberately off. This function's job is to report
		// what is actually there — including a missing or untrusted certificate,
		// which during first-time ACME issuance is exactly the interesting case.
		// Verifying here would replace a precise diagnosis with a generic error.
		InsecureSkipVerify: true, //nolint:gosec // diagnostic probe, not a trust decision
		MinVersion:         tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return "TLS handshake failed: " + err.Error(), err
	}
	state := tlsConn.ConnectionState()
	cert := "no certificate"
	if len(state.PeerCertificates) > 0 {
		leaf := state.PeerCertificates[0]
		cert = fmt.Sprintf("certificate for %s, expires %s",
			strings.Join(leaf.DNSNames, ","), leaf.NotAfter.UTC().Format("2006-01-02"))
		if len(leaf.DNSNames) == 0 {
			cert = fmt.Sprintf("certificate for %s, expires %s",
				leaf.Subject.CommonName, leaf.NotAfter.UTC().Format("2006-01-02"))
		}
	}
	return fmt.Sprintf("TLS %s, %s", tlsVersionName(state.Version), cert), nil
}

// probeHTTP sends one request and reads the first bytes of the response.
func probeHTTP(conn net.Conn, host, path string) (string, error) {
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: dsh-gateway-doctor\r\nConnection: close\r\n\r\n", path, host)
	if _, err := io.WriteString(conn, req); err != nil {
		return "write failed: " + err.Error(), err
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if n == 0 {
		msg := "the connection was closed without a response"
		if err != nil {
			msg += ": " + err.Error()
		}
		return msg, errors.New("no data")
	}
	line, _, _ := strings.Cut(string(buf[:n]), "\r\n")
	return strings.TrimSpace(line), nil
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	default:
		return fmt.Sprintf("0x%04x", v)
	}
}

func httpGet(ctx context.Context, rawURL string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return string(body), nil
}

// tomlValue extracts a scalar from a TOML document.
//
// This is deliberately not a TOML parser. It reads the two known keys out of a
// file this project wrote, and it splits on the first '=' with both sides
// trimmed — the detail an earlier hand-rolled attempt got wrong, producing
// nonsense like `= "10.0.0.1` and a port of zero.
func tomlValue(raw, key string) string {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

// notRunningHint says how to look at why the gateway is down, on the platform
// the operator is actually on.
//
// `doctor` runs anywhere, so the launchd incantation is offered only where it
// exists and only for the label the bundled installer uses — someone who started
// the gateway by hand would otherwise be told to query a service that was never
// installed.
func notRunningHint() string {
	if runtime.GOOS == "darwin" {
		return "is it running? If you installed it with deploy/mac/install.sh, " +
			"`launchctl print gui/$(id -u)/dev.dsh-gateway.gateway | head -20`; " +
			"otherwise use whatever started it. Then read its log — the installer " +
			"prints the path, and ~/Library/Logs/dsh-gateway/gateway.log is the default."
	}
	return "is it running? Check whatever started it (systemd, a container, a " +
		"foreground shell), then read its log."
}
