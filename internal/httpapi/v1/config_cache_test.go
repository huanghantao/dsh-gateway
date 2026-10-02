package v1

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// TestCatalogSurvivesARestart covers the window the operator actually hits:
// every restart of the gateway, before any session has attached again.
//
// ACP answers "what models do you offer" only as part of creating or resuming a
// session, so a process that has not attached one knows nothing — and the app,
// whose pickers are built from that answer, can then offer only "Leave
// unchanged". Remembering the last observation is what keeps the model picker
// usable, and the configured default selectable, across a restart.
func TestCatalogSurvivesARestart(t *testing.T) {
	ts := newTestServer(t)
	workspace := ts.deps.Config.Workspaces[0]

	// Attach a session, which is how the catalog is learned in the first place.
	rec := ts.do(http.MethodPost, "/api/v1/sessions", withCookie(ts.token),
		withJSON(`{"workspace":"`+workspace+`"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /sessions returned %d: %s", rec.Code, rec.Body.String())
	}

	// A new process over the same state directory, with nothing attached yet to
	// it. This is the state the operator's phone asks GET /models in.
	restarted, err := New(ts.deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	found := false
	for _, option := range restarted.cfgCache.get() {
		if option.ID != "model" {
			continue
		}
		found = true
		if option.Current != "m1" {
			t.Errorf("remembered model current = %q, want m1", option.Current)
		}
		if len(option.Options) != 1 || option.Options[0].ID != "m1" {
			t.Errorf("remembered model values = %+v, want the one observed value", option.Options)
		}
	}
	if !found {
		t.Fatal("the restarted gateway remembered no model option at all")
	}
}

// TestUnreadableCatalogIsNotFatal pins the failure mode: a cache is a
// convenience, and a gateway that refuses to start because one is corrupt would
// be trading a working agent for a picker.
func TestUnreadableCatalogIsNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	logger := logx.Discard()

	for name, content := range map[string]string{
		"truncated json":  `{"version":1,"catalog":[`,
		"unknown version": `{"version":99,"catalog":[{"id":"model","current":"m1"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			var cache configCache
			cache.open(path, logger)
			if got := cache.get(); len(got) != 0 {
				t.Errorf("cache.get() = %+v, want an empty catalog", got)
			}
		})
	}
}
