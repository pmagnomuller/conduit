package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"time"
)

// openURL hands the control page to the platform opener. A package var so tests
// can assert the target without launching a browser.
var openURL = defaultOpenURL

func defaultOpenURL(url string) error {
	name, argv, err := openCommand(runtime.GOOS, url)
	if err != nil {
		return err
	}
	// Discard the opener's output: `open` is silent, xdg-open is chatty.
	// Bounded: xdg-open can block forever on a headless host, and a CLI must
	// not hang the terminal on a browser that will never appear.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}

func runUI(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("ui", stdout, stderr)
	if rc, ok := parseFlags(fs, args); !ok {
		return rc
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "conduitctl: ui takes no arguments\n\n%s", usageText)
		return exitUsage
	}
	c := newClient()
	// Probe first: opening a browser onto a dead port shows a browser error
	// page instead of the reason, which is a worse message than this one.
	var health struct {
		OK bool `json:"ok"`
	}
	if err := c.get("/_gateway/health", &health); err != nil {
		fmt.Fprintf(stderr, "conduitctl: %v\n", err)
		return exitError
	}
	url := c.base + "/_gateway/ui"
	if err := openURL(url); err != nil {
		fmt.Fprintf(stderr, "conduitctl: %v (open %s manually)\n", err, url)
		return exitError
	}
	fmt.Fprintf(stdout, "opened %s\n", url)
	return exitOK
}

// openCommand picks the platform opener.
func openCommand(goos, url string) (string, []string, error) {
	switch goos {
	case "darwin":
		return "open", []string{url}, nil
	case "linux":
		return "xdg-open", []string{url}, nil
	default:
		return "", nil, fmt.Errorf("no browser opener for %s; open %s manually", goos, url)
	}
}
