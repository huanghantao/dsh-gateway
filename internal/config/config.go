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
	Changes    Changes    `yaml:"changes"`
}

// Log configures the process logger.
type Log struct {
	// Level is the minimum level to log; an unknown value falls back to info.
	Level string `yaml:"level"`
	// Format selects the handler; an unknown value falls back to text.
	Format string `yaml:"format"`
	// AddSource records the calling file and line on every record.
	AddSource bool `yaml:"addSource"`
}

// DSH describes the managed DeepSeek Harness child process.
type DSH struct {
	// Mode is accepted and ignored.
	//
	// It used to select between running the harness child in the agent host and
	// running it in this process. The second option was removed — a gateway that
	// runs the agent itself loses whatever turn is in flight when it is
	// redeployed, because DeepSeek Harness binds an ACP child to its client's
	// stdin — and the key is kept only so that an old config produces a sentence
	// rather than a silent change in behaviour.
	Mode string `yaml:"mode,omitempty"`
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
	// ApprovalGrantTTL bounds how long a scoped approval lasts. Choosing a
	// scoped option on a prompt — "this tool, in this session" — records a rule
	// that then answers matching requests without asking again, and this is when
	// that rule expires.
	//
	// Zero disables scoped grants entirely: the broker offers only the harness's
	// own allow-once and reject-once, every tool call needs its own answer, and
	// the app does not offer the scoped options at all.
	ApprovalGrantTTL Duration `yaml:"approvalGrantTTL"`
	// PromptQueueDepth is how many prompts may wait behind a running turn in one
	// session. A prompt that arrives while the agent works is queued and runs
	// when the turn finishes, instead of being refused and lost.
	//
	// Zero refuses a mid-turn prompt with 409, which is the strict behaviour a
	// deployment that wants exactly one prompt at a time can ask for.
	PromptQueueDepth int `yaml:"promptQueueDepth"`
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
	// DeployDrainTimeout is how long a shutdown waits for in-flight turns before
	// going ahead without them.
	//
	// It exists because a redeploy and a turn have incompatible clocks: a turn
	// runs for minutes and a deploy wants to be over in seconds. Refusing to
	// wait abandons work the operator asked for; waiting forever means a stuck
	// turn blocks every future deploy. This is the compromise, and it is
	// deliberately much longer than limits.shutdownTimeout — that one bounds
	// cleaning up, this one bounds finishing.
	//
	// Zero means do not wait at all, which is what an operator who wants a
	// redeploy to be instantaneous asks for.
	DeployDrainTimeout Duration `yaml:"deployDrainTimeout"`
}

// Limits bounds request handling.
type Limits struct {
	// MaxBodyBytes caps one request body. The default is 8 MiB rather than the
	// 1 MiB a text-only prompt needed, because a phone photograph is a few
	// megabytes: this ceiling has to clear MaxImageBytes before that can mean
	// anything.
	MaxBodyBytes int64 `yaml:"maxBodyBytes"`
	// ReadHeaderTimeout bounds how long a client may take over its headers.
	ReadHeaderTimeout Duration `yaml:"readHeaderTimeout"`
	// ReadTimeout bounds one request read.
	ReadTimeout Duration `yaml:"readTimeout"`
	// WriteTimeout is 0 by default: streaming responses and the WebSocket own
	// their deadlines.
	WriteTimeout Duration `yaml:"writeTimeout"`
	// IdleTimeout is how long a keep-alive connection may sit idle.
	IdleTimeout Duration `yaml:"idleTimeout"`
	// ShutdownTimeout bounds the graceful shutdown before connections are cut.
	ShutdownTimeout Duration `yaml:"shutdownTimeout"`
	// MaxPromptBytes bounds one prompt's text.
	MaxPromptBytes int `yaml:"maxPromptBytes"`
	// MaxConcurrentTurns caps how many turns run at once across all sessions.
	MaxConcurrentTurns int `yaml:"maxConcurrentTurns"`
	// MaxImageBytes bounds one image prompt block, decoded. Zero — the default —
	// means no bound tighter than the request body limit, which is what actually
	// caps a request in practice: the body carries an image base64-encoded, so it
	// is a third larger on the wire than the number checked here.
	//
	// A positive value caps one image below that, which is a policy about what a
	// prompt may contain rather than a limit on the request.
	MaxImageBytes int `yaml:"maxImageBytes"`
	// ToolInputBytes bounds one tool call's arguments on their way to a phone.
	//
	// The arguments are what the client reads to say what a call did — a path, a
	// command, which two strings an edit replaced — so they are bounded by
	// clipping long string *values* rather than by cutting the document: see
	// toolresult.LimitJSON for why half a JSON payload is worse than a trimmed
	// one. The default holds every ordinary argument and trims a whole-file write.
	ToolInputBytes int `yaml:"toolInputBytes"`
	// ToolOutputBytes bounds one tool call's result. Both ends are kept, because
	// the beginning is where a failure starts and the end is where the summary
	// and the exit status are.
	ToolOutputBytes int `yaml:"toolOutputBytes"`
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
	// IncludeTaskNames lets a notification say which delegated task finished, by
	// the description the model wrote for it.
	//
	// On by default, and a different decision from IncludeSessionName: without
	// it, three subagents finishing in one session produce three identical
	// "Subagent finished" notifications, which is the confusion the notification
	// vocabulary exists to remove. It is still a switch, because a task
	// description is generated text derived from the operator's prompt and a
	// deployment may not want it on a lock screen or mirrored to a chat channel.
	IncludeTaskNames bool `yaml:"includeTaskNames"`
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

// Changes controls the "what did this session change" screen.
//
// Reading is always available while the session log is: the projection answers
// from the agent's own record of its edits and touches nothing in the workspace.
// Writing is not, and that asymmetry is the whole point of this struct.
type Changes struct {
	Revert Revert `yaml:"revert"`
}

// Revert controls undoing a session's edits in the workspace.
//
// Off by default. Turning it on is the only way any part of this gateway writes
// to an operator's files, so a deployment that leaves it off has no such code
// path reachable at all — not a disabled branch, an unconstructed one.
type Revert struct {
	Enabled bool `yaml:"enabled"`
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
			Mode:              DSHModeAgentHost,
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
			IdleTimeout:      Duration(5 * time.Minute),
			PromptTimeout:    Duration(30 * time.Minute),
			ApprovalTimeout:  Duration(5 * time.Minute),
			ApprovalGrantTTL: Duration(30 * time.Minute),
			PromptQueueDepth: 4,
			EventBuffer:      256,
			ReplayBuffer:     1024,
			// Two minutes is long enough for the common case — a turn that is
			// nearly done — and short enough that an operator redeploying at a
			// terminal is not left wondering whether it hung.
			DeployDrainTimeout: Duration(2 * time.Minute),
		},
		Limits: Limits{
			// 8 MiB rather than the 1 MiB a text-only prompt needed: a phone
			// photograph is a few megabytes, and this is the ceiling the body
			// limit has to clear before maxImageBytes can mean anything.
			MaxBodyBytes:       8 << 20,
			ReadHeaderTimeout:  Duration(10 * time.Second),
			ReadTimeout:        Duration(60 * time.Second),
			WriteTimeout:       Duration(0), // streaming responses and websockets own their deadlines
			IdleTimeout:        Duration(120 * time.Second),
			ShutdownTimeout:    Duration(20 * time.Second),
			MaxPromptBytes:     256 << 10,
			MaxConcurrentTurns: 4,
			MaxImageBytes:      0,
			// A tool card is read on a 390px screen, and the card scrolls inside
			// a 45dvh box: 24 KiB of output is already hundreds of lines, and the
			// measurement behind these numbers found single results of 57 KiB.
			// Arguments get more room because the client parses them to describe
			// the call, and a trimmed edit still has to diff.
			ToolInputBytes:  64 << 10,
			ToolOutputBytes: 24 << 10,
		},
		DesktopUI: DesktopUI{
			Enabled:  false,
			Upstream: "http://127.0.0.1:3080",
		},
		Push: Push{
			Enabled:          true,
			Subject:          DefaultPushSubject,
			TurnThreshold:    Duration(2 * time.Minute),
			IncludeTaskNames: true,
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

// SocketPath is where the agent host listens, and where this gateway looks for
// it. It is derived from the state directory so that the two ends cannot be
// configured to disagree — a mismatch there is a silent "host unavailable" that
// looks exactly like a host that is not running.
func (c Config) SocketPath() string {
	return filepath.Join(c.StateDir, AgentHostSocket)
}

// AgentHostSocket is the socket's file name inside the state directory.
const AgentHostSocket = "agent-host.sock"

// DSHModeAgentHost is the only value `dsh.mode` accepts. It exists so that a
// config written by the installer, or by someone following the documentation,
// reads as a deliberate statement rather than a line that does nothing.
const DSHModeAgentHost = "agent-host"

// SessionsDir returns the effective session-log root: where DSH keeps the logs
// this deployment reads history from. It follows DSH_HOME, because the phone and
// the desktop have to see one store.
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
	if c.DSH.Mode != "" && c.DSH.Mode != DSHModeAgentHost {
		// A config that names the removed mode must say so rather than be
		// silently accepted. `in-process` was a real setting, and an operator who
		// chose it is owed a sentence explaining that it is gone and why — not a
		// gateway that starts anyway and behaves differently from what their file
		// asks for.
		fail("dsh.mode: %q is no longer supported. The agent always runs in the agent "+
			"host, because a gateway that runs the agent itself loses whatever turn is in "+
			"flight when it is redeployed. Remove the line, or set it to %q.",
			c.DSH.Mode, DSHModeAgentHost)
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
	if c.Session.DeployDrainTimeout < 0 {
		fail("session.deployDrainTimeout: must not be negative; zero means a " +
			"shutdown does not wait for running turns at all")
	}
	if c.Session.PromptTimeout <= 0 {
		fail("session.promptTimeout: must be positive")
	}
	if c.Session.ApprovalTimeout <= 0 {
		fail("session.approvalTimeout: must be positive")
	}
	if c.Session.ApprovalGrantTTL < 0 {
		fail("session.approvalGrantTTL: must not be negative; use 0 to disable " +
			"scoped grants, which leaves only allow-once and reject-once")
	}
	if c.Session.ApprovalGrantTTL > 0 && c.Session.ApprovalGrantTTL.Std() < c.Session.ApprovalTimeout.Std() {
		// A grant shorter than one approval window would expire while the very
		// request that created it was still open, which reads as the feature
		// being broken rather than as a policy.
		fail("session.approvalGrantTTL: %s is shorter than approvalTimeout %s, so a "+
			"grant would expire before the prompt that created it was answered",
			c.Session.ApprovalGrantTTL.Std(), c.Session.ApprovalTimeout.Std())
	}
	if c.Session.PromptQueueDepth < 0 {
		fail("session.promptQueueDepth: must not be negative; use 0 to refuse a " +
			"prompt that arrives while a turn is running")
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
	if c.Limits.MaxImageBytes < 0 {
		fail("limits.maxImageBytes: must not be negative; use 0 for no bound " +
			"tighter than maxBodyBytes")
	}
	if c.Limits.MaxImageBytes > 0 && int64(c.Limits.MaxImageBytes) > c.Limits.MaxBodyBytes {
		// The body limit is enforced before any handler reads, so a per-image cap
		// above it could never bind. Saying so beats an operator who sets it and
		// concludes the setting does nothing.
		fail("limits.maxImageBytes: %d is above maxBodyBytes (%d), so it could never "+
			"take effect; lower it or use 0", c.Limits.MaxImageBytes, c.Limits.MaxBodyBytes)
	}
	if c.Limits.MaxConcurrentTurns <= 0 {
		fail("limits.maxConcurrentTurns: must be positive")
	}
	if c.Limits.ToolInputBytes <= 0 {
		fail("limits.toolInputBytes: must be positive; a tool card with no arguments " +
			"cannot say what the call did")
	}
	if c.Limits.ToolOutputBytes <= 0 {
		fail("limits.toolOutputBytes: must be positive; use a large value rather than 0 " +
			"to send results whole")
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
