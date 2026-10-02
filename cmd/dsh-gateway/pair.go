package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/pairing"
	"github.com/huanghantao/dsh-gateway/web"
)

// runPair prints the current pairing link and a scannable QR code.
//
// It is a separate process from the running gateway, which is possible because
// the pairing code is derived from a secret file rather than held in memory. That
// design choice is what lets this command exist at all: the alternative would be
// an administrative HTTP endpoint, and an endpoint that mints device credentials
// is a much bigger thing to expose than a file read.
func runPair(args []string) error {
	var (
		configPath string
		stateDir   string
		publicURL  string
		dshHome    string
		asJSON     bool
		workspaces stringList
	)

	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.StringVar(&configPath, "config", "", "path to config.yaml (default: <stateDir>/config.yaml)")
	fs.StringVar(&stateDir, "state-dir", "", "where the pairing secret lives (default ~/.dsh-gateway)")
	fs.StringVar(&publicURL, "public-url", "", "externally reachable base URL (overrides the config file)")
	fs.StringVar(&dshHome, "dsh-home", "", "DSH_HOME (only used to locate the default config)")
	fs.BoolVar(&asJSON, "json", false, "print machine-readable output instead of a QR code")
	fs.Var(&workspaces, "workspace", "an allowed working directory; repeatable, and only needed\nif the config file does not already list one")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: dsh-gateway pair [flags]\n\n")
		fmt.Fprint(os.Stderr, "Prints a one-time pairing code and a QR code that opens the mobile app\n"+
			"with the code prefilled. The code rotates every pairing TTL (default 10 minutes)\n"+
			"and is derived from the pairing secret, so it stays valid as long as that window.\n\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Pairing shares the running gateway's configuration so that the derived code
	// matches. Workspaces are validated too, even though pairing does not use
	// them: a config that the gateway would refuse to start with should fail here
	// as well, rather than letting an operator pair a phone against a deployment
	// that cannot serve it.
	cfg, _, err := resolveConfig(runFlags{
		configPath: configPath,
		stateDir:   stateDir,
		publicURL:  publicURL,
		dshHome:    dshHome,
		workspaces: workspaces,
	})
	if err != nil {
		return err
	}

	// The state directory is created rather than assumed. `run` creates it, but
	// `pair` is the command an operator reaches for first and the one a fresh
	// clone reaches for through `make pair` — where nothing has created it yet,
	// and the failure was a bare "no such file or directory" from a temp file
	// that never got made. 0700 for the same reason `run` uses it: the pairing
	// secret lands here.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("pairing: create %s: %w", cfg.StateDir, err)
	}

	// This process only derives a code, so it needs no enroller and no harness.
	service, err := pairing.Open(cfg.StateDir, cfg.Auth.PairingTTL.Std(), nil, nil, time.Now)
	if err != nil {
		return err
	}

	code, expiresAt := service.Current()
	url := pairingURL(cfg.PublicURL, code)

	if asJSON {
		fmt.Printf("{\"code\":%q,\"url\":%q,\"expiresAt\":%q}\n",
			code, url, expiresAt.UTC().Format(time.RFC3339))
		return nil
	}

	fmt.Printf("\n  Pairing code:  %s\n", code)
	fmt.Printf("  Expires:       %s (%s from now)\n",
		expiresAt.Local().Format("15:04:05"), time.Until(expiresAt).Round(time.Second))

	if cfg.PublicURL == "" {
		fmt.Println("\n  publicURL is not configured, so there is no link to scan.")
		fmt.Println("  Set it in the config file or pass -public-url, then run this again.")
		fmt.Println("  You can still enter the code above by hand in the app.")
		return nil
	}

	fmt.Printf("  Link:          %s\n\n", url)

	if err := printQR(os.Stdout, url); err != nil {
		// A terminal that cannot render the QR is not a reason to fail: the code
		// above is still perfectly usable.
		fmt.Fprintf(os.Stderr, "  (could not render a QR code: %v)\n", err)
	}
	fmt.Println("  Scan the QR with your phone, or open the link. The code rotates;")
	fmt.Println("  run `dsh-gateway pair` again if it has expired.")
	fmt.Println()
	return nil
}

// pairingURL builds the link the QR encodes.
//
// The fragment route matters: the app is a single page served under /m/, and its
// router reads location.hash, so a path-based URL would 404 and a query string
// would never reach the client.
func pairingURL(publicURL, code string) string {
	base := strings.TrimRight(publicURL, "/")
	return fmt.Sprintf("%s%s#/pair?code=%s", base, web.MountPath, code)
}

// publishPairingHint prints the pairing instructions at startup.
//
// It runs at every start, not just the first, because the code is time-derived: an
// operator joining a session hours later needs the code that is valid *now*, and
// there is no other place it is displayed.
func publishPairingHint(cfg config.Config, service *pairing.Service, logger *logx.Logger) {
	if cfg.PublicURL == "" {
		logger.Warn("publicURL is not set, so no pairing link can be built; " +
			"set it and restart, or run `dsh-gateway pair` with -public-url")
		return
	}
	code, expiresAt := service.Current()
	url := pairingURL(cfg.PublicURL, code)

	// Neither the code nor the URL that embeds it appears here, and that is the
	// point: the gateway's stderr is a log file that outlives the code, and the
	// code is a credential. Anyone who can read that file inside the code's
	// window can enrol a fully authorised device. Saying where to get one is
	// enough — `dsh-gateway pair` prints the current one on demand.
	logger.Info("pairing is available",
		"expires_at", expiresAt.UTC().Format(time.RFC3339),
		"hint", "run `dsh-gateway pair` to print the current code and a QR code",
	)

	// The QR goes to stderr so that it cannot corrupt anything a caller may be
	// parsing from stdout. It carries the code, which is the point of it: it is
	// for the operator watching this terminal now, not for the log.
	if err := printQR(os.Stderr, url); err != nil {
		logger.Debug("could not render the pairing QR code", "error", err.Error())
	}
}

// printQR renders url as a QR code using half-block characters, which packs two
// module rows into one text row and keeps the code square in a terminal.
func printQR(w *os.File, url string) error {
	qr, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return err
	}
	// The library's bitmap includes no quiet zone; add one, because scanners rely
	// on it and a QR flush against surrounding text is genuinely unreliable.
	qr.DisableBorder = false
	bitmap := qr.Bitmap()

	// Half-block characters are used rather than ANSI background colours so the
	// code renders correctly when piped, copied out of a log, or viewed in a
	// terminal with no colour support.
	var sb strings.Builder
	// Two module rows per text row via the upper-half-block character.
	for y := 0; y+1 < len(bitmap); y += 2 {
		for x := 0; x < len(bitmap[y]); x++ {
			top := bitmap[y][x]
			bottom := bitmap[y+1][x]
			switch {
			case top && bottom:
				sb.WriteString("█")
			case top:
				sb.WriteString("▀")
			case bottom:
				sb.WriteString("▄")
			default:
				sb.WriteString(" ")
			}
		}
		sb.WriteString("\n")
	}
	if len(bitmap)%2 == 1 {
		// An odd module count leaves one row over; render it as upper halves.
		last := bitmap[len(bitmap)-1]
		for x := 0; x < len(last); x++ {
			if last[x] {
				sb.WriteString("▀")
			} else {
				sb.WriteString(" ")
			}
		}
		sb.WriteString("\n")
	}

	_, err = fmt.Fprint(w, sb.String())
	return err
}
