// Command dsh-gateway exposes a desktop DeepSeek Harness installation to a phone
// browser over the public internet.
//
// The binary has two jobs:
//
//	dsh-gateway run    serve the gateway (the default)
//	dsh-gateway pair   print a pairing link and QR code for a new phone
//
// `run` is the long-lived service. `pair` is a short-lived helper that derives
// the current pairing code from the same secret file the running gateway uses, so
// no administrative HTTP endpoint has to exist for it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

// version is stamped at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	os.Exit(realMain(os.Args[1:]))
}

// realMain exists so that every path returns through a deferred cleanup and the
// process exit code is set in exactly one place.
func realMain(args []string) int {
	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}

	var err error
	switch command {
	case "run":
		err = runGateway(args)
	case "pair":
		err = runPair(args)
	case "doctor":
		err = runDoctor(args)
	case "version":
		fmt.Printf("dsh-gateway %s\n", version)
		return 0
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "dsh-gateway: unknown command %q\n\n", command)
		usage(os.Stderr)
		return 2
	}

	if err != nil {
		// flag.ErrHelp means the operator asked for help, not that anything broke.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "dsh-gateway: %v\n", err)
		return 1
	}
	return 0
}

func usage(w *os.File) {
	// A failure to write usage text is not actionable: the only caller passes
	// stdout or stderr, and there is nowhere left to report the problem.
	_, _ = fmt.Fprint(w, `dsh-gateway — drive DeepSeek Harness on your desktop from your phone.

Usage:
  dsh-gateway run [flags]     Serve the gateway. This is the default.
  dsh-gateway pair [flags]    Print a pairing link and QR code for a new device.
  dsh-gateway doctor [flags]  Check the whole path and report what is broken.
  dsh-gateway version         Print the build version.

Run "dsh-gateway <command> -h" for the flags of each command. If something is
not working, start with:

  dsh-gateway doctor

The gateway binds a loopback address only. Put a TLS terminator in front of it —
see docs/deployment.md — because the agent behind it can run arbitrary commands.
`)
}
