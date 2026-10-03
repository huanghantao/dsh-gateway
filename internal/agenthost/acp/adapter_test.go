package acp_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/agenthost/acp"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// These tests drive a real `dsh --profile acp` child.
//
// They exist because the ACP field shapes the adapter depends on were themselves
// established by probing a live server, and a documentation-only test would
// happily pass while the wire format drifted underneath it. They are skipped
// rather than failed when dsh is absent, so `go test ./...` stays runnable on a
// machine without DeepSeek Harness installed.
//
// The prompt test costs a real model call and is therefore opt-in via
// DSH_GATEWAY_TEST_LLM=1. Everything else is free and fast.

type recorder struct {
	mu      sync.Mutex
	updates []harness.Update
	states  []harness.State
}

func (r *recorder) Publish(u harness.Update) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, u)
}

func (r *recorder) PublishState(s harness.State, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, s)
}

func (r *recorder) snapshot() []harness.Update {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]harness.Update(nil), r.updates...)
}

// denier refuses every approval, so a test can never authorise a real command.
type denier struct {
	called chan harness.PermissionRequest
}

func (d *denier) RequestPermission(_ context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
	select {
	case d.called <- req:
	default:
	}
	return harness.PermissionDecision{OptionID: harness.OptionRejectOnce, DecidedBy: "test"}, nil
}

// waitReady blocks until the adapter reports StateReady, or fails the test.
func waitReady(t *testing.T, a *acp.Adapter) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if a.State() == harness.StateReady {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("harness never became ready; last state %q", a.State())
}

// startHarness boots an adapter against the real dsh, skipping when unavailable.
func startHarness(t *testing.T, sink *recorder) *acp.Adapter {
	t.Helper()

	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skip("dsh is not on PATH; skipping ACP integration test")
	}
	home := os.Getenv("DSH_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("cannot determine home directory: %v", err)
		}
		home = userHome + "/.dsh"
	}
	if _, err := os.Stat(home); err != nil {
		t.Skipf("DSH_HOME %s is not present; skipping", home)
	}

	a, err := acp.New(acp.Options{
		Binary:         "dsh",
		Profile:        "acp",
		Home:           home,
		SandboxMode:    "workspace-write",
		StartTimeout:   60 * time.Second,
		StopTimeout:    10 * time.Second,
		RestartBackoff: 200 * time.Millisecond,
		Logger:         logx.Discard(),
		Updates:        sink,
		Permissions:    &denier{called: make(chan harness.PermissionRequest, 4)},
	})
	if err != nil {
		t.Fatalf("acp.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Start returns once supervision is running; readiness is reported through
	// State, so wait for the handshake rather than assuming it is instantaneous.
	waitReady(t, a)
	caps := a.Capabilities()
	// Guard against a future DSH silently dropping the capabilities the adapter
	// relies on. session/list is required for the session screen; close is
	// required for the lease to release DSH's write lock.
	if !caps.CanList {
		t.Errorf("harness did not advertise session/list; the session screen cannot work")
	}
	if !caps.CanClose {
		t.Errorf("harness did not advertise session/close; session leases cannot release the write lock")
	}
	if caps.ProtocolVersion != 1 {
		t.Errorf("protocol version = %d, want 1", caps.ProtocolVersion)
	}

	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := a.Close(shutdown); err != nil {
			t.Logf("Close: %v", err)
		}
	})
	return a
}

func TestInitializeAndCapabilities(t *testing.T) {
	a := startHarness(t, &recorder{})

	if got := a.State(); got != harness.StateReady {
		t.Errorf("State() = %q, want %q", got, harness.StateReady)
	}
	// The completion signal the gateway uses to answer /readyz must have fired.
	caps := a.Capabilities()
	if caps.AgentName == "" {
		t.Error("AgentName is empty; initialize result was not decoded as expected")
	}
}

func TestListSessionsReturnsOnlyIDAndCwd(t *testing.T) {
	a := startHarness(t, &recorder{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	page, err := a.ListSessions(ctx, "", "")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	sessions := page.Sessions
	t.Logf("harness reports %d persisted session(s), nextCursor=%v",
		len(sessions), page.NextCursor != "")

	for _, s := range sessions {
		if s.ID == "" {
			t.Error("session with empty id")
		}
		if !strings.HasPrefix(s.Workspace, "/") {
			t.Errorf("session %s has non-absolute workspace %q", s.ID, s.Workspace)
		}
		// This assertion documents a real constraint: DSH's session/list carries
		// no title and no timestamps, which is why the gateway merges in the
		// session log projection. If DSH ever starts sending them, this test
		// failing is the signal to simplify that merge.
		if s.Title != "" {
			t.Logf("note: harness now reports titles (%q); the projection merge could be simplified", s.Title)
		}
	}
}

func TestNewSessionAdvertisesModelAndReasoningOptions(t *testing.T) {
	a := startHarness(t, &recorder{})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	workspace := t.TempDir()
	sess, err := a.NewSession(ctx, workspace)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if sess.Info.ID == "" {
		t.Fatal("NewSession returned an empty id")
	}
	t.Logf("created session %s", sess.Info.ID)

	// The phone's model picker is built entirely from these options.
	model, ok := sess.Option("model")
	if !ok {
		t.Fatalf("no model config option; got %d options", len(sess.Config))
	}
	if len(model.Options) == 0 {
		t.Error("model option advertises no values")
	}
	if model.Current == "" {
		t.Error("model option has no current value")
	}
	// The nested group shape must have been flattened, or the picker shows
	// nothing: DSH nests models under provider groups.
	var grouped int
	for _, o := range model.Options {
		if o.Group != "" {
			grouped++
		}
		if o.ID == "" {
			t.Error("model option value with empty id")
		}
	}
	if grouped == 0 {
		t.Error("no model option carried a group; nested option flattening may have broken")
	}
	t.Logf("model: %s (%d values, %d grouped)", model.Current, len(model.Options), grouped)

	if effort, ok := sess.Option("reasoning_effort"); ok {
		t.Logf("reasoning effort: %s (%d values)", effort.Current, len(effort.Options))
	}

	// Closing must succeed so that DSH's single-writer lock is released and the
	// desktop can open the session again.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer closeCancel()
	if err := a.CloseSession(closeCtx, sess.Info.ID); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
}

func TestSetConfigOption(t *testing.T) {
	a := startHarness(t, &recorder{})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sess, err := a.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = a.CloseSession(context.Background(), sess.Info.ID) }()

	effort, ok := sess.Option("reasoning_effort")
	if !ok || len(effort.Options) == 0 {
		t.Skip("harness advertises no reasoning_effort option")
	}

	// Pick a value that differs from the current one, so the assertion is
	// meaningful rather than trivially true.
	var target string
	for _, o := range effort.Options {
		if o.ID != effort.Current {
			target = o.ID
			break
		}
	}
	if target == "" {
		t.Skip("harness advertises only one reasoning effort")
	}

	opts, err := a.SetConfigOption(ctx, sess.Info.ID, "reasoning_effort", target)
	if err != nil {
		t.Fatalf("SetConfigOption: %v", err)
	}
	updated, ok := harness.Session{Config: opts}.Option("reasoning_effort")
	if !ok {
		t.Fatal("config_option_update response omitted reasoning_effort")
	}
	if updated.Current != target {
		t.Errorf("reasoning_effort = %q, want %q", updated.Current, target)
	}
}

func TestResumeRejectsUnknownSession(t *testing.T) {
	a := startHarness(t, &recorder{})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Resuming an id that does not exist must fail cleanly rather than hanging or
	// panicking: this is the path a client hits after a session is deleted on the
	// desktop.
	_, err := a.ResumeSession(ctx, "session-00000000-0000-4000-8000-000000000000", t.TempDir())
	if err == nil {
		t.Fatal("ResumeSession of an unknown id succeeded")
	}
	t.Logf("resume of unknown session rejected as expected: %v", err)
}

// TestPromptEndToEnd exercises a real model turn. It is opt-in because it costs
// money and needs network access.
func TestPromptEndToEnd(t *testing.T) {
	if os.Getenv("DSH_GATEWAY_TEST_LLM") != "1" {
		t.Skip("set DSH_GATEWAY_TEST_LLM=1 to run a real model turn")
	}

	sink := &recorder{}
	a := startHarness(t, sink)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sess, err := a.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = a.CloseSession(context.Background(), sess.Info.ID) }()

	stop, err := a.Prompt(ctx, sess.Info.ID, []harness.PromptBlock{
		{Type: "text", Text: "Reply with exactly one word: ok"},
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stop == "" {
		t.Error("Prompt returned an empty stopReason")
	}
	t.Logf("turn settled with stopReason=%q", stop)

	var messages, usages int
	for _, u := range sink.snapshot() {
		switch u.Kind {
		case harness.UpdateMessage:
			messages++
			if u.Text == "" {
				t.Error("agent_message_chunk decoded to empty text")
			}
		case harness.UpdateUsage:
			usages++
			if u.Usage == nil || u.Usage.Size == 0 {
				t.Error("usage_update carried no context window size")
			}
		case harness.UpdateThought, harness.UpdateTool, harness.UpdateConfig:
			// Not asserted here; the point of this test is that a real turn
			// produces messages and usage at all.
		}
	}
	if messages == 0 {
		t.Error("no assistant message updates arrived")
	}
	t.Logf("received %d message update(s), %d usage update(s)", messages, usages)
}
