package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/httpcore"
)

// A route table that panics at startup takes the whole service down, and the
// failure surfaces as "the gateway will not start" rather than as a test
// failure. Registering on a real mux is therefore the cheapest way to prove the
// pattern set is well formed.
//
// This is not hypothetical: an earlier revision registered both "/m/" and
// "/m/{path...}", which the standard library rejects as a conflict because
// "{path...}" also matches the empty remainder.
func TestRegisterProducesAValidRouteTable(t *testing.T) {
	h, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mux := http.NewServeMux()
	// A panic here is the failure being tested for; recover so the test reports
	// it as a failure with a useful message instead of crashing the binary.
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("route registration panicked: %v", rec)
		}
	}()
	h.Register(mux)
}

func TestServesShellWithNonceAndAssets(t *testing.T) {
	h, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux)

	// The handler reads the nonce from the context, so the request must pass
	// through the middleware that installs it — the same arrangement production
	// uses.
	handler := httpcore.Chain(httpcore.SecurityHeaders(false))(mux)

	t.Run("shell", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/m/", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()

		// The placeholder leaking through would mean every script tag is
		// blocked by the CSP, producing a blank page with no obvious cause.
		if strings.Contains(body, noncePlaceholder) {
			t.Error("index.html still contains the nonce placeholder; the shell would be blocked by its own CSP")
		}

		// The nonce in the markup must match the one the header advertises.
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "'nonce-") {
			t.Fatalf("CSP carries no nonce: %q", csp)
		}
		start := strings.Index(csp, "'nonce-") + len("'nonce-")
		end := strings.Index(csp[start:], "'")
		if end < 0 {
			t.Fatalf("malformed CSP: %q", csp)
		}
		nonce := csp[start : start+end]
		if !strings.Contains(body, `nonce="`+nonce+`"`) {
			t.Error("markup nonce does not match the CSP nonce; scripts would be blocked")
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("shell Cache-Control = %q, want no-store (a cached nonce is a dead page)", got)
		}
	})

	t.Run("asset", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/m/app.js", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		// A module served with the wrong media type is refused outright by the
		// browser, which would present as a blank page.
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
			t.Errorf("Content-Type = %q, want text/javascript", ct)
		}
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Fatal("no ETag on an asset")
		}

		// A conditional request must be answered without a body.
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/m/app.js", nil)
		req.Header.Set("If-None-Match", etag)
		cond := httptest.NewRecorder()
		handler.ServeHTTP(cond, req)
		if cond.Code != http.StatusNotModified {
			t.Errorf("conditional GET status = %d, want 304", cond.Code)
		}
	})

	t.Run("service worker is served as javascript", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/m/sw.js", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("sw.js status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
			t.Errorf("sw.js Content-Type = %q; a worker with the wrong type is silently not registered", ct)
		}
	})

	t.Run("manifest", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/m/manifest.webmanifest", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("manifest status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
			t.Errorf("manifest Content-Type = %q", ct)
		}
	})

	t.Run("unknown asset is a 404, not the shell", func(t *testing.T) {
		// The app routes on the URL fragment, so an unknown path is a genuine
		// miss. Answering with the shell would hide a broken asset reference
		// behind a page that renders with no styles.
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/m/does-not-exist.js", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("path traversal is refused", func(t *testing.T) {
		for _, target := range []string{
			"/m/../internal/config/config.go",
			"/m/..%2f..%2fgo.mod",
			"/m/./../../go.mod",
		} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil))
			if rec.Code == http.StatusOK {
				t.Errorf("%s served a file from outside the bundle", target)
			}
		}
	})
}

// TestShellReferencesOnlyExistingAssets guards against a build that emits an
// index referring to files the embed never captured, which would present as a
// blank page with a console full of 404s.
func TestShellReferencesOnlyExistingAssets(t *testing.T) {
	h, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	shell := string(h.index)

	for _, ref := range []string{"./app.js", "./styles.css", "./manifest.webmanifest"} {
		if !strings.Contains(shell, ref) {
			t.Errorf("shell does not reference %s", ref)
		}
		name := strings.TrimPrefix(ref, "./")
		if _, err := h.fs.Open(name); err != nil {
			t.Errorf("shell references %s but it is not in the embedded bundle: %v", ref, err)
		}
	}
}
