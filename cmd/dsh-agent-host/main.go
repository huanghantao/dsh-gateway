// Command dsh-agent-host holds the DeepSeek Harness child on behalf of a
// gateway that may be redeployed underneath it.
//
// It is a peer of the gateway, not its child, and that is load-bearing: launchd
// kills every process in a job's process group when the job dies, so a host
// spawned by the gateway would be killed by exactly the event it exists to
// survive — and the two-tier design would look like it worked while doing
// nothing. The installer therefore gives it its own label.
//
//	dsh-agent-host serve   hold the child and serve the control socket
//	dsh-agent-host status  ask a running host what it is holding
//
// `serve` is the long-lived service. `status` is a one-shot helper for an
// operator, and for a deploy script deciding whether a restart is safe.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/agenthost"
	"github.com/huanghantao/dsh-gateway/internal/agenthost/acp"
	"github.com/huanghantao/dsh-gateway/internal/agenthost/hostlink"
	"github.com/huanghantao/dsh-gateway/internal/agenthost/hostwire"
	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// version is stamped at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() { os.Exit(realMain(os.Args[1:])) }

func realMain(args []string) int {
	command := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}

	var err error
	switch command {
	case "serve":
		err = runServe(args)
	case "status":
		err = runStatus(args)
	case "version":
		fmt.Printf("dsh-agent-host %s\n", version)
		return 0
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "dsh-agent-host: unknown command %q\n\n", command)
		usage(os.Stderr)
		return 2
	}

	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "dsh-agent-host: %v\n", err)
		return 1
	}
	return 0
}

func usage(w *os.File) {
	// The write is to stdout or stderr at the end of a usage path; a failure
	// there has nowhere to be reported and does not change the exit code.
	_, _ = fmt.Fprint(w, `dsh-agent-host — hold the DeepSeek Harness child across gateway redeploys

Usage:
  dsh-agent-host serve [flags]   hold the child and serve the control socket
  dsh-agent-host status [flags]  report what a running host is holding
  dsh-agent-host version         print the version

It reads the same config.yaml as the gateway, so the child it launches and the
one the gateway expects to be talking to cannot drift apart. The socket it
serves is <stateDir>/`+config.AgentHostSocket+`, and only the owning user can
reach it.
`)
}

// flags are shared between the two commands, because they both need to find the
// same config and the same socket.
type flags struct {
	configPath string
	stateDir   string
	socket     string
	logLevel   string
	logFormat  string
	dshBinary  string
	dshHome    string

	drainTimeout time.Duration
}

func (f *flags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.configPath, "config", "", "path to config.yaml (default: <stateDir>/config.yaml)")
	fs.StringVar(&f.stateDir, "state-dir", "", "state directory (default ~/.dsh-gateway)")
	fs.StringVar(&f.socket, "socket", "", "control socket (default: <stateDir>/"+config.AgentHostSocket+")")
	fs.StringVar(&f.logLevel, "log-level", "", "debug, info, warn, or error")
	fs.StringVar(&f.logFormat, "log-format", "", "text or json")
	fs.StringVar(&f.dshBinary, "dsh", "", "path to the dsh executable (default: from config)")
	fs.StringVar(&f.dshHome, "dsh-home", "", "DSH_HOME (default: from config)")
}

// resolve reads the configuration the same way the gateway does, so the two
// processes agree about the child, the state directory and the socket.
func (f *flags) resolve() (config.Config, string, error) {
	provisional := config.Default()
	if f.stateDir != "" {
		provisional.StateDir = f.stateDir
	}
	if err := provisional.Resolve(); err != nil {
		return config.Config{}, "", err
	}

	path := f.configPath
	if path == "" {
		path = provisional.StateDir + "/config.yaml"
	}
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, path, err
	}
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return config.Config{}, path, err
	}
	if f.stateDir != "" {
		cfg.StateDir = f.stateDir
	}
	if f.logLevel != "" {
		cfg.Log.Level = f.logLevel
	}
	if f.logFormat != "" {
		cfg.Log.Format = f.logFormat
	}
	if f.dshBinary != "" {
		cfg.DSH.Binary = f.dshBinary
	}
	if f.dshHome != "" {
		cfg.DSH.Home = f.dshHome
	}
	if err := cfg.Resolve(); err != nil {
		return config.Config{}, path, err
	}
	return cfg, path, nil
}

func (f *flags) socketPath(cfg config.Config) string {
	if f.socket != "" {
		return f.socket
	}
	return cfg.SocketPath()
}

func runServe(args []string) error {
	var f flags
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	f.bind(fs)
	fs.DurationVar(&f.drainTimeout, "drain-timeout", 2*time.Minute,
		"how long a shutdown waits for turns in flight before ending them")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, configPath, err := f.resolve()
	if err != nil {
		return err
	}
	logger := logx.New(os.Stderr, logx.Config{Level: cfg.Log.Level, Format: logx.Format(cfg.Log.Format)})
	logger.Debug("configuration loaded", "path", configPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The child is built by the host, because it reports *to* the host. That
	// inversion is the only reason this is a factory rather than a value.
	host, err := agenthost.New(agenthost.Options{
		Logger: logger,
		NewHarness: func(updates harness.UpdateSink, permissions harness.PermissionHandler, states harness.StateSink) (harness.Harness, error) {
			return acp.New(acp.Options{
				Binary:            cfg.DSH.Binary,
				Profile:           cfg.DSH.Profile,
				Home:              cfg.DSH.Home,
				SandboxMode:       cfg.DSH.SandboxMode,
				ExtraEnv:          cfg.DSH.ExtraEnv,
				StartTimeout:      cfg.DSH.StartTimeout.Std(),
				StopTimeout:       cfg.DSH.StopTimeout.Std(),
				RestartBackoff:    cfg.DSH.RestartBackoff.Std(),
				MaxRestartBackoff: cfg.DSH.MaxRestartBackoff.Std(),
				ApprovalTimeout:   cfg.Session.ApprovalTimeout.Std(),
				Logger:            logger,
				Updates:           updates,
				Permissions:       permissions,
				States:            states,
			})
		},
		TurnTimeout:       cfg.Session.PromptTimeout.Std(),
		PermissionTimeout: cfg.Session.ApprovalTimeout.Std(),
		DrainTimeout:      f.drainTimeout,
	})
	if err != nil {
		return err
	}

	socketPath := f.socketPath(cfg)
	listener, err := hostlink.Listen(ctx, socketPath)
	if err != nil {
		return err
	}
	defer hostlink.Remove(socketPath)
	defer func() { _ = listener.Close() }()

	if err := host.Start(ctx); err != nil {
		return fmt.Errorf("start the harness child: %w", err)
	}
	logger.Info("agent host listening",
		"socket", socketPath,
		"state_dir", cfg.StateDir,
		"dsh", cfg.DSH.Binary,
		"profile", cfg.DSH.Profile,
		"sandbox_mode", cfg.DSH.SandboxMode,
		"version", version)

	go acceptLoop(ctx, listener, host, logger)

	<-ctx.Done()
	logger.Info("shutting down: the child and its turns end with this process")

	// A host restart is the one restart that can end a turn, so it asks first
	// and waits. The bounded wait is the operator's escape from a wedged turn.
	remaining := host.DrainAndWait(ctx, "shutdown")
	if remaining > 0 {
		logger.Warn("ending turns that did not finish inside the drain window", "turns", remaining)
		host.CancelRunning("shutdown")
	}
	_ = listener.Close()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.DSH.StopTimeout.Std()+5*time.Second)
	defer cancel()
	if err := host.Close(shutdownCtx); err != nil {
		logger.Warn("the child did not stop cleanly", "error", err.Error())
	}
	return nil
}

// acceptLoop serves connections until the context ends.
//
// Every connection is served, not just one, because the host cannot know whether
// a connecting process is a new gateway or an operator's `status` call. What is
// exclusive is not the socket but the *control* role: a connection becomes the
// active one by saying hello, and `Adopt` takes that role from whoever held it.
// A status call never says hello, so it can never displace the gateway.
func acceptLoop(ctx context.Context, listener net.Listener, host *agenthost.Server, logger *logx.Logger) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			logger.Warn("accept failed", "error", err.Error())
			continue
		}
		go host.Serve(ctx, hostwire.NewConn(conn))
	}
}

func runStatus(args []string) error {
	var f flags
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	f.bind(fs)
	timeout := fs.Duration("timeout", 5*time.Second, "how long to wait for an answer")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, _, err := f.resolve()
	if err != nil {
		return err
	}
	socketPath := f.socketPath(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	conn, err := hostlink.Dial(ctx, socketPath, 0)
	if err != nil {
		// A host that is not running is a state, not a failure of this command:
		// a deploy script asks precisely to find that out.
		fmt.Printf("no agent host at %s\n", socketPath)
		return nil
	}
	defer func() { _ = conn.Close() }()

	wire := hostwire.NewConn(conn)
	wire.SetErrorClassifier(hostwire.ErrorOf)
	wire.Start()

	// Deliberately not ClaimsControl: this command reports, and taking the role
	// from the gateway in order to do so would disconnect it. That is not a
	// hypothetical — it is what the first version of this command did.
	var hello hostwire.HelloResult
	if err := wire.Call(ctx, hostwire.MethodHello, hostwire.Hello{
		Protocol: hostwire.Protocol, Caller: "status",
	}, &hello); err != nil {
		// A host that is listening but not answering is worth distinguishing
		// from one that is absent: the first is a wedged process to look at,
		// the second is a job that is not loaded.
		return fmt.Errorf("the agent host at %s did not answer: %w", socketPath, err)
	}

	var status hostwire.StatusResult
	if err := wire.Call(ctx, hostwire.MethodStatus, nil, &status); err != nil {
		return err
	}

	state := string(status.State)
	if status.Draining {
		state += " (draining)"
	}
	fmt.Printf("agent host  %s\n", socketPath)
	fmt.Printf("  epoch     %s\n", status.HostEpoch)
	fmt.Printf("  pid       %d\n", status.HostPID)
	fmt.Printf("  up since  %s\n", status.StartedAt.Local().Format(time.RFC3339))
	fmt.Printf("  harness   %s", state)
	if status.Detail != "" {
		fmt.Printf(" (%s)", status.Detail)
	}
	fmt.Println()
	fmt.Printf("  sessions  %d held\n", status.SessionsHeld)
	fmt.Printf("  turns     %d running\n", status.TurnsRunning)
	return nil
}
