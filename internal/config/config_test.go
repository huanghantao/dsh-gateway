package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/huanghantao/dsh-gateway/internal/config"
)

// validConfig is the defaults plus the one thing they deliberately leave to the
// operator: the allowlisted workspace roots.
func validConfig(t *testing.T) config.Config {
	t.Helper()

	cfg := config.Default()
	cfg.Workspaces = []string{t.TempDir()}
	if err := cfg.Resolve(); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return cfg
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// TestDefaultConfigRequiresAnExplicitWorkspaceAllowlist pins a deliberate
// security choice: config.Default() leaves Workspaces empty, so the built-in defaults
// do not validate on their own. The workspace list is the only thing bounding
// where an agent that can run shell commands may operate, so a convenience
// default would let the gateway silently decide how much of the operator's
// machine the agent can touch. An allowlist with that consequence is written
// down by the person who owns the machine: the installer asks the question, and
// a bare run is told to.
func TestDefaultConfigRequiresAnExplicitWorkspaceAllowlist(t *testing.T) {
	cfg := config.Default()
	if err := cfg.Resolve(); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("config.Default() validated with no workspace root: an unset allowlist must never be read as the whole machine")
	}

	// The missing workspace must be the *only* complaint. If a future default
	// acquires a second fault, this test should name it rather than drown it.
	if !strings.Contains(err.Error(), "workspaces") {
		t.Errorf("config.Default() was rejected for a reason other than the workspace list: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(err.Error()), "\n"); len(lines) != 1 {
		t.Errorf("config.Default() reports %d problems, want exactly 1:\n%s", len(lines), err)
	}

	// The message has to be actionable: it names the field and says the list is
	// an allowlist, because that is the fix the operator has to apply.
	if msg := err.Error(); !strings.Contains(msg, "at least one workspace root is required") {
		t.Errorf("the complaint does not say what is required: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "allowlisted roots") {
		t.Errorf("the complaint does not say the list is an allowlist: %v", err)
	}

	// Answering the one question is all it takes for the defaults to be usable.
	cfg.Workspaces = []string{t.TempDir()}
	if err := cfg.Resolve(); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the defaults plus one explicit workspace do not validate: %v", err)
	}
}

// TestListenMustResolveToLoopback is the single most important invariant in the
// package: a publicly bound gateway puts an agent with shell access on the
// internet with no TLS of its own.
func TestListenMustResolveToLoopback(t *testing.T) {
	tests := []struct {
		name   string
		listen string
		wantOK bool
	}{
		{"a wildcard IPv4 bind listens on every interface", "0.0.0.0:8787", false},
		{"an empty host means every interface", ":8787", false},
		{"a LAN address is reachable from the network", "192.168.1.5:8787", false},
		{"a wildcard IPv6 bind listens on every interface", "[::]:8787", false},
		{"a hostname can resolve anywhere", "localhost:8787", false},
		{"a public address is refused", "203.0.113.9:8787", false},
		{"a bare address is not a listen address", "127.0.0.1", false},
		{"an empty address is not a listen address", "", false},
		{"loopback IPv4", "127.0.0.1:8787", true},
		{"loopback IPv6", "[::1]:8787", true},
		{"the rest of 127.0.0.0/8 is loopback too", "127.0.0.2:1", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Listen = tc.listen

			err := cfg.Validate()
			if tc.wantOK {
				if err != nil {
					t.Fatalf("listen %q was rejected: %v", tc.listen, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("listen %q was accepted; the gateway would be reachable off-host", tc.listen)
			}
			if !strings.Contains(err.Error(), "listen") {
				t.Errorf("listen %q was rejected by some other rule: %v", tc.listen, err)
			}
		})
	}
}

func TestEmptyWorkspaceListIsRejected(t *testing.T) {
	cfg := validConfig(t)
	cfg.Workspaces = nil

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a configuration with no workspace roots was accepted; the agent would have nothing to open")
	}
	if !strings.Contains(err.Error(), "workspaces") {
		t.Errorf("error = %v, want it to name the workspaces", err)
	}
}

// TestWorkspacesMustBeUsableAbsolutePaths covers ValidateWorkspaces, which runs
// after Resolve has expanded ~ and made entries absolute.
//
// The relative case is the one that matters: anchored silently to the process
// working directory — which under launchd or systemd is not the directory the
// operator pictured — it would point an agent with shell access at somewhere
// they never chose.
func TestWorkspacesMustBeUsableAbsolutePaths(t *testing.T) {
	t.Run("empty entries are refused", func(t *testing.T) {
		cfg := config.Default()
		cfg.Workspaces = []string{""}
		if err := cfg.ValidateWorkspaces(); err == nil {
			t.Error("an empty workspace entry was accepted")
		}
	})

	t.Run("relative entries are refused with the reason", func(t *testing.T) {
		cfg := config.Default()
		cfg.Workspaces = []string{"code/api"}
		err := cfg.ValidateWorkspaces()
		if err == nil {
			t.Fatal("a relative workspace entry was accepted")
		}
		// The message must explain why, because "not absolute" alone invites the
		// operator to just prepend a slash without understanding the risk.
		if !strings.Contains(err.Error(), "working directory") {
			t.Errorf("error does not explain the consequence: %v", err)
		}
	})

	t.Run("duplicates are refused and name both positions", func(t *testing.T) {
		cfg := config.Default()
		cfg.Workspaces = []string{"/a/b", "/a/b"}
		err := cfg.ValidateWorkspaces()
		if err == nil {
			t.Fatal("a duplicated workspace entry was accepted")
		}
		if !strings.Contains(err.Error(), "duplicates") {
			t.Errorf("error does not name the duplication: %v", err)
		}
	})

	t.Run("the resolved default shape is accepted", func(t *testing.T) {
		cfg := config.Default()
		cfg.Workspaces = []string{t.TempDir(), "/tmp"}
		if err := cfg.ValidateWorkspaces(); err != nil {
			t.Errorf("absolute, distinct entries were rejected: %v", err)
		}
	})
}

// TestValidateAloneDoesNotInspectWorkspaceShape records that shape checking is
// deliberately a separate step: Validate runs before Resolve, so it cannot judge
// whether an entry is absolute. ValidateWorkspaces does that afterwards, and the
// composition root calls both.
func TestValidateAloneDoesNotInspectWorkspaceShape(t *testing.T) {
	// Validate counts the list and nothing else: an empty or relative entry
	// passes, and Resolve quietly anchors a relative one to the process working
	// directory, which under launchd is not the operator's shell directory.
	// Pinned so that tightening the rule is a visible change.
	cfg := validConfig(t)
	cfg.Workspaces = []string{"", "  ", "relative/root"}

	if err := cfg.Resolve(); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate = %v, want the current lenient acceptance", err)
	}
	if cfg.Workspaces[0] != "" {
		t.Errorf("an empty workspace entry became %q", cfg.Workspaces[0])
	}
	if !filepath.IsAbs(cfg.Workspaces[2]) {
		t.Errorf("Resolve left the relative entry %q relative", cfg.Workspaces[2])
	}
}

// TestTheRemovedInProcessModeIsRefusedRatherThanIgnored guards an upgrade path.
//
// `dsh.mode: in-process` was a real setting: it told the gateway to run the
// DeepSeek Harness child itself, which meant a redeploy ended whatever turn was
// running. It has been removed, and the danger of removing a setting is the
// config file that still names it — silently ignored, it would leave an operator
// believing their gateway behaves one way while it behaves another. So the value
// is refused, and the refusal explains both what happened and what to do.
func TestTheRemovedInProcessModeIsRefusedRatherThanIgnored(t *testing.T) {
	cfg := validConfig(t)
	cfg.DSH.Mode = "in-process"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("`in-process` was accepted; a config asking to run the agent in the " +
			"gateway would be silently ignored, and its author would never learn that " +
			"turns now survive redeploys")
	}
	for _, want := range []string{"dsh.mode", "no longer supported", "agent host"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not tell the operator "+
				"what happened or what to do: %v", want, err)
		}
	}
}

func TestTheAgentHostModeIsAcceptedAndMayBeOmitted(t *testing.T) {
	// Named, because the shipped template names it and a file that is explicit
	// should validate.
	named := validConfig(t)
	named.DSH.Mode = config.DSHModeAgentHost
	if err := named.Validate(); err != nil {
		t.Errorf("the documented value was rejected: %v", err)
	}

	// Absent, because every config written before this setting existed should
	// keep working — and get the behaviour that does not lose work.
	absent := validConfig(t)
	absent.DSH.Mode = ""
	if err := absent.Validate(); err != nil {
		t.Errorf("a config without a mode was rejected: %v", err)
	}
}

func TestSandboxModeRejectsUnknownValues(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		wantOK bool
	}{
		// "ask" is an approval policy, not a sandbox mode. It was a real bug:
		// the two concepts share the DSH_PERMISSION_MODE variable, and a mode
		// the harness does not recognise silently falls back to asking.
		{"ask is an approval policy, not a sandbox mode", "ask", false},
		{"an invented mode", "yolo", false},
		{"an empty mode", "", false},
		{"read-only", config.SandboxReadOnly, true},
		{"workspace-write", config.SandboxWorkspaceWrite, true},
		{"danger-full-access", config.SandboxFullAccess, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.DSH.SandboxMode = tc.mode

			err := cfg.Validate()
			if tc.wantOK {
				if err != nil {
					t.Fatalf("sandboxMode %q was rejected: %v", tc.mode, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("sandboxMode %q was accepted; the value is passed straight to DSH_PERMISSION_MODE", tc.mode)
			}
			if !strings.Contains(err.Error(), "sandboxMode") {
				t.Errorf("sandboxMode %q was rejected by some other rule: %v", tc.mode, err)
			}
		})
	}
}

func TestDesktopUIUpstreamMustBeLoopbackWhenEnabled(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		upstr   string
		wantOK  bool
	}{
		{"loopback when enabled", true, "http://127.0.0.1:3080", true},
		{"loopback without an explicit port", true, "http://127.0.0.1", true},
		{"IPv6 loopback", true, "http://[::1]:3080", true},
		{"a LAN address when enabled", true, "http://192.168.1.5:3080", false},
		{"a public hostname when enabled", true, "http://evil.example.com:3080", false},
		{"an empty upstream when enabled", true, "", false},
		{"a LAN address is not checked while the GUI is off", false, "http://192.168.1.5:3080", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.DesktopUI.Enabled = tc.enabled
			cfg.DesktopUI.Upstream = tc.upstr

			err := cfg.Validate()
			if tc.wantOK {
				if err != nil {
					t.Fatalf("upstream %q (enabled=%v) was rejected: %v", tc.upstr, tc.enabled, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("upstream %q was accepted while the GUI is enabled", tc.upstr)
			}
			if !strings.Contains(err.Error(), "desktopUI.upstream") {
				t.Errorf("upstream %q was rejected by some other rule: %v", tc.upstr, err)
			}
		})
	}
}

func TestPairingTTLIsCappedAtAnHour(t *testing.T) {
	tests := []struct {
		name   string
		ttl    time.Duration
		wantOK bool
	}{
		{"ten minutes, the shipped default", 10 * time.Minute, true},
		{"exactly the cap", time.Hour, true},
		{"one nanosecond past the cap", time.Hour + time.Nanosecond, false},
		{"two hours", 2 * time.Hour, false},
		{"a day", 24 * time.Hour, false},
		{"zero", 0, false},
		{"negative", -time.Minute, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Auth.PairingTTL = config.Duration(tc.ttl)

			err := cfg.Validate()
			if tc.wantOK {
				if err != nil {
					t.Fatalf("pairingTTL %v was rejected: %v", tc.ttl, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("pairingTTL %v was accepted", tc.ttl)
			}
			if !strings.Contains(err.Error(), "pairingTTL") {
				t.Errorf("pairingTTL %v was rejected by some other rule: %v", tc.ttl, err)
			}
		})
	}
}

func TestReplayBufferMustCoverTheEventBuffer(t *testing.T) {
	tests := []struct {
		name          string
		event, replay int
		wantOK        bool
	}{
		{"equal is enough", 100, 100, true},
		{"more is fine", 100, 200, true},
		{"less is rejected: replay would hand the client a gap", 100, 99, false},
		{"an event buffer below the minimum", 15, 99, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Session.EventBuffer = tc.event
			cfg.Session.ReplayBuffer = tc.replay

			err := cfg.Validate()
			if tc.wantOK {
				if err != nil {
					t.Fatalf("eventBuffer=%d replayBuffer=%d was rejected: %v", tc.event, tc.replay, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("eventBuffer=%d replayBuffer=%d was accepted", tc.event, tc.replay)
			}
		})
	}
}

func TestMaxPromptBytesMustNotExceedMaxBodyBytes(t *testing.T) {
	tests := []struct {
		name         string
		body, prompt int64
		wantOK       bool
	}{
		{"equal is allowed", 1024, 1024, true},
		{"smaller is fine", 1024, 512, true},
		{"larger is rejected: the body limit would reject the request first", 1024, 1025, false},
		{"zero is rejected", 1024, 0, false},
		{"negative is rejected", 1024, -1, false},
		{"a non-positive body limit is rejected", 0, 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Limits.MaxBodyBytes = tc.body
			cfg.Limits.MaxPromptBytes = int(tc.prompt)

			err := cfg.Validate()
			if tc.wantOK {
				if err != nil {
					t.Fatalf("maxBodyBytes=%d maxPromptBytes=%d was rejected: %v", tc.body, tc.prompt, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("maxBodyBytes=%d maxPromptBytes=%d was accepted", tc.body, tc.prompt)
			}
		})
	}
}

func TestCookieNameRejectsIllegalCharacters(t *testing.T) {
	tests := []struct {
		name   string
		cookie string
		wantOK bool
	}{
		{"the shipped name", "dsh_gw_session", true},
		{"dots, dashes and underscores are legal", "dsh.gw-session_1", true},
		{"empty", "", false},
		{"a space", "dsh gw", false},
		{"a semicolon", "dsh;gw", false},
		{"a comma", "dsh,gw", false},
		{"an equals sign", "dsh=gw", false},
		{"a double quote", `dsh"gw`, false},
		{"a backslash", `dsh\gw`, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Auth.CookieName = tc.cookie

			err := cfg.Validate()
			if tc.wantOK {
				if err != nil {
					t.Fatalf("cookieName %q was rejected: %v", tc.cookie, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("cookieName %q was accepted", tc.cookie)
			}
			if !strings.Contains(err.Error(), "cookieName") {
				t.Errorf("cookieName %q was rejected by some other rule: %v", tc.cookie, err)
			}
		})
	}
}

func TestTrustedProxiesAcceptBothCIDRsAndBareAddresses(t *testing.T) {
	// Both spellings are accepted, matching httpcore.NewClientIPResolver, which
	// widens a bare address to a single host. Requiring the mask here while the
	// resolver accepted the bare form was an asymmetry with no security value:
	// it produced a confusing startup error rather than a protection.
	cfg := validConfig(t)

	for _, proxies := range [][]string{
		{"127.0.0.1"},
		{"127.0.0.1/32", "::1/128"},
		{"::1"},
		{"10.0.0.0/8", "192.168.1.5"},
	} {
		cfg.Auth.TrustedProxies = proxies
		if err := cfg.Validate(); err != nil {
			t.Errorf("trustedProxies %v were rejected: %v", proxies, err)
		}
	}
}

func TestTrustedProxiesRejectThingsThatAreNeitherIPNorCIDR(t *testing.T) {
	cfg := validConfig(t)

	for _, bad := range []string{"not-an-ip", "999.1.1.1", "10.0.0.0/33", " ", "localhost"} {
		cfg.Auth.TrustedProxies = []string{bad}
		if err := cfg.Validate(); err == nil {
			t.Errorf("trustedProxies %q was accepted", bad)
		}
	}
}

// TestValidateReportsEveryProblemAtOnce is what stops an operator discovering a
// broken configuration one restart at a time.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	cfg := validConfig(t)
	cfg.Listen = "0.0.0.0:8787"
	cfg.DSH.SandboxMode = "ask"
	cfg.Auth.CookieName = "bad name"
	cfg.Auth.PairingTTL = config.Duration(2 * time.Hour)
	cfg.Session.ReplayBuffer = cfg.Session.EventBuffer - 1
	cfg.Limits.MaxPromptBytes = int(cfg.Limits.MaxBodyBytes) + 1

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a configuration with six deliberate faults was accepted")
	}

	want := []string{"listen", "sandboxMode", "cookieName", "pairingTTL", "replayBuffer", "maxPromptBytes"}
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	if len(lines) != len(want) {
		t.Errorf("Validate reported %d problems, want %d:\n%s", len(lines), len(want), err)
	}
	for _, fragment := range want {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("the report never mentions %s:\n%s", fragment, err)
		}
	}
}

func TestYAMLDurationsDecodeFromGoDurationStrings(t *testing.T) {
	path := writeConfig(t, `
listen: "127.0.0.1:9999"
dsh:
  startTimeout: "90s"
  stopTimeout: "250ms"
  restartBackoff: "5s"
  maxRestartBackoff: "1m"
auth:
  sessionTTL: "720h"
  pairingTTL: "5m"
  lockoutBase: "1m30s"
  maxLockout: "2h"
session:
  idleTimeout: "15m"
  promptTimeout: "45m"
  approvalTimeout: "30s"
limits:
  readHeaderTimeout: "10s"
  readTimeout: "60s"
  writeTimeout: "0s"
  idleTimeout: "120s"
  shutdownTimeout: "20s"
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	checks := []struct {
		name string
		got  config.Duration
		want time.Duration
	}{
		{"dsh.startTimeout", cfg.DSH.StartTimeout, 90 * time.Second},
		{"dsh.stopTimeout", cfg.DSH.StopTimeout, 250 * time.Millisecond},
		{"dsh.restartBackoff", cfg.DSH.RestartBackoff, 5 * time.Second},
		{"dsh.maxRestartBackoff", cfg.DSH.MaxRestartBackoff, time.Minute},
		{"auth.sessionTTL", cfg.Auth.SessionTTL, 720 * time.Hour},
		{"auth.pairingTTL", cfg.Auth.PairingTTL, 5 * time.Minute},
		{"auth.lockoutBase", cfg.Auth.LockoutBase, 90 * time.Second},
		{"auth.maxLockout", cfg.Auth.MaxLockout, 2 * time.Hour},
		{"session.idleTimeout", cfg.Session.IdleTimeout, 15 * time.Minute},
		{"session.promptTimeout", cfg.Session.PromptTimeout, 45 * time.Minute},
		{"session.approvalTimeout", cfg.Session.ApprovalTimeout, 30 * time.Second},
		{"limits.readTimeout", cfg.Limits.ReadTimeout, time.Minute},
		{"limits.writeTimeout", cfg.Limits.WriteTimeout, 0},
	}
	for _, c := range checks {
		if got := c.got.Std(); got != c.want {
			t.Errorf("%s = %v, want %v", c.name, got, c.want)
		}
	}

	// The file is applied over the defaults, not instead of them.
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("listen = %q, want the file value", cfg.Listen)
	}
	if cfg.Auth.MaxFailures != config.Default().Auth.MaxFailures {
		t.Errorf("auth.maxFailures = %d, want the default %d", cfg.Auth.MaxFailures, config.Default().Auth.MaxFailures)
	}
	if cfg.Log.Level != config.Default().Log.Level {
		t.Errorf("log.level = %q, want the default %q", cfg.Log.Level, config.Default().Log.Level)
	}
}

// TestSessionDefaultsComeFromTheDocumentedYAMLKeys pins the spelling.
//
// The values are opaque ACP ids the operator copies out of the app's picker or
// `GET /models`, so a typo here fails silently: yaml.v3 drops a key it does not
// recognise, and a new session quietly starts on the harness's own default
// instead. Reading the documented spelling back is what makes that visible.
func TestSessionDefaultsComeFromTheDocumentedYAMLKeys(t *testing.T) {
	path := writeConfig(t, `
listen: "127.0.0.1:9999"
session:
  defaultModel: '["command-code","deepseek/deepseek-v4.1-flash"]'
  defaultReasoningEffort: "max"
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Single-quoted in YAML, so the brackets and quotes are literal: this is
	// one opaque id, not a two-element list.
	if want := `["command-code","deepseek/deepseek-v4.1-flash"]`; cfg.Session.DefaultModel != want {
		t.Errorf("session.defaultModel = %q, want %q", cfg.Session.DefaultModel, want)
	}
	if cfg.Session.DefaultReasoningEffort != "max" {
		t.Errorf("session.defaultReasoningEffort = %q, want %q", cfg.Session.DefaultReasoningEffort, "max")
	}
}

// TestFollowKeysComeFromTheDocumentedYAMLKeys pins the spelling of the keys that
// make a session another process runs visible on the phone. A typo here fails
// quietly: yaml.v3 drops a key it does not recognise, and the follower simply
// never turns on.
func TestFollowKeysComeFromTheDocumentedYAMLKeys(t *testing.T) {
	path := writeConfig(t, `
listen: "127.0.0.1:9999"
transcript:
  follow: false
  followInterval: "3s"
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Transcript.Follow {
		t.Error("transcript.follow: false in the file did not turn it off")
	}
	if got := cfg.Transcript.FollowInterval.Std(); got != 3*time.Second {
		t.Errorf("transcript.followInterval = %v, want 3s", got)
	}

	// On by default: the feature exists because a desk session that only appears
	// after a refresh looks broken, not because an operator asked for it.
	if !config.Default().Transcript.Follow {
		t.Error("transcript.follow defaults to off; a new install would not follow anything")
	}
}

func TestLoadRejectsAnUnparseableDuration(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"a bare number is ambiguous", "auth:\n  sessionTTL: 30\n"},
		{"a duration without a unit", "auth:\n  sessionTTL: \"30\"\n"},
		{"an unknown unit", "auth:\n  sessionTTL: \"30 fortnights\"\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := config.Load(writeConfig(t, tc.body)); err == nil {
				t.Error("Load accepted a duration it cannot interpret")
			}
		})
	}
}

func TestDurationRoundTripsThroughYAML(t *testing.T) {
	var doc struct {
		Timeout config.Duration `yaml:"timeout"`
	}
	if err := yaml.Unmarshal([]byte("timeout: 1m30s\n"), &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := doc.Timeout.Std(); got != 90*time.Second {
		t.Fatalf("decoded %v, want 1m30s", got)
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "timeout: 1m30s" {
		t.Errorf("re-encoded as %q, want %q", got, "timeout: 1m30s")
	}
}

func TestLoadPrecedenceFileOverDefaultsThenEnvironment(t *testing.T) {
	path := writeConfig(t, "listen: \"127.0.0.1:1111\"\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:1111" {
		t.Fatalf("listen = %q, want the file to win over the default", cfg.Listen)
	}

	if err := cfg.ApplyEnv(env(map[string]string{"DSH_GATEWAY_LISTEN": "127.0.0.1:2222"})); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.Listen != "127.0.0.1:2222" {
		t.Errorf("listen = %q, want the environment to win over the file", cfg.Listen)
	}

	// Unset variables must leave the file value alone.
	other, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := other.ApplyEnv(env(nil)); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if other.Listen != "127.0.0.1:1111" {
		t.Errorf("listen = %q, want the file value when the environment is empty", other.Listen)
	}
}

func TestApplyEnvHonoursOnlyTheDocumentedVariables(t *testing.T) {
	// Environment is the easiest place for an unsafe override to hide, so the
	// set of honoured variables is deliberately small. Anything not on the list
	// must be ignored rather than guessed at.
	cfg := config.Default()
	before := cfg

	err := cfg.ApplyEnv(env(map[string]string{
		"DSH_GATEWAY_AUTH_COOKIE_NAME":  "evil",
		"DSH_GATEWAY_TRUSTED_PROXIES":   "0.0.0.0/0",
		"DSH_GATEWAY_MAX_FAILURES":      "0",
		"DSH_GATEWAY_WORKSPACES":        "/",
		"DSH_GATEWAY_LISTEN_UNRELATED":  "0.0.0.0:1",
		"DSH_GATEWAY_DSH_SANDBOX_MODE":  config.SandboxFullAccess,
		"DSH_GATEWAY_EXPOSE_DESKTOP_UI": "true",
	}))
	if err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}

	if cfg.Auth.CookieName != before.Auth.CookieName {
		t.Errorf("cookieName = %q, want %q: the variable is not in the honoured set", cfg.Auth.CookieName, before.Auth.CookieName)
	}
	if len(cfg.Auth.TrustedProxies) != len(before.Auth.TrustedProxies) {
		t.Errorf("trustedProxies = %q, want %q: the variable is not in the honoured set", cfg.Auth.TrustedProxies, before.Auth.TrustedProxies)
	}
	if cfg.Auth.MaxFailures != before.Auth.MaxFailures {
		t.Errorf("maxFailures = %d, want %d: the variable is not in the honoured set", cfg.Auth.MaxFailures, before.Auth.MaxFailures)
	}
	if len(cfg.Workspaces) != 0 {
		t.Errorf("workspaces = %q, want none: the variable is not in the honoured set", cfg.Workspaces)
	}
	if cfg.Listen != before.Listen {
		t.Errorf("listen = %q, want %q: an unrelated variable was read", cfg.Listen, before.Listen)
	}

	// The ones that are honoured must still work.
	if cfg.DSH.SandboxMode != config.SandboxFullAccess {
		t.Errorf("sandboxMode = %q, want the environment value", cfg.DSH.SandboxMode)
	}
	if !cfg.DesktopUI.Enabled {
		t.Error("DSH_GATEWAY_EXPOSE_DESKTOP_UI=true did not enable the desktop GUI")
	}
}

// TestLoadOfAMissingFileKeepsTheDefaults covers the first-run path the installer
// relies on: before any config file exists, Load must hand back the defaults
// rather than an error. The defaults still have no workspace root, so validation
// — not loading — is what asks the operator that one question.
func TestLoadOfAMissingFileKeepsTheDefaults(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	cfg, err := config.Load(missing)
	if err != nil {
		t.Fatalf("Load of a missing file = %v, want the defaults", err)
	}
	if cfg.Listen != config.Default().Listen {
		t.Errorf("listen = %q, want the default %q", cfg.Listen, config.Default().Listen)
	}
	if len(cfg.Workspaces) != 0 {
		t.Errorf("workspaces = %q, want none: a missing file must not invent an allowlist", cfg.Workspaces)
	}

	empty, err := config.Load("")
	if err != nil {
		t.Fatalf("Load(\"\") = %v, want the defaults", err)
	}
	if empty.Listen != config.Default().Listen {
		t.Errorf("listen = %q, want the default %q", empty.Listen, config.Default().Listen)
	}
}

func TestExpandResolvesTildeAgainstTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"a tilde path", "~/x", filepath.Join(home, "x")},
		{"a tilde alone", "~", home},
		{"a nested tilde path", "~/a/b", filepath.Join(home, "a", "b")},
		{"an empty path stays empty", "", ""},
		{"an absolute path is left alone", filepath.Join(home, "y"), filepath.Join(home, "y")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := config.Expand(tc.in)
			if err != nil {
				t.Fatalf("Expand(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Expand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	got, err := config.Expand("relative/path")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Expand(%q) = %q, want an absolute path", "relative/path", got)
	}
}

func TestSecureCookiesIsTrueOnlyForAnHTTPSPublicURL(t *testing.T) {
	tests := []struct {
		name      string
		publicURL string
		want      bool
	}{
		{"https", "https://gw.example.com", true},
		{"https with a port and a path", "https://gw.example.com:8443/api", true},
		{"http", "http://gw.example.com", false},
		{"unset", "", false},
		{"garbage", "://not a url", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.PublicURL = tc.publicURL
			if got := cfg.SecureCookies(); got != tc.want {
				t.Errorf("SecureCookies() with publicURL %q = %v, want %v", tc.publicURL, got, tc.want)
			}
		})
	}
}

func TestSessionsDirFollowsDSHHomeUnlessOverridden(t *testing.T) {
	cfg := config.Default()
	cfg.DSH.Home = filepath.Join(t.TempDir(), "dsh")

	if got, want := cfg.SessionsDir(), filepath.Join(cfg.DSH.Home, "sessions"); got != want {
		t.Errorf("SessionsDir() = %q, want %q", got, want)
	}

	override := filepath.Join(t.TempDir(), "history")
	cfg.Transcript.SessionsDir = override
	if got := cfg.SessionsDir(); got != override {
		t.Errorf("SessionsDir() = %q, want the override %q", got, override)
	}
}

func TestPublicURLMustBeAnAbsoluteHTTPURL(t *testing.T) {
	// publicURL decides whether the session cookie carries Secure and what host
	// the pairing link points at, so a malformed value must not slip through.
	tests := []struct {
		name      string
		publicURL string
		wantOK    bool
	}{
		{"unset is allowed for a loopback-only trial run", "", true},
		{"https", "https://gw.example.com", true},
		{"http is allowed but not Secure", "http://gw.example.com", true},
		{"https with a port and a path", "https://gw.example.com:8443/api", true},
		{"a non-HTTP scheme", "ftp://gw.example.com", false},
		{"a bare host has no scheme", "gw.example.com", false},
		{"a scheme with no host", "https://", false},
		{"not a URL", "://not a url", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.PublicURL = tc.publicURL

			err := cfg.Validate()
			if tc.wantOK {
				if err != nil {
					t.Fatalf("publicURL %q was rejected: %v", tc.publicURL, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("publicURL %q was accepted", tc.publicURL)
			}
			if !strings.Contains(err.Error(), "publicURL") {
				t.Errorf("publicURL %q was rejected by some other rule: %v", tc.publicURL, err)
			}
		})
	}
}

func TestValidateRejectsEmptyAndNonPositiveSettings(t *testing.T) {
	// Each case breaks exactly one field of an otherwise valid configuration and
	// names the field the message must mention, so a failure says which
	// invariant is unguarded.
	tests := []struct {
		name   string
		field  string
		break_ func(*config.Config)
	}{
		{"an empty state directory", "stateDir", func(c *config.Config) { c.StateDir = "" }},
		{"an empty dsh binary", "dsh.binary", func(c *config.Config) { c.DSH.Binary = "" }},
		{"an empty dsh home", "dsh.home", func(c *config.Config) { c.DSH.Home = "" }},
		{"an empty dsh profile", "dsh.profile", func(c *config.Config) { c.DSH.Profile = "" }},
		{"a zero start timeout", "startTimeout", func(c *config.Config) { c.DSH.StartTimeout = 0 }},
		{"a negative stop timeout", "stopTimeout", func(c *config.Config) { c.DSH.StopTimeout = -1 }},
		{"a zero restart backoff", "restartBackoff", func(c *config.Config) { c.DSH.RestartBackoff = 0 }},
		{"a max backoff below the first backoff", "maxRestartBackoff", func(c *config.Config) {
			c.DSH.RestartBackoff = config.Duration(time.Minute)
			c.DSH.MaxRestartBackoff = config.Duration(time.Second)
		}},
		{"zero tolerated failures", "maxFailures", func(c *config.Config) { c.Auth.MaxFailures = 0 }},
		{"a zero lockout base", "lockoutBase", func(c *config.Config) { c.Auth.LockoutBase = 0 }},
		{"a max lockout below the base", "maxLockout", func(c *config.Config) {
			c.Auth.LockoutBase = config.Duration(time.Hour)
			c.Auth.MaxLockout = config.Duration(time.Minute)
		}},
		{"a zero request rate", "requestsPerSecond", func(c *config.Config) { c.Auth.RequestsPerSecond = 0 }},
		{"a zero burst", "burst", func(c *config.Config) { c.Auth.Burst = 0 }},
		{"a zero session idle timeout", "idleTimeout", func(c *config.Config) { c.Session.IdleTimeout = 0 }},
		{"a zero prompt timeout", "promptTimeout", func(c *config.Config) { c.Session.PromptTimeout = 0 }},
		{"a zero approval timeout", "approvalTimeout", func(c *config.Config) { c.Session.ApprovalTimeout = 0 }},
		{"a zero concurrent turn limit", "maxConcurrentTurns", func(c *config.Config) { c.Limits.MaxConcurrentTurns = 0 }},
		{"a zero transcript page size", "pageSize", func(c *config.Config) { c.Transcript.PageSize = 0 }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			tc.break_(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("%s was rejected by some other rule (want %s named): %v", tc.name, tc.field, err)
			}
		})
	}
}

func TestApplyEnvOverlaysEachDocumentedVariable(t *testing.T) {
	// The other half of TestApplyEnvHonoursOnlyTheDocumentedVariables: the
	// variables that are supposed to work must actually work.
	cfg := config.Default()
	vars := map[string]string{
		"DSH_GATEWAY_LISTEN":            "127.0.0.1:9999",
		"DSH_GATEWAY_PUBLIC_URL":        "https://gw.example.com",
		"DSH_GATEWAY_STATE_DIR":         "~/state",
		"DSH_GATEWAY_LOG_LEVEL":         "debug",
		"DSH_GATEWAY_LOG_FORMAT":        "json",
		"DSH_GATEWAY_DSH_BINARY":        "/opt/dsh",
		"DSH_GATEWAY_DSH_HOME":          "~/dsh",
		"DSH_GATEWAY_DSH_SANDBOX_MODE":  config.SandboxReadOnly,
		"DSH_GATEWAY_EXPOSE_DESKTOP_UI": "1",
	}
	if err := cfg.ApplyEnv(env(vars)); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}

	checks := []struct {
		name string
		got  string
		want string
	}{
		{"listen", cfg.Listen, "127.0.0.1:9999"},
		{"publicURL", cfg.PublicURL, "https://gw.example.com"},
		{"stateDir", cfg.StateDir, "~/state"},
		{"log.level", cfg.Log.Level, "debug"},
		{"log.format", cfg.Log.Format, "json"},
		{"dsh.binary", cfg.DSH.Binary, "/opt/dsh"},
		{"dsh.home", cfg.DSH.Home, "~/dsh"},
		{"dsh.sandboxMode", cfg.DSH.SandboxMode, config.SandboxReadOnly},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if !cfg.DesktopUI.Enabled {
		t.Error("DSH_GATEWAY_EXPOSE_DESKTOP_UI=1 did not enable the desktop GUI")
	}
}

func TestResolveFailsWhenTheHomeDirectoryCannotBeResolved(t *testing.T) {
	// A tilde path is only resolvable with a home directory. Failing loudly
	// beats silently leaving "~/.dsh-gateway" as a relative path, which is what
	// a launchd job would then create somewhere unexpected.
	t.Setenv("HOME", "")

	// Every path-valued field must propagate the failure, not just the first.
	tests := []struct {
		name   string
		break_ func(*config.Config)
	}{
		{"stateDir", func(c *config.Config) { c.StateDir = "~/state" }},
		{"dsh.home", func(c *config.Config) {
			c.StateDir = "/tmp/state"
			c.DSH.Home = "~/dsh"
		}},
		{"transcript.sessionsDir", func(c *config.Config) {
			c.StateDir = "/tmp/state"
			c.DSH.Home = "/tmp/dsh"
			c.Transcript.SessionsDir = "~/history"
		}},
		{"workspaces", func(c *config.Config) {
			c.StateDir = "/tmp/state"
			c.DSH.Home = "/tmp/dsh"
			c.Workspaces = []string{"~/code"}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.break_(&cfg)
			if err := cfg.Resolve(); err == nil {
				t.Fatalf("Resolve accepted a tilde in %s with no home directory", tc.name)
			}
		})
	}

	// An empty path stays empty, so this is not the failure being tested.
	if _, err := config.Expand(""); err != nil {
		t.Errorf("Expand(\"\") = %v, want nil", err)
	}
}

func TestLoadReportsAnUnreadablePath(t *testing.T) {
	// Pointing -config at a directory is an easy mistake; it must be an error
	// rather than an empty configuration that silently starts on defaults.
	if _, err := config.Load(t.TempDir()); err == nil {
		t.Error("Load accepted a directory as a config file")
	}
}

// TestApplyEnvAnnouncesAnOverride covers the trap this check exists for: the
// launchd job on a Mac carries DSH_GATEWAY_PUBLIC_URL, and an environment value
// wins over the file. A stale job therefore silently keeps serving the old
// address however correct config.yaml is — which is exactly what happened here,
// and cost an afternoon of "but the file says :8443".
func TestApplyEnvAnnouncesAnOverride(t *testing.T) {
	cfg := config.Default()
	cfg.PublicURL = "https://example.test:8443"
	cfg.StateDir = "/Users/me/.dsh-gateway"

	if err := cfg.ApplyEnv(env(map[string]string{
		"DSH_GATEWAY_PUBLIC_URL": "https://example.test",
	})); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.PublicURL != "https://example.test" {
		t.Errorf("publicURL = %q, want the environment to win", cfg.PublicURL)
	}
	warnings := cfg.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one about publicURL", warnings)
	}
	for _, want := range []string{"publicURL", "https://example.test:8443", "https://example.test"} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("warning %q does not mention %q", warnings[0], want)
		}
	}

	// Agreeing values are not news: the installer writes both from one variable,
	// and a warning on every start would train the operator to ignore them.
	agreed := config.Default()
	agreed.PublicURL = "https://example.test:8443"
	if err := agreed.ApplyEnv(env(map[string]string{"DSH_GATEWAY_PUBLIC_URL": "https://example.test:8443"})); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if got := agreed.Warnings(); len(got) != 0 {
		t.Errorf("warnings = %v, want none when the two agree", got)
	}
}

// TestPushSubjectMustBeAUriAPushServiceAccepts covers the claim RFC 8292 makes
// about the VAPID subject.
//
// A bare address is the mistake worth catching: "you@example.com" reads like a
// contact to a human and like a malformed claim to Apple, which answers 403
// without saying which field it disliked. Finding that out at first send means
// the operator has already missed the approval the notification was for.
func TestPushSubjectMustBeAUriAPushServiceAccepts(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		subject string
		wantErr bool
	}{
		{"the shipped placeholder is accepted, and warned about elsewhere", true, config.DefaultPushSubject, false},
		{"a real mailto is accepted", true, "mailto:ops@example.com", false},
		{"an https contact is accepted", true, "https://example.com/contact", false},
		{"surrounding space is not a scheme", true, "  mailto:ops@example.com  ", false},
		{"a bare address is not a URI", true, "ops@example.com", true},
		{"an empty subject is refused while push is on", true, "", true},
		{"whitespace is not a subject", true, "   ", true},
		{"an unsupported scheme is refused", true, "tel:+15551234", true},
		{"http is not https", true, "http://example.com", true},
		{"a mailto with no address is refused", true, "mailto:", true},
		{"an https with no host is refused", true, "https://", true},
		{"push off means the subject is not inspected", false, "", false},
		{"push off means a bad subject is not inspected either", false, "ops@example.com", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Push.Enabled = tc.enabled
			cfg.Push.Subject = tc.subject
			// The subject is the only thing under test here.
			cfg.Workspaces = []string{"/tmp"}

			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("push.subject %q (enabled=%v) was accepted", tc.subject, tc.enabled)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("push.subject %q (enabled=%v) was rejected: %v", tc.subject, tc.enabled, err)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "push.subject") {
				t.Errorf("the error does not name push.subject: %v", err)
			}
		})
	}
}
