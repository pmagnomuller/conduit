// Command conduitctl is a thin control client for a locally running conduit
// gateway. It is built from ./cmd/conduit and installed as ~/.local/bin/conduitctl
// — not as `conduit`, which is the gateway binary that scripts/conduit-run and
// the launchd plist exec.
//
// It holds no credentials and reads no config file: every subcommand is a
// loopback HTTP call to /_gateway/route or /_gateway/status, or a read of the
// local decisions log.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const usageText = `conduitctl — control the local conduit gateway

usage:
  conduitctl route                     show mode, pin and the last request
  conduitctl route auto                back to automatic routing
  conduitctl route jev                 let Jev pick provider+model per call
  conduitctl route pin <provider> [model]
  conduitctl status [-json]            human summary, or the raw payload
  conduitctl decisions [-n N]          tail the Jev decision log (default 20)
  conduitctl ui                        open the control page in a browser

The gateway URL defaults to http://127.0.0.1:8787; override it with
CONDUIT_GATEWAY_URL. Providers: anthropic, glm, deepseek.

-config is accepted and ignored: this client needs no config file.
`

// Exit codes follow the usual convention: success, runtime failure, usage.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the whole program minus os.Exit, so tests can drive every subcommand
// against an httptest server.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "route":
		return runRoute(rest, stdout, stderr)
	case "status":
		return runStatus(rest, stdout, stderr)
	case "decisions":
		return runDecisions(rest, stdout, stderr)
	case "ui":
		return runUI(rest, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	default:
		fmt.Fprintf(stderr, "conduitctl: unknown command %q\n\n%s", cmd, usageText)
		return exitUsage
	}
}

// newFlagSet builds the FlagSet shared by every subcommand: usage text on
// stdout (so --help is a success, not an error dump), the FlagSet's own error
// line on stderr, and -config accepted and ignored so muscle memory from the
// gateway binary still works.
func newFlagSet(name string, stdout, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stdout, usageText) }
	fs.String("config", "", "ignored; conduitctl reads no config file")
	return fs
}

// parseFlags parses args. The bool is false when the caller should return rc
// immediately: on --help (returned as success, usage already printed) and on a
// parse error (the FlagSet has already named the bad flag on stderr, so this
// does not repeat it).
func parseFlags(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitUsage, false
	}
	return exitOK, true
}
