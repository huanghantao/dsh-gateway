// Package config defines the gateway's complete configuration surface and the
// precedence rules that resolve it: built-in defaults, then the YAML file, then
// DSH_GATEWAY_* environment variables, then command-line flags.
//
// Configuration is validated once at startup. A value that would weaken a
// security invariant (for example binding a non-loopback address) is rejected
// here rather than silently honoured later.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML can express it as "30s" or "5m" instead
// of a raw nanosecond count.
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML renders the duration as a Go duration string.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Sandbox modes accepted by DSH. The names are the harness's, not the
// gateway's, because the value is passed straight through in DSH_PERMISSION_MODE.
const (
	// SandboxReadOnly lets the agent read but not modify anything.
	SandboxReadOnly = "read-only"
	// SandboxWorkspaceWrite confines writes to the session workspace. It is the
	// harness's own default and the gateway's, because it is the only mode under
	// which approval prompts are both meaningful and sufficient.
	SandboxWorkspaceWrite = "workspace-write"
	// SandboxFullAccess disables the sandbox and, as a side effect, disables
	// approval prompts entirely. The gateway warns at startup when it is set,
	// because it silently removes the human from the loop.
	SandboxFullAccess = "danger-full-access"
)

// Config is the fully resolved gateway configuration.
type Config struct {
	// warnings records settings an environment variable overrode. They are not
	// errors: the environment is allowed to win, it just has to say so.
	warnings []string

	// Listen is the TCP address the gateway binds. It must resolve to a
	// loopback address; see Validate.
	Listen string `yaml:"listen"`
	// PublicURL is the externally reachable base URL, used to build the pairing
	// link and to decide whether cookies may be marked Secure.
	PublicURL string `yaml:"publicURL"`
	// StateDir holds device records, the audit log, and the pairing secret.
	StateDir string `yaml:"stateDir"`

	Log        Log        `yaml:"log"`
	DSH        DSH        `yaml:"dsh"`
	Auth       Auth       `yaml:"auth"`
	Session    Session    `yaml:"session"`
	Limits     Limits     `yaml:"limits"`
	DesktopUI  DesktopUI  `yaml:"desktopUI"`
	Workspaces []string   `yaml:"workspaces"`
	Transcript Transcript `yaml:"transcript"`
	Push       Push       `yaml:"push"`
	Receipt    Receipt    `yaml:"receipt"`
	Curation   Curation   `yaml:"curation"`
}

// Log configures the process logger.
type Log struct {
	Level     string `yaml:"level"`
	Format    string `yaml:"format"`
	AddSource bool   `yaml:"addSource"`
}

// DSH describes the managed DeepSeek Harness child process.
type DSH struct {
	// Binary is the dsh executable. Looked up on PATH when not absolute.
	Binary string `yaml:"binary"`
	// Home is the DSH_HOME handed to the child; must match the desktop install
	// so that sessions are shared.
	Home string `yaml:"home"`
	// Profile is the ACP profile name. The shipped default is "acp".
	Profile string `yaml:"profile"`
	// SandboxMode maps to DSH_PERMISSION_MODE.
	//
	// Despite the environment variable's name, DSH uses this value as the
	// *sandbox mode*, and derives the approval policy from it:
	// `danger-full-access` becomes "never" (no prompts at all) and every other
	// value becomes "ask". Verified in the harness's own base composition.
	//
	// That derivation is why the gateway always sets this explicitly rather than
	// letting the child inherit the operator's shell environment: an exported
	// `danger-full-access` would silently turn every approval prompt into an
	// automatic yes, and the entire approval surface would appear to work while
	// never asking anything.
	SandboxMode string `yaml:"sandboxMode"`
	// StartTimeout bounds initialize.
	StartTimeout Duration `yaml:"startTimeout"`
	// StopTimeout bounds graceful shutdown before the child is killed.
	StopTimeout Duration `yaml:"stopTimeout"`
	// RestartBackoff is the first restart delay; it doubles up to MaxRestartBackoff.
	RestartBackoff Duration `yaml:"restartBackoff"`
	// MaxRestartBackoff caps the supervisor's exponential backoff.
	MaxRestartBackoff Duration `yaml:"maxRestartBackoff"`
	// ExtraEnv is appended to the child environment, as KEY=VALUE.
	ExtraEnv []string `yaml:"extraEnv"`
}

// Auth configures pairing, sessions, and client-address trust.
type Auth struct {
	// CookieName is the session cookie name. It must not collide with the name
	// DSH derives for its own GUI cookie.
	CookieName string `yaml:"cookieName"`
	// SessionTTL is how long a device stays signed in.
	SessionTTL Duration `yaml:"sessionTTL"`
	// PairingTTL bounds how long a pairing code stays valid.
	PairingTTL Duration `yaml:"pairingTTL"`
	// TrustedProxies lists CIDRs whose X-Forwarded-For / X-Forwarded-Proto
	// headers are believed. Anything outside this set cannot spoof them.
	TrustedProxies []string `yaml:"trustedProxies"`
	// MaxFailures locks a client out after this many failed pairing attempts.
	MaxFailures int `yaml:"maxFailures"`
	// LockoutBase is the first lockout duration; it doubles per further failure.
	LockoutBase Duration `yaml:"lockoutBase"`
	// MaxLockout caps the lockout duration.
	MaxLockout Duration `yaml:"maxLockout"`
	// RequestsPerSecond and Burst configure the per-client token bucket.
	RequestsPerSecond float64 `yaml:"requestsPerSecond"`
	Burst             int     `yaml:"burst"`
}

// Session tunes the session lease that avoids DSH's single-writer file lock.
type Session struct {
	// IdleTimeout releases a resumed session once no client is watching it, so
	// the desktop can open it again.
	IdleTimeout Duration `yaml:"idleTimeout"`
	// PromptTimeout bounds one turn.
	PromptTimeout Duration `yaml:"promptTimeout"`
	// ApprovalTimeout bounds how long an approval waits for a human; on expiry
	// the request is rejected (fail closed).
	ApprovalTimeout Duration `yaml:"approvalTimeout"`
	// DefaultModel and DefaultReasoningEffort are what a new session starts
	// with when the client does not choose — the app's "Gateway default".
	//
	// The values are ACP option value ids, which are opaque and defined by the
	// harness, so the model one looks like `["provider","model"]`. They are not
	// validated at startup: the gateway cannot know the catalog before a session
	// exists, and a stale id should cost a warning and the harness's own
	// default, not a gateway that refuses to start. GET /models reports them
	// back, and the app preselects them so the sheet says what will happen.
	//
	// Empty leaves the choice to the harness.
	DefaultModel           string `yaml:"defaultModel"`
	DefaultReasoningEffort string `yaml:"defaultReasoningEffort"`
	// EventBuffer is the per-subscriber queue depth before the slowest consumer
	// starts losing the oldest events.
	EventBuffer int `yaml:"eventBuffer"`
	// ReplayBuffer is how many recent events are retained for reconnect replay.
	ReplayBuffer int `yaml:"replayBuffer"`
}

// Limits bounds request handling.
type Limits struct {
	MaxBodyBytes       int64    `yaml:"maxBodyBytes"`
	ReadHeaderTimeout  Duration `yaml:"readHeaderTimeout"`
	ReadTimeout        Duration `yaml:"readTimeout"`
	WriteTimeout       Duration `yaml:"writeTimeout"`
	IdleTimeout        Duration `yaml:"idleTimeout"`
	ShutdownTimeout    Duration `yaml:"shutdownTimeout"`
	MaxPromptBytes     int      `yaml:"maxPromptBytes"`
	MaxConcurrentTurns int      `yaml:"maxConcurrentTurns"`
}

// DesktopUI exposes the stock DSH browser GUI behind the gateway.
type DesktopUI struct {
	// Enabled is off by default: the GUI can drive arbitrary tools and has no
	// authentication of its own, so it is the largest attack surface.
	Enabled bool `yaml:"enabled"`
	// Upstream is the loopback address of `dsh web`.
	Upstream string `yaml:"upstream"`
}

// Push controls Web Push notifications to the phones this gateway serves.
type Push struct {
	// Enabled turns notifications on. The gateway still accepts subscriptions
	// when it is off, so turning it back on does not require the phones to
	// subscribe again.
	Enabled bool `yaml:"enabled"`
	// Subject is the contact a push service can reach the operator at. Some
	// services (Apple's, notably) refuse to deliver without one, so a
	// syntactically valid default is shipped rather than an empty value.
	Subject string `yaml:"subject"`
	// TurnThreshold is how long a turn must run before its end is worth an
	// interruption: a phone that buzzes for every short answer is a phone whose
	// notifications get switched off.
	TurnThreshold Duration `yaml:"turnThreshold"`
	// IncludeSessionName puts the session's title — or, when it has none, the
	// first line of the prompt that opened it — into the notification body.
	//
	// Off by default, because a notification body is not private the way the app
	// is: it is rendered on a lock screen, stored by the operating system, and
	// mirrored to any chat webhook that is configured. What it would carry is
	// text the operator typed, so it is opt-in rather than assumed.
	IncludeSessionName bool `yaml:"includeSessionName"`
	// Webhooks are chat channels that receive the same notifications. They exist
	// because Web Push on Android is Google's push service and nothing else, so a
	// phone that cannot reach it — no Google Play services, or a network that
	// cannot route to it — can never be notified by any amount of work on this
	// end. A chat client has its own delivery path and works anywhere.
	//
	// Empty by default, and the same reasoning as IncludeSessionName applies with
	// more force: a webhook posts in clear to a third party.
	Webhooks []PushWebhook `yaml:"webhooks"`
}

// Curation tunes the "what can be tidied" suggestions on the sessions screen.
//
// Both settings exist because "this is a test run" is a local definition. The
// shipped rules are the portable ones — the sentences any script says to an
// agent, and the scratch directories the runtime reports — and a deployment
// whose harness leaves its own markers teaches them here rather than having them
// guessed at in the source.
type Curation struct {
	// TestTitlePatterns are extra Go regular expressions, ORed with the built-in
	// set, that recognise a title only an automated run would produce. They are
	// only ever *evidence*: triage still requires the session to be short.
	TestTitlePatterns []string `yaml:"testTitlePatterns"`
	// TempRoots replaces the scratch-directory list. Empty keeps the default,
	// which is the platform's temp directory plus the usual suspects.
	TempRoots []string `yaml:"tempRoots"`
}

// Receipt controls what a session's "what did it do" panel can say.
type Receipt struct {
	// Pricing is how a session's token counts become money. It is configured
	// rather than fetched: the authoritative numbers come from the account
	// (`arkcli pricing models --model …` prints them, discount included), the
	// daemon has no business shelling out to a CLI that holds credentials, and a
	// price that changed under an old session would rewrite history.
	Pricing []ModelPrice `yaml:"pricing"`
}

// ModelPrice is one model's unit prices, per million tokens.
type ModelPrice struct {
	// Model matches the model a session selected, with or without its provider
	// prefix ("deepseek/deepseek-v4.1-flash" and "deepseek-v4.1-flash" both match).
	Model    string  `yaml:"model"`
	Currency string  `yaml:"currency"`
	Input    float64 `yaml:"inputPerMillion"`
	Output   float64 `yaml:"outputPerMillion"`
	// CacheRead is what a cached prompt token costs, which is usually a fraction
	// of a fresh one — and on a long session most tokens are cached, so a table
	// without it overstates the bill by a lot.
	CacheRead float64 `yaml:"cacheReadPerMillion"`
}

// PushWebhook is one chat channel.
type PushWebhook struct {
	// Kind selects the message format. "feishu" is the one this build knows.
	Kind string `yaml:"kind"`
	// URL is the bot's incoming-webhook address, which is a capability: anyone
	// holding it can post to that chat.
	URL string `yaml:"url"`
}

// Transcript controls reading of the persisted session log.
type Transcript struct {
	// Enabled turns on the read-only history projection.
	Enabled bool `yaml:"enabled"`
	// SessionsDir overrides the derived session-log root. Empty means
	// <dsh.home>/sessions.
	SessionsDir string `yaml:"sessionsDir"`
	// PageSize is how many transcript items a page holds when the caller asks
	// for no limit of its own. An item carries a whole tool output or a whole
	// reasoning block, so this is a response-size decision as much as a paging
	// one: a large page ships history the reader never scrolls to.
	PageSize int `yaml:"pageSize"`
	// Follow streams what other DSH processes append to a session log, so a
	// session running on the desktop is live on the phone rather than appearing
	// only when the reader refreshes. It reads the same files the projection
	// does, so it is off whenever the projection is off.
	Follow bool `yaml:"follow"`
	// FollowInterval is how often those logs are re-read. Lower is more live and
	// more work; a session being written appends a handful of events per second
	// at most, so the shipped interval is well inside the noise.
	FollowInterval Duration `yaml:"followInterval"`
}

// DefaultPushSubject is the VAPID contact shipped when the operator configures
// none, and DefaultPushSubjectShort is the same value in the form a message
// needs it.
//
// It exists because push services require the claim to be non-empty, and it is
// a *placeholder*: RFC 8292 asks for a way to contact whoever is sending, and
// localhost reaches nobody. Whether a given service enforces that is not
// something this project can determine, which is why the value is accepted and
// warned about rather than rejected.
const (
	DefaultPushSubject      = "mailto:dsh-gateway@localhost"
	DefaultPushSubjectShort = "dsh-gateway@localhost"
)

// validPushSubject reports whether a VAPID subject is a URI a push service will
// accept.
//
// RFC 8292 permits a mailto: or https: URI and nothing else. The mistake this
// catches is a bare address — "you@example.com" reads like a contact to a human
// and like a malformed claim to a push service, which answers 403 with no
// explanation of which field it disliked.
func validPushSubject(subject string) bool {
	u, err := url.Parse(strings.TrimSpace(subject))
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "mailto":
		return strings.TrimSpace(u.Opaque+u.Path) != ""
	case "https":
		return u.Host != ""
	default:
		return false
	}
}

// Default returns the configuration used when nothing is configured.
func Default() Config {
	return Config{
		Listen:    "127.0.0.1:8787",
		PublicURL: "",
		StateDir:  "~/.dsh-gateway",
		Log: Log{
			Level:  "info",
			Format: "text",
		},
		DSH: DSH{
			Binary:            "dsh",
			Home:              "~/.dsh",
			Profile:           "acp",
			SandboxMode:       SandboxWorkspaceWrite,
			StartTimeout:      Duration(90 * time.Second),
			StopTimeout:       Duration(15 * time.Second),
			RestartBackoff:    Duration(1 * time.Second),
			MaxRestartBackoff: Duration(60 * time.Second),
		},
		Auth: Auth{
			CookieName:        "dsh_gw_session",
			SessionTTL:        Duration(30 * 24 * time.Hour),
			PairingTTL:        Duration(10 * time.Minute),
			TrustedProxies:    []string{"127.0.0.1/32", "::1/128"},
			MaxFailures:       5,
			LockoutBase:       Duration(30 * time.Second),
			MaxLockout:        Duration(1 * time.Hour),
			RequestsPerSecond: 8,
			Burst:             24,
		},
		Session: Session{
			IdleTimeout:     Duration(5 * time.Minute),
			PromptTimeout:   Duration(30 * time.Minute),
			ApprovalTimeout: Duration(5 * time.Minute),
			EventBuffer:     256,
			ReplayBuffer:    1024,
		},
		Limits: Limits{
			MaxBodyBytes:       1 << 20, // 1 MiB: prompts are text, not uploads
			ReadHeaderTimeout:  Duration(10 * time.Second),
			ReadTimeout:        Duration(60 * time.Second),
			WriteTimeout:       Duration(0), // streaming responses and websockets own their deadlines
			IdleTimeout:        Duration(120 * time.Second),
			ShutdownTimeout:    Duration(20 * time.Second),
			MaxPromptBytes:     256 << 10,
			MaxConcurrentTurns: 4,
		},
		DesktopUI: DesktopUI{
			Enabled:  false,
			Upstream: "http://127.0.0.1:3080",
		},
		Push: Push{
			Enabled:       true,
			Subject:       DefaultPushSubject,
			TurnThreshold: Duration(2 * time.Minute),
		},
		Transcript: Transcript{
			Enabled:        true,
			PageSize:       50,
			Follow:         true,
			FollowInterval: Duration(time.Second),
		},
	}
}

// Load reads a YAML file over the defaults. A missing file is not an error: the
// gateway must be able to start on defaults alone.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	// path is the operator-supplied configuration location, by definition.
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied configuration path
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return cfg, nil
}

// Warnings reports settings that an environment variable overrode.
//
// Two sources of truth for one setting is how a deployment quietly uses the
// wrong address: the file says one thing, the launchd job's environment says
// another, and the environment wins without saying so. Nothing here is fatal —
// the value is deterministic and documented — but a startup line naming both is
// the difference between a five-minute diagnosis and an afternoon.
func (c Config) Warnings() []string {
	return append([]string(nil), c.warnings...)
}

// noteOverride records that an environment variable replaced a different value.
func (c *Config) noteOverride(name, from, to string) {
	c.warnings = append(c.warnings, fmt.Sprintf(
		"%s: the environment overrides the configured value (%q in the file, %q in effect); "+
			"if that is a surprise, whatever started the gateway is carrying a stale value",
		name, from, to))
}

// ApplyEnv overlays DSH_GATEWAY_* variables. getenv is injected so tests need no
// process environment.
//
// Only a deliberately small set of variables is honoured; everything else is
// file- or flag-configured, because environment is the easiest place for a
// secret or an unsafe override to hide.
func (c *Config) ApplyEnv(getenv func(string) string) error {
	if v := getenv("DSH_GATEWAY_LISTEN"); v != "" {
		if c.Listen != "" && c.Listen != v {
			c.noteOverride("listen", c.Listen, v)
		}
		c.Listen = v
	}
	if v := getenv("DSH_GATEWAY_PUBLIC_URL"); v != "" {
		if c.PublicURL != "" && c.PublicURL != v {
			c.noteOverride("publicURL", c.PublicURL, v)
		}
		c.PublicURL = v
	}
	if v := getenv("DSH_GATEWAY_STATE_DIR"); v != "" {
		if c.StateDir != "" && c.StateDir != v {
			c.noteOverride("stateDir", c.StateDir, v)
		}
		c.StateDir = v
	}
	if v := getenv("DSH_GATEWAY_LOG_LEVEL"); v != "" {
		if c.Log.Level != "" && c.Log.Level != v {
			c.noteOverride("log.level", c.Log.Level, v)
		}
		c.Log.Level = v
	}
	if v := getenv("DSH_GATEWAY_LOG_FORMAT"); v != "" {
		c.Log.Format = v
	}
	if v := getenv("DSH_GATEWAY_DSH_BINARY"); v != "" {
		c.DSH.Binary = v
	}
	if v := getenv("DSH_GATEWAY_DSH_HOME"); v != "" {
		c.DSH.Home = v
	}
	if v := getenv("DSH_GATEWAY_DSH_SANDBOX_MODE"); v != "" {
		c.DSH.SandboxMode = v
	}
	if v := getenv("DSH_GATEWAY_EXPOSE_DESKTOP_UI"); v != "" {
		c.DesktopUI.Enabled = v == "1" || strings.EqualFold(v, "true")
	}
	return nil
}

// Expand resolves a leading ~ and makes the path absolute.
func Expand(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("config: resolve home directory: %w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("config: make %s absolute: %w", path, err)
	}
	return abs, nil
}

// Resolve normalises every path-valued field in place. Call it after Load and
// ApplyEnv, before Validate.
func (c *Config) Resolve() error {
	var err error
	if c.StateDir, err = Expand(c.StateDir); err != nil {
		return err
	}
	if c.DSH.Home, err = Expand(c.DSH.Home); err != nil {
		return err
	}
	if c.Transcript.SessionsDir != "" {
		if c.Transcript.SessionsDir, err = Expand(c.Transcript.SessionsDir); err != nil {
			return err
		}
	}
	for i, ws := range c.Workspaces {
		if c.Workspaces[i], err = Expand(ws); err != nil {
			return err
		}
	}
	return nil
}

// ValidateWorkspaces checks the allowlist after Resolve has made every entry
// absolute.
//
// It runs separately from Validate because Resolve must happen first, and both
// matter: a relative entry would be anchored to the process working directory,
// which under launchd or systemd is not the directory the operator had in mind.
// Silently resolving it to somewhere unexpected is exactly the kind of quiet
// surprise an allowlist must not have.
func (c Config) ValidateWorkspaces() error {
	var problems []error
	seen := map[string]int{}
	for i, ws := range c.Workspaces {
		switch {
		case ws == "":
			problems = append(problems, fmt.Errorf("workspaces[%d]: an empty entry is not a path", i))
			continue
		case !filepath.IsAbs(ws):
			problems = append(problems, fmt.Errorf(
				"workspaces[%d]: %q is not absolute; a relative path would be anchored to the "+
					"process working directory, which is not what you meant", i, ws))
			continue
		}
		if first, dup := seen[ws]; dup {
			problems = append(problems, fmt.Errorf(
				"workspaces[%d]: %q duplicates workspaces[%d]", i, ws, first))
			continue
		}
		seen[ws] = i
	}
	return errors.Join(problems...)
}

// SessionsDir returns the effective session-log root.
func (c Config) SessionsDir() string {
	if c.Transcript.SessionsDir != "" {
		return c.Transcript.SessionsDir
	}
	return filepath.Join(c.DSH.Home, "sessions")
}

// SecureCookies reports whether the public URL is HTTPS, which decides the
// Secure attribute on the session cookie.
func (c Config) SecureCookies() bool {
	u, err := url.Parse(c.PublicURL)
	return err == nil && u.Scheme == "https"
}

// Validate enforces every startup invariant. Errors are collected so an operator
// sees all problems at once instead of fixing them one restart at a time.
func (c Config) Validate() error {
	var problems []error
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	// The gateway is the only thing standing between the internet and an agent
	// that runs arbitrary commands. Binding it to a public interface would put
	// it directly on the internet with no TLS of its own, so it is refused.
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		fail("listen: %q is not a host:port address: %v", c.Listen, err)
	} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		fail("listen: %q is not a loopback address; the gateway must sit behind "+
			"the frp tunnel and a TLS terminator, never on a public interface", c.Listen)
	}

	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		switch {
		case err != nil:
			fail("publicURL: %q is not a valid URL: %v", c.PublicURL, err)
		case u.Scheme != "https" && u.Scheme != "http":
			fail("publicURL: scheme must be http or https, got %q", u.Scheme)
		case u.Host == "":
			fail("publicURL: %q has no host", c.PublicURL)
		}
	}

	if c.StateDir == "" {
		fail("stateDir: must not be empty")
	}
	if c.DSH.Binary == "" {
		fail("dsh.binary: must not be empty")
	}
	if c.DSH.Home == "" {
		fail("dsh.home: must not be empty")
	}
	if c.DSH.Profile == "" {
		fail("dsh.profile: must not be empty")
	}
	switch c.DSH.SandboxMode {
	case SandboxReadOnly, SandboxWorkspaceWrite, SandboxFullAccess:
	case "":
		fail("dsh.sandboxMode: must not be empty")
	default:
		fail("dsh.sandboxMode: unknown mode %q; expected one of %s, %s, %s",
			c.DSH.SandboxMode, SandboxReadOnly, SandboxWorkspaceWrite, SandboxFullAccess)
	}
	if c.DSH.StartTimeout <= 0 {
		fail("dsh.startTimeout: must be positive")
	}
	if c.DSH.StopTimeout <= 0 {
		fail("dsh.stopTimeout: must be positive")
	}
	if c.DSH.RestartBackoff <= 0 || c.DSH.MaxRestartBackoff < c.DSH.RestartBackoff {
		fail("dsh.restartBackoff/maxRestartBackoff: require 0 < backoff <= maxRestartBackoff")
	}

	if c.Auth.CookieName == "" {
		fail("auth.cookieName: must not be empty")
	}
	if strings.ContainsAny(c.Auth.CookieName, " ;,=\"\\") {
		fail("auth.cookieName: %q contains characters that are illegal in a cookie name", c.Auth.CookieName)
	}
	if c.Auth.SessionTTL <= 0 {
		fail("auth.sessionTTL: must be positive")
	}
	if c.Auth.PairingTTL <= 0 {
		fail("auth.pairingTTL: must be positive")
	}
	if c.Auth.PairingTTL.Std() > time.Hour {
		fail("auth.pairingTTL: %s is too long for a one-time code", c.Auth.PairingTTL.Std())
	}
	for _, proxy := range c.Auth.TrustedProxies {
		// A bare address is accepted and treated as a single host, matching what
		// httpcore.NewClientIPResolver does. Requiring a /32 here while the
		// resolver happily widened it was an asymmetry with no security value:
		// this list is written by the operator, and the failure mode of the two
		// layers disagreeing is a confusing startup error rather than a
		// protection.
		if _, _, err := net.ParseCIDR(proxy); err == nil {
			continue
		}
		if ip := net.ParseIP(proxy); ip != nil {
			continue
		}
		fail("auth.trustedProxies: %q is neither an IP address nor a CIDR", proxy)
	}
	if c.Auth.MaxFailures <= 0 {
		fail("auth.maxFailures: must be positive")
	}
	if c.Auth.LockoutBase <= 0 || c.Auth.MaxLockout < c.Auth.LockoutBase {
		fail("auth.lockoutBase/maxLockout: require 0 < lockoutBase <= maxLockout")
	}
	if c.Auth.RequestsPerSecond <= 0 || c.Auth.Burst <= 0 {
		fail("auth.requestsPerSecond/burst: must be positive")
	}

	if c.Session.IdleTimeout <= 0 {
		fail("session.idleTimeout: must be positive; without it a resumed session " +
			"would hold DSH's single-writer lock forever and block the desktop")
	}
	if c.Session.PromptTimeout <= 0 {
		fail("session.promptTimeout: must be positive")
	}
	if c.Session.ApprovalTimeout <= 0 {
		fail("session.approvalTimeout: must be positive")
	}
	if c.Session.EventBuffer < 16 {
		fail("session.eventBuffer: must be at least 16, got %d", c.Session.EventBuffer)
	}
	if c.Session.ReplayBuffer < c.Session.EventBuffer {
		fail("session.replayBuffer: must be >= eventBuffer (%d), got %d",
			c.Session.EventBuffer, c.Session.ReplayBuffer)
	}

	if c.Limits.MaxBodyBytes <= 0 {
		fail("limits.maxBodyBytes: must be positive")
	}
	if c.Limits.MaxPromptBytes <= 0 || int64(c.Limits.MaxPromptBytes) > c.Limits.MaxBodyBytes {
		fail("limits.maxPromptBytes: must be positive and <= maxBodyBytes")
	}
	if c.Limits.MaxConcurrentTurns <= 0 {
		fail("limits.maxConcurrentTurns: must be positive")
	}

	if c.DesktopUI.Enabled {
		u, err := url.Parse(c.DesktopUI.Upstream)
		if err != nil || u.Host == "" {
			fail("desktopUI.upstream: %q is not a valid URL", c.DesktopUI.Upstream)
		} else {
			host, _, err := net.SplitHostPort(u.Host)
			if err != nil {
				host = u.Host
			}
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				fail("desktopUI.upstream: %q must point at a loopback address", c.DesktopUI.Upstream)
			}
		}
	}

	if c.Transcript.PageSize <= 0 {
		fail("transcript.pageSize: must be positive")
	}
	if c.Transcript.Follow && c.Transcript.FollowInterval <= 0 {
		fail("transcript.followInterval: must be positive while transcript.follow is on")
	}
	if c.Push.Enabled {
		if c.Push.TurnThreshold < 0 {
			fail("push.turnThreshold: must not be negative")
		}
		// Checked here rather than at first send: a push service answers a bad
		// subject with a bare 403, and finding out at that point means the
		// operator has already missed the approval it was trying to deliver.
		switch {
		case strings.TrimSpace(c.Push.Subject) == "":
			fail("push.subject: must not be empty while push is enabled; push services " +
				"require a contact URI")
		case !validPushSubject(c.Push.Subject):
			fail("push.subject: %q is not a URI a push service accepts; RFC 8292 allows "+
				"a mailto: or https: URI, e.g. mailto:you@example.com", c.Push.Subject)
		}
	}
	for i, price := range c.Receipt.Pricing {
		switch {
		case price.Model == "":
			fail("receipt.pricing[%d]: a model is required", i)
		case price.Input < 0 || price.Output < 0 || price.CacheRead < 0:
			fail("receipt.pricing[%d]: prices must not be negative", i)
		case price.Currency == "":
			fail("receipt.pricing[%d]: a currency is required (CNY, USD, …)", i)
		}
	}
	for i, hook := range c.Push.Webhooks {
		switch {
		case hook.Kind == "":
			fail("push.webhooks[%d]: a kind is required (feishu)", i)
		case !strings.HasPrefix(hook.URL, "https://"):
			// A webhook carries the capability to post into a chat, so it must
			// not travel in clear — and every provider requires https anyway.
			fail("push.webhooks[%d]: the url must be https", i)
		}
	}

	if len(c.Workspaces) == 0 {
		fail("workspaces: at least one workspace root is required; the ACP client " +
			"must supply an absolute cwd and the gateway only offers allowlisted roots")
	}

	return errors.Join(problems...)
}
