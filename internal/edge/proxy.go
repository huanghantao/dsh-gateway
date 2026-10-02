// Package edge reverse-proxies the stock DeepSeek Harness browser GUI.
//
// This surface is optional and off by default. It exists because the mobile app
// covers the conversational loop but not everything DSH's own GUI does — the
// terminal, the file browser, settings, plugin management — and sometimes the
// only way to finish a job is to open the real thing.
//
// It is also the largest attack surface the gateway can expose: the GUI drives an
// agent with shell access and authenticates only with its own session cookie. So
// it inherits the gateway's authentication, but an operator has to opt in.
//
// Several details are load-bearing, and each was established by reading DSH's
// compiled server rather than inferred:
//
//   - The Host header must be forwarded exactly as the browser sent it, with no
//     port added. DSH derives both its trust decision and its cookie name from
//     the request authority, so a proxy that appends ":443" changes the cookie
//     name and silently produces a login loop.
//   - Origin must not be rewritten. DSH refuses a request whose Origin host does
//     not equal its Host, and the browser's Origin is the public hostname.
//   - The GUI cannot be mounted on a subpath. Its routes (/api, /plugins) are
//     root-relative and its bundle relies on <base href="./">, so this proxy owns
//     "/" outright rather than a prefix.
//   - DSH gzips almost every response, and its compressed HTML has no
//     Content-Length. Asking upstream for identity encoding keeps responses
//     inspectable and removes an entire class of framing surprise.
package edge

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// Proxy forwards requests to a loopback DSH web server.
type Proxy struct {
	upstream *url.URL
	rp       *httputil.ReverseProxy
	logger   *logx.Logger
}

// New builds the proxy.
func New(upstream string, logger *logx.Logger) (*Proxy, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, errx.Wrap(err, errx.KindInvalid, "invalid_upstream", "the desktop UI upstream URL is not valid")
	}
	if target.Host == "" {
		return nil, errx.New(errx.KindInvalid, "invalid_upstream", "the desktop UI upstream URL has no host")
	}
	host, _, err := net.SplitHostPort(target.Host)
	if err != nil {
		host = target.Host
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, errx.New(errx.KindInvalid, "invalid_upstream",
			"the desktop UI upstream must be a loopback address")
	}

	p := &Proxy{upstream: target, logger: logger}

	p.rp = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			incomingHost := r.In.Host

			r.SetURL(target)
			// Restore the browser's Host. SetURL rewrites it to the upstream
			// authority, which would break both the trust fence and the cookie
			// name that DSH derives from it.
			r.Out.Host = incomingHost

			// DSH ignores X-Forwarded-* entirely, so there is nothing to add for
			// its benefit. Clearing what we inherited keeps the upstream request
			// honest about where it came from rather than carrying a chain this
			// process cannot verify.
			r.Out.Header.Del("X-Forwarded-For")
			r.Out.Header.Del("X-Forwarded-Host")
			r.Out.Header.Del("X-Forwarded-Proto")

			// See the package comment: this removes gzip from the response path,
			// which makes every response length-delimited and inspectable.
			r.Out.Header.Set("Accept-Encoding", "identity")
		},
		// -1 disables response buffering, so server-sent events and long-poll
		// responses reach the phone as they are produced rather than at the end.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Warn("desktop UI proxy failed",
				"path", r.URL.Path,
				"error", err.Error(),
				"request_id", httpcore.RequestIDFrom(r.Context()),
			)
			// A dead upstream is the common case — the operator has not started
			// `dsh web` — so say that rather than a bare 502.
			httpcore.WriteError(w, r, logger, httpcore.RequestIDFrom(r.Context()),
				errx.Wrap(err, errx.KindUnavailable, "desktop_ui_unavailable",
					"the DeepSeek Harness web server is not reachable; start it with "+
						"`dsh web --no-open --trusted-host <this host>`"))
		},
	}
	return p, nil
}

// ServeHTTP proxies one request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A WebSocket upgrade needs a context that outlives the handshake, and
	// ReverseProxy handles the tunnelling itself once the upstream answers 101.
	// Nothing extra is required here beyond not imposing a read deadline, which
	// the server configuration already avoids for upgrades.
	p.rp.ServeHTTP(w, r)
}

// Register mounts the proxy at the site root.
func (p *Proxy) Register(mux *http.ServeMux) {
	mux.Handle("/", p)
}

// HealthCheck reports whether the upstream is answering, for /readyz.
func (p *Proxy) HealthCheck(ctx context.Context) error {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.upstream.String()+"/", nil)
	if err != nil {
		return err
	}
	// Any HTTP response means the server is up. It will answer 401 without a
	// cookie, which is exactly the healthy case.
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return nil
}

// RootHandler redirects the site root to the mobile app when the desktop UI is
// disabled, so that a bare visit lands somewhere useful.
func RootHandler(mountPath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			// Anything else at the root is a stale bookmark or a probe. Answering
			// the redirect would mask a genuine 404.
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, mountPath, http.StatusTemporaryRedirect)
	})
}
