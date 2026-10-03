package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/agenthost"
	"github.com/huanghantao/dsh-gateway/internal/agenthost/hostlink"
	"github.com/huanghantao/dsh-gateway/internal/app/approvals"
	"github.com/huanghantao/dsh-gateway/internal/app/bridge"
	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/app/lease"
	"github.com/huanghantao/dsh-gateway/internal/app/lifecycle"
	"github.com/huanghantao/dsh-gateway/internal/app/turns"
	"github.com/huanghantao/dsh-gateway/internal/audit"
	"github.com/huanghantao/dsh-gateway/internal/authn/devicetoken"
	"github.com/huanghantao/dsh-gateway/internal/authn/ratelimit"
	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/curation"
	"github.com/huanghantao/dsh-gateway/internal/edge"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/httpapi/v1"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
	"github.com/huanghantao/dsh-gateway/internal/idgen"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/pairing"
	"github.com/huanghantao/dsh-gateway/internal/push"
	"github.com/huanghantao/dsh-gateway/internal/sessionlog"
	"github.com/huanghantao/dsh-gateway/internal/sessionwatch"
	"github.com/huanghantao/dsh-gateway/internal/workspace"
	"github.com/huanghantao/dsh-gateway/web"
)

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return fmt.Sprintf("%v", []string(*s)) }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// runFlags holds the command-line overrides. Flags win over the file and the
// environment, because the operator who typed one is the most recent authority.
type runFlags struct {
	configPath string
	listen     string
	publicURL  string
	stateDir   string
	logLevel   string
	logFormat  string
	dshBinary  string
	dshHome    string
	exposeUI   bool

	workspaces stringList
}

func runGateway(args []string) error {
	var flags runFlags

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&flags.configPath, "config", "", "path to config.yaml (default: <stateDir>/config.yaml)")
	fs.StringVar(&flags.listen, "listen", "", "loopback address to bind (default 127.0.0.1:8787)")
	fs.StringVar(&flags.publicURL, "public-url", "", "externally reachable base URL, e.g. https://host (required for Secure cookies)")
	fs.StringVar(&flags.stateDir, "state-dir", "", "where devices, the audit log, and the pairing secret live (default ~/.dsh-gateway)")
	fs.StringVar(&flags.logLevel, "log-level", "", "debug, info, warn, or error")
	fs.StringVar(&flags.logFormat, "log-format", "", "text or json")
	fs.StringVar(&flags.dshBinary, "dsh", "", "path to the dsh executable (default: found on PATH)")
	fs.StringVar(&flags.dshHome, "dsh-home", "", "DSH_HOME shared with the desktop install (default ~/.dsh)")
	fs.BoolVar(&flags.exposeUI, "expose-desktop-ui", false, "also reverse-proxy the full DeepSeek Harness web GUI at /")
	fs.Var(&flags.workspaces, "workspace", "an allowed working directory; repeatable")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: dsh-gateway run [flags]\n\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, configPath, err := resolveConfig(flags)
	if err != nil {
		return err
	}

	logger := logx.New(os.Stderr, logx.Config{
		Level:     cfg.Log.Level,
		Format:    logx.Format(cfg.Log.Format),
		AddSource: cfg.Log.AddSource,
	})
	// Naming the file that was actually read removes the most common cause of
	// "my change had no effect": editing a different config.yaml than the one in
	// use.
	logger.Debug("configuration loaded", "path", configPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return serve(ctx, cfg, configPath, logger)
}

// resolveConfig applies the precedence chain and validates the result. It also
// returns the path it consulted so that a validation failure can name the file
// the operator should be editing.
func resolveConfig(flags runFlags) (config.Config, string, error) {
	// The state directory has to be resolved first, because it is where the
	// default config file lives.
	provisional := config.Default()
	if flags.stateDir != "" {
		provisional.StateDir = flags.stateDir
	}
	if err := provisional.Resolve(); err != nil {
		return config.Config{}, "", err
	}

	configPath := flags.configPath
	if configPath == "" {
		configPath = filepath.Join(provisional.StateDir, "config.yaml")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, configPath, err
	}
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return config.Config{}, configPath, err
	}

	// Flags last.
	if flags.listen != "" {
		cfg.Listen = flags.listen
	}
	if flags.publicURL != "" {
		cfg.PublicURL = flags.publicURL
	}
	if flags.stateDir != "" {
		cfg.StateDir = flags.stateDir
	}
	if flags.logLevel != "" {
		cfg.Log.Level = flags.logLevel
	}
	if flags.logFormat != "" {
		cfg.Log.Format = flags.logFormat
	}
	if flags.dshBinary != "" {
		cfg.DSH.Binary = flags.dshBinary
	}
	if flags.dshHome != "" {
		cfg.DSH.Home = flags.dshHome
	}
	if flags.exposeUI {
		cfg.DesktopUI.Enabled = true
	}
	if len(flags.workspaces) > 0 {
		cfg.Workspaces = flags.workspaces
	}

	if err := cfg.Resolve(); err != nil {
		return config.Config{}, configPath, err
	}
	if err := cfg.ValidateWorkspaces(); err != nil {
		return config.Config{}, configPath,
			fmt.Errorf("the configuration at %s is not usable:\n%w", configPath, err)
	}
	if err := cfg.Validate(); err != nil {
		// Every problem at once, so an operator fixes one file rather than
		// discovering issues across a dozen restarts. The path is named because
		// the default location is not obvious and creating the file is the fix
		// for most of these.
		return config.Config{}, configPath,
			fmt.Errorf("the configuration at %s is not usable:\n%w", configPath, err)
	}
	if cfg.PublicURL == "" {
		// Not fatal: a loopback-only trial run is legitimate. But a public
		// deployment without it gets an insecure cookie, so say so loudly.
		fmt.Fprintln(os.Stderr,
			"warning: publicURL is not set; session cookies will not carry the Secure flag "+
				"and the pairing link will point at a loopback address")
	}
	return cfg, configPath, nil
}

// serve runs the gateway: the composition root, where every dependency is built
// in dependency order and passed down explicitly. Nothing reaches for a global,
// so the whole wiring is readable in one place and a test can build any subset.
// configPath is passed in rather than re-derived so that an error message can
// name the file the operator is actually editing — the same reason resolveConfig
// returns it.
func serve(ctx context.Context, cfg config.Config, configPath string, logger *logx.Logger) error {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory %s: %w", cfg.StateDir, err)
	}

	// --- security spine -----------------------------------------------------

	devices, err := devicetoken.Open(filepath.Join(cfg.StateDir, "devices.json"))
	if err != nil {
		return err
	}
	authenticator := devicetoken.New(devices, cfg.Auth.SessionTTL.Std(), time.Now)

	limiter := ratelimit.New(ratelimit.Config{
		PerSecond:    cfg.Auth.RequestsPerSecond,
		Burst:        cfg.Auth.Burst,
		MaxFailures:  cfg.Auth.MaxFailures,
		LockoutBase:  cfg.Auth.LockoutBase.Std(),
		MaxLockout:   cfg.Auth.MaxLockout.Std(),
		IdleEviction: time.Hour,
	}, time.Now)

	pairingService, err := pairing.Open(cfg.StateDir, cfg.Auth.PairingTTL.Std(), authenticator, limiter, time.Now)
	if err != nil {
		return err
	}

	auditLog, err := audit.Open(audit.Config{
		Path: filepath.Join(cfg.StateDir, "audit.jsonl"),
	}, logger, time.Now)
	if err != nil {
		return err
	}
	defer func() { _ = auditLog.Close() }()

	resolver, err := httpcore.NewClientIPResolver(cfg.Auth.TrustedProxies)
	if err != nil {
		return err
	}

	// --- harness ------------------------------------------------------------

	bus := events.New(events.Config{
		Replay: cfg.Session.ReplayBuffer,
		Queue:  cfg.Session.EventBuffer,
	})

	// The gateway's own operational state. It is built with the bus because
	// entering the draining state is something connected clients are told about,
	// and everything that admits work consults it.
	life := lifecycle.New(bus, logger)

	// The bridge is the update sink, so it is built before the adapter that
	// publishes into it.
	updates := bridge.New(bus, cfg.Limits)

	approvalsBroker := approvals.New(approvals.Options{
		Timeout:  cfg.Session.ApprovalTimeout.Std(),
		GrantTTL: cfg.Session.ApprovalGrantTTL.Std(),
		Bus:      bus,
		Logger:   logger,
		Now:      time.Now,
	})
	defer approvalsBroker.Close()

	// The listening socket is claimed before anything else, and the order is
	// load-bearing rather than tidy.
	//
	// It was the other way round, and a second gateway — a deploy overlap, a
	// stray `run` from a shell — would connect to the agent host *first*, take
	// the control connection from the gateway that was serving the phone, and
	// only then discover that the port was taken and exit. The live gateway was
	// left displaced and reconnecting, twice per redeploy, for no reason at all.
	//
	// The port is the resource only one process can hold. Claiming it first
	// makes single-instance a property of the operating system rather than of
	// everyone remembering to be careful, and it fails before any shared state
	// has been touched.
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w\n"+
			"       Another gateway is probably already serving this deployment. "+
			"Check with:\n"+
			"         launchctl print gui/$(id -u)/dev.dsh-gateway.gateway | grep pid\n"+
			"         lsof -nP -iTCP:%s -sTCP:LISTEN",
			cfg.Listen, err, strings.TrimPrefix(cfg.Listen, "127.0.0.1:"))
	}
	defer func() { _ = listener.Close() }()
	logger.Info("listening socket claimed", "listen", cfg.Listen, "pid", os.Getpid())

	// The agent runs in the agent host, always. This gateway is a client of it,
	// and there is deliberately no way to run the child in this process — which
	// is the difference that makes a redeploy a reconnect rather than the end of
	// whatever turn was in flight.
	//
	// There was one, briefly: a `dsh.mode: in-process` switch that spawned the
	// ACP child here instead. It was removed because it was a switch back to the
	// behaviour this architecture exists to eliminate, and a switch like that is
	// not insurance. It is what
	// an operator reaches for at 2am when the host will not start, which is
	// precisely the moment it would quietly reintroduce the bug, and it cannot
	// even serve as a fallback: DSH's single-writer lock belongs to the host, so
	// a second writer could not take over a session anyway.
	//
	// What replaces it is a refusal that names the fix. The wait below is
	// bounded, and a gateway that cannot reach its host says so and stops,
	// rather than appearing to work.
	socketPath := cfg.SocketPath()
	instanceID := idgen.New("gw")
	harnessDriver := agenthost.NewClient(agenthost.ClientOptions{
		// A wait, not a refusal at the first attempt: a supervisor may start
		// this job before the host's, and a gateway that would not come up for
		// that reason is a deployment that only works when two jobs happen to
		// start in the right order.
		Dial: func(ctx context.Context) (net.Conn, error) {
			return hostlink.Dial(ctx, socketPath, cfg.DSH.StartTimeout.Std())
		},
		InstanceID:          instanceID,
		Logger:              logger,
		StateSink:           updates,
		Updates:             updates,
		Permissions:         approvalsBroker,
		ReconnectBackoff:    cfg.DSH.RestartBackoff.Std(),
		MaxReconnectBackoff: cfg.DSH.MaxRestartBackoff.Std(),
	})
	// The cleanup context is created here, from Background, rather than inside
	// the deferred call: by the time it runs, `ctx` is cancelled — that is what
	// shutting down means — so deriving from it would leave every cleanup step
	// with an expired deadline. Saying that once, at the point the context is
	// made, is clearer than a note on a function nobody will look at.
	closeHarness := func() { //nolint:contextcheck // the context is created inside, from Background, deliberately
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cfg.Limits.ShutdownTimeout.Std())
		defer cancel()
		if err := harnessDriver.Close(cleanupCtx); err != nil { //nolint:contextcheck // rooted at Background deliberately; see above
			logger.Warn("could not close the agent host connection", "error", err.Error())
		}
	}
	defer closeHarness()
	logger.Info("driving the agent through the agent host",
		"socket", socketPath, "instance", instanceID)

	// The scheduler is built before the lease because the lease asks it whether
	// a session is mid-turn. The dependency runs one way: the scheduler knows
	// nothing about leases, so there is a single call direction and a single
	// lock order to reason about.
	scheduler := turns.New(turns.Options{
		Harness:       harnessDriver,
		Bus:           bus,
		Logger:        logger,
		Timeout:       cfg.Session.PromptTimeout.Std(),
		QueueDepth:    cfg.Session.PromptQueueDepth,
		MaxConcurrent: cfg.Limits.MaxConcurrentTurns,
	})

	leases := lease.New(lease.Options{
		Harness:     harnessDriver,
		IdleTimeout: cfg.Session.IdleTimeout.Std(),
		Logger:      logger,
		Busy:        scheduler.Busy,
		OnAcquire: func(info harness.SessionInfo) {
			bus.Publish(events.TypeSessionState, info.ID, map[string]any{"leased": true})
		},
		OnRelease: func(sessionID, reason string) {
			// A grant names a session, so a session the gateway has let go takes
			// its authorisations with it. Leaving them would mean a rule agreed
			// to for work that is finished quietly applying to whatever runs in
			// that session next.
			approvalsBroker.RevokeSessionGrants(sessionID)
			bus.Publish(events.TypeSessionState, sessionID, map[string]any{
				"leased": false, "reason": reason,
			})
		},
	})

	// --- undo ---------------------------------------------------------------
	//
	// The only component in the gateway that writes to an operator's files, and
	// it is constructed only when the operator asked for it. A deployment that
	// leaves changes.revert off has no reverter at all, so the write path is
	// absent rather than disabled — a stronger property, and a simpler one to
	// check.
	var reverter *workspace.Reverter
	if cfg.Changes.Revert.Enabled {
		reverter = workspace.New(workspace.Options{Roots: cfg.Workspaces, Logger: logger})
		logger.Warn("changes.revert is enabled: this gateway may write to files " +
			"inside the configured workspaces when an operator asks it to undo a session's edits")
	}

	// --- history ------------------------------------------------------------

	var history *sessionlog.Store
	if cfg.Transcript.Enabled {
		history, err = sessionlog.New(cfg.SessionsDir(), logger)
		if err != nil {
			return err
		}
		defer history.Close()
	}

	// Unit prices for a session receipt's cost estimate. Configured, not fetched:
	// the numbers come from the account, and the account's credentials do not
	// belong in a daemon's environment.
	if history != nil && len(cfg.Receipt.Pricing) > 0 {
		prices := make([]sessionlog.Price, 0, len(cfg.Receipt.Pricing))
		for _, price := range cfg.Receipt.Pricing {
			prices = append(prices, sessionlog.Price{
				Model:     price.Model,
				Currency:  price.Currency,
				Input:     price.Input,
				Output:    price.Output,
				CacheRead: price.CacheRead,
			})
		}
		history.SetPrices(prices)
	}

	// --- trash ---------------------------------------------------------------

	// Deletions from the phone are moves into here, and this is the only place
	// that ever removes one for good.
	var trash *sessionlog.Trash
	if history != nil {
		trash, err = sessionlog.OpenTrash(history, filepath.Join(cfg.StateDir, "trash"))
		if err != nil {
			return err
		}
		if purged, err := trash.Purge(trashGrace, time.Now()); err != nil {
			logger.Warn("could not purge the trash", "error", err.Error())
		} else if purged > 0 {
			logger.Info("purged expired trash", "sessions", purged)
		}
	}

	// --- curation -----------------------------------------------------------

	// What the operator has put away, from both screens: DSH's own store (which
	// the desktop writes) and the gateway's overlay (which this phone writes).
	curationStore, err := curation.Open(curation.Options{
		StateDir: cfg.StateDir,
		DSHHome:  cfg.DSH.Home,
		Logger:   logger,
	})
	if err != nil {
		return err
	}

	// Triage's idea of "a test" is portable by default, and a deployment whose
	// own harness leaves different markers adds them here. A pattern that will
	// not compile is fatal at startup rather than ignored: a typo that silently
	// disabled the rule would be found only by noticing suggestions that no
	// longer appear.
	curationRules := curation.DefaultRules()
	if len(cfg.Curation.TempRoots) > 0 {
		curationRules.TempRoots = cfg.Curation.TempRoots
	}
	if curationRules, err = curationRules.WithPatterns(cfg.Curation.TestTitlePatterns); err != nil {
		return err
	}

	// --- notifications -------------------------------------------------------

	// Two things earn an interruption: an approval that expires, and a turn that
	// ran long enough that nobody is watching it. Both go through the push
	// service, whose subscriptions belong to paired devices.
	var (
		pushService  *push.Service
		pushNotifier *push.Notifier
		pushChannels []push.Webhook
	)
	for _, hook := range cfg.Push.Webhooks {
		channel := push.Webhook{Kind: hook.Kind, URL: hook.URL}
		if err := channel.Validate(); err != nil {
			return fmt.Errorf("push.webhooks: %w", err)
		}
		channel.Open(logger, nil, cfg.PublicURL)
		pushChannels = append(pushChannels, channel)
	}

	if cfg.Push.Enabled {
		pushService, err = push.Open(push.Options{
			StateDir: cfg.StateDir,
			Subject:  cfg.Push.Subject,
			Logger:   logger,
		})
		if err != nil {
			return err
		}
		notifier, err := push.NewNotifier(push.NotifierOptions{
			Bus:       bus,
			Service:   pushService,
			Webhooks:  pushChannels,
			Logger:    logger,
			Threshold: cfg.Push.TurnThreshold.Std(),
			// Off unless asked for: a notification body is read on a lock screen
			// and mirrored to any configured webhook, and this one would be text
			// the operator typed. `Preview` in particular is the opening line of
			// their first prompt, so it is the most sensitive thing that could
			// have been put here. What still names the session is the *title*,
			// which is what makes two notifications distinguishable at a glance.
			IncludeSessionName: cfg.Push.IncludeSessionName,
			Describe: func(ctx context.Context, sessionID string) string {
				if history == nil {
					return ""
				}
				meta, err := history.Meta(ctx, sessionID)
				if err != nil {
					return ""
				}
				if meta.Title != "" {
					return meta.Title
				}
				return meta.Preview
			},
		})
		if err != nil {
			return err
		}
		pushNotifier = notifier
	}

	// --- sessions other processes run ---------------------------------------

	// A session started at the desk belongs to a different DSH process, and the
	// gateway hears nothing about it over ACP. Following the log is what makes
	// it live on the phone instead of visible only after a refresh.
	var follower *sessionwatch.Watcher
	if history != nil && cfg.Transcript.Follow {
		follower, err = sessionwatch.New(sessionwatch.Options{
			Store:    history,
			Bus:      bus,
			Owned:    leases.IsLeased,
			Limits:   cfg.Limits,
			Interval: cfg.Transcript.FollowInterval.Std(),
			Logger:   logger,
		})
		if err != nil {
			return err
		}
	}

	// --- API ----------------------------------------------------------------

	deps := v1.Deps{
		Config:        cfg,
		Logger:        logger,
		Bus:           bus,
		Leases:        leases,
		Approvals:     approvalsBroker,
		Turns:         scheduler,
		Harness:       harnessDriver,
		Sessions:      history,
		Workspace:     reverter,
		Trash:         trash,
		Curation:      curationStore,
		CurationRules: curationRules,
		Push:          pushService,
		Webhooks:      pushChannels,
		Auth:          authenticator,
		Pairing:       pairingService,
		Audit:         auditLog,
		Limiter:       limiter,
		Resolver:      resolver,
		Version:       version,
		StartedAt:     time.Now(),
		Lifecycle:     life,
		// Readiness is the child's, and the child lives in the agent host: a
		// driver that cannot reach the host reports a state that is not ready,
		// so this one expression is the whole rule.
		Ready: func() bool { return harnessDriver.State() == harness.StateReady },
		// ACP reveals the model catalog only when a session attaches, so it is
		// remembered between runs: otherwise the picker in the app has nothing
		// to offer until someone opens a session after every restart.
		CatalogPath: filepath.Join(cfg.StateDir, "models.json"),
	}
	// Assigned rather than set inline: a nil *Watcher in an interface field is
	// not a nil interface, and the API's `!= nil` check would happily call a
	// method on it.
	if follower != nil {
		deps.Follower = follower
	}
	// The harness is the authority on what it still holds, and those are exactly
	// the sessions its own listing cannot show. A client always knows.
	deps.Held = harnessDriver

	api, err := v1.New(deps)
	if err != nil {
		return err
	}

	// --- routes -------------------------------------------------------------

	// One mux, and the standard library's precedence rules do the routing:
	// exact patterns win over prefixes, and the longest prefix wins.
	//
	// The routes must share a mux rather than being nested under an "/api/"
	// prefix, because the gateway's API and the desktop GUI's API would otherwise
	// collide: DSH's own client speaks to `/api/remote.mux` and `/api/<ns>/<method>`,
	// and an "/api/" mount for the gateway would swallow both. Registering the
	// gateway's specific `/api/v1/...` patterns directly leaves every other
	// `/api/*` path to fall through to the desktop proxy.
	mux := http.NewServeMux()

	app, err := web.New(logger)
	if err != nil {
		return fmt.Errorf("the embedded web app is missing; build the frontend first "+
			"(cd web && npm install && npm run build): %w", err)
	}
	app.Register(mux)
	api.Register(mux)

	if cfg.DesktopUI.Enabled {
		proxy, err := edge.New(cfg.DesktopUI.Upstream, logger)
		if err != nil {
			return err
		}
		proxy.Register(mux)
		logger.Warn("the desktop GUI is exposed at /; it can run arbitrary commands and " +
			"is protected only by the gateway's device authentication")
	} else {
		mux.Handle("/", edge.RootHandler(web.MountPath))
	}

	handler := httpcore.Chain(
		httpcore.RequestID(),
		httpcore.ClientAddress(resolver),
		httpcore.Recover(logger),
		httpcore.AccessLog(logger),
		httpcore.SecurityHeaders(cfg.SecureCookies()),
		httpcore.BodyLimit(cfg.Limits.MaxBodyBytes),
		httpcore.Timeout(cfg.Limits.ReadTimeout.Std()),
	)(mux)

	// --- run ----------------------------------------------------------------

	if err := harnessDriver.Start(ctx); err != nil {
		// This is the one way the gateway cannot start, and the message has to
		// be an instruction rather than a symptom: the operator reading it has a
		// phone that is not working and no other clue.
		return fmt.Errorf("could not reach the agent host at %s: %w\n"+
			"       The gateway does not run the agent itself; it drives the process that does.\n"+
			"       Start it, or reinstall both jobs:\n"+
			"         %s\n"+
			"         bash deploy/mac/install.sh --server <vps-ip> --public-url <url> --workspaces <paths>\n"+
			"       If it is already running, ask it what it thinks:\n"+
			"         dsh-agent-host status -config %s",
			socketPath, err,
			"dsh-agent-host serve -config "+configPath, configPath)
	}

	go leases.Run(ctx)
	go sweepLimiter(ctx, limiter)
	if trash != nil {
		go purgeTrashDaily(ctx, trash, logger)
	}
	if pushNotifier != nil {
		go pushNotifier.Run(ctx)
	}
	if follower != nil {
		go follower.Run(ctx)
	}

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
		// A slow or stalled header read is the cheapest denial of service, and
		// these endpoints are reachable from the internet through the tunnel.
		ReadHeaderTimeout: cfg.Limits.ReadHeaderTimeout.Std(),
		ReadTimeout:       cfg.Limits.ReadTimeout.Std(),
		// WriteTimeout stays zero: streamed responses and the event WebSocket
		// own their own deadlines, and a server-wide write deadline would sever
		// them mid-flight.
		WriteTimeout: 0,
		IdleTimeout:  cfg.Limits.IdleTimeout.Std(),
		BaseContext:  func(net.Listener) context.Context { return ctx },
	}

	if cfg.DSH.SandboxMode == config.SandboxFullAccess {
		// This is not a stylistic warning. DSH derives its approval policy from
		// the sandbox mode, so this value means every tool call runs without
		// asking. The phone's approval UI would still render; it would simply
		// never be shown anything.
		logger.Warn("dsh.sandboxMode is danger-full-access, which makes DeepSeek Harness " +
			"auto-approve every tool call: the phone will never be asked to authorise " +
			"anything. Use workspace-write if you want approvals to mean something.")
	}
	if cfg.Push.Enabled && cfg.Push.Subject == config.DefaultPushSubject {
		// Not fatal, because the claim is syntactically valid and some services
		// accept it. Worth saying out loud all the same: RFC 8292 wants a way to
		// reach whoever is sending, this one reaches nobody, and a service that
		// enforces it answers 403 with nothing to point at.
		logger.Warn("push.subject is still the shipped placeholder, which reaches nobody",
			"subject", config.DefaultPushSubjectShort,
			"hint", "set push.subject to a real mailto: or https: address you control")
	}

	// Settings that came from the environment rather than the file are worth
	// saying out loud: the file is what an operator edits, and a stale job or
	// shell quietly winning is otherwise invisible.
	for _, warning := range cfg.Warnings() {
		logger.Warn(warning)
	}

	logger.Info("gateway serving",
		"listen", cfg.Listen,
		"public_url", cfg.PublicURL,
		"state_dir", cfg.StateDir,
		"desktop_ui", cfg.DesktopUI.Enabled,
		"sandbox_mode", cfg.DSH.SandboxMode,
		"workspaces", len(cfg.Workspaces),
		"version", version,
	)

	// Pairing is announced on every start because a code is time-derived and an
	// operator who missed the previous one has no other way to see the current.
	publishPairingHint(cfg, pairingService, logger)

	errCh := make(chan error, 1)
	go func() {
		// Serve, not ListenAndServe: the socket was claimed above, before the
		// agent host was contacted, and this process is already the only one
		// that could have it.
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	// Draining comes first, and it is not a formality: it is what stops a new
	// prompt from being admitted by a process that is about to close the agent
	// underneath it, and what tells connected clients to move to the successor
	// before the listener goes away rather than after a failed request.
	//
	// A turn already running is given the deploy window to finish. This is the
	// one place the gateway knowingly trades a longer shutdown for work that
	// would otherwise be destroyed, which is why the window is its own setting
	// rather than a reuse of shutdownTimeout.
	life.Drain(lifecycle.ReasonDeploy)
	if window := cfg.Session.DeployDrainTimeout.Std(); window > 0 {
		logger.Info("waiting for in-flight turns", "window", window.String())
		started := time.Now()
		idle := lifecycle.WaitFor(ctx, scheduler.BusyAny, window, 250*time.Millisecond)
		logger.Info("drain finished",
			"idle", idle,
			"waited", time.Since(started).Round(time.Millisecond).String())
	}

	// Shutdown order matters. Stop accepting new work, then refuse pending
	// approvals, then hand sessions back to the desktop, then stop the harness.
	// Reversing any of these would leave a tool waiting on a gateway that is
	// already gone, or a session locked by a process that no longer exists.
	// Deliberately rooted at Background rather than at ctx: ctx is already
	// cancelled — that is why we are shutting down — so deriving from it would
	// give every cleanup step an already-expired deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Limits.ShutdownTimeout.Std())
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil { //nolint:contextcheck // rooted at Background deliberately; see above
		logger.Warn("http shutdown did not complete cleanly", "error", err.Error())
	}
	approvalsBroker.Close()
	leases.ReleaseAll(shutdownCtx, "shutdown") //nolint:contextcheck // see above
	// The harness connection is closed by the deferred closeHarness above.
	// Keeping the child — and the turn it is running — alive across a redeploy
	// is the agent host's business, not this process's.
	return nil
}

// sweepLimiter periodically drops rate-limit state for addresses that have gone
// quiet, so that a long-running gateway does not accumulate one entry per source
// address it has ever seen.
func sweepLimiter(ctx context.Context, limiter *ratelimit.Limiter) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			limiter.Sweep()
		}
	}
}

// trashGrace is how long a deleted session stays recoverable.
//
// Long enough that a mistake is noticed (a month is what a phone's own photo
// bin offers), and short enough that the trash is not a second sessions
// directory.
const trashGrace = 30 * 24 * time.Hour

// purgeTrashDaily removes expired deletions once a day, and once at startup.
func purgeTrashDaily(ctx context.Context, trash *sessionlog.Trash, logger *logx.Logger) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purged, err := trash.Purge(trashGrace, time.Now())
			if err != nil {
				logger.Warn("could not purge the trash", "error", err.Error())
				continue
			}
			if purged > 0 {
				logger.Info("purged expired trash", "sessions", purged)
			}
		}
	}
}
