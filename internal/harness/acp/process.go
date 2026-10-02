package acp

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// processConfig describes how to launch the ACP child.
type processConfig struct {
	// Binary is the dsh executable.
	Binary string
	// Args are the arguments after the binary, for example
	// ["--profile", "acp"].
	Args []string
	// Env is the complete child environment, as KEY=VALUE.
	Env []string
	// Dir is the child's working directory. The ACP client supplies an absolute
	// cwd per session, so this only affects the launcher itself.
	Dir string
}

// process is one ACP child process.
//
// DSH reserves stdout for protocol frames and writes diagnostics to stderr, so
// the two streams are wired separately and stderr is forwarded to the gateway log
// rather than being mixed into the protocol.
type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	waited  chan struct{}
	waitErr error
	waitMu  sync.Mutex
}

// startProcess launches the child and starts draining its stderr.
func startProcess(cfg processConfig, logger *logx.Logger) (*process, error) {
	// Launching a configured binary is this adapter's entire purpose, and the
	// binary and arguments come from the gateway's own configuration, which only
	// the operator can write.
	//
	// A context is deliberately not attached: the child's lifetime is the
	// gateway's, not any request's, and it is managed explicitly — Stop closes
	// stdin, which DeepSeek Harness treats as the signal for an orderly
	// shutdown, and escalates to a kill only if that is ignored. A context would
	// be a second, redundant cancellation path that could kill the agent
	// mid-turn for reasons unrelated to the agent.
	cmd := exec.Command(cfg.Binary, cfg.Args...) //nolint:gosec,noctx // lifetime managed explicitly by Stop
	cmd.Env = cfg.Env
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: open child stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: open child stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: open child stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: start %s: %w", cfg.Binary, err)
	}

	p := &process{cmd: cmd, stdin: stdin, stdout: stdout, waited: make(chan struct{})}

	// Draining stderr is not optional: a child that fills its stderr pipe with
	// warnings would block forever on write and appear to hang.
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 16*1024), 1<<20)
		for scanner.Scan() {
			logger.Info("harness", "stream", "stderr", "line", scanner.Text())
		}
	}()

	go func() {
		err := cmd.Wait()
		p.waitMu.Lock()
		p.waitErr = err
		p.waitMu.Unlock()
		close(p.waited)
	}()

	return p, nil
}

// Wait returns after the child exits and reports its wait error.
func (p *process) Wait() error {
	<-p.waited
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return p.waitErr
}

// Closed reports whether the child has already exited.
func (p *process) Closed() bool {
	select {
	case <-p.waited:
		return true
	default:
		return false
	}
}

// Stop closes stdin, which DSH treats as the supported shutdown signal, then
// waits up to grace for a clean exit before killing the process.
//
// Closing stdin rather than sending a signal matters: DSH binds stdin EOF to a
// bounded, orderly shutdown that drains agents and flushes persistence. A SIGTERM
// would skip that.
func (p *process) Stop(grace time.Duration) error {
	_ = p.stdin.Close()

	select {
	case <-p.waited:
		return p.Wait()
	case <-time.After(grace):
	}

	// The child ignored EOF for longer than the grace period. Escalate.
	if err := p.cmd.Process.Kill(); err != nil {
		return fmt.Errorf("acp: kill child: %w", err)
	}
	select {
	case <-p.waited:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("acp: child did not exit after kill")
	}
	return p.Wait()
}

// Kill terminates the child immediately, for use when the gateway itself is
// shutting down under a deadline.
func (p *process) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}
