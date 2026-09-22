package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// providers is the set the gateway accepts on {"provider":...}. It is checked
// here too so a typo costs no round trip and the error names the valid values.
var providers = []string{"anthropic", "glm", "deepseek"}

func validProvider(p string) bool {
	for _, ok := range providers {
		if p == ok {
			return true
		}
	}
	return false
}

func runRoute(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("route", stdout, stderr)
	if rc, ok := parseFlags(fs, args); !ok {
		return rc
	}
	rest := fs.Args()
	switch {
	case len(rest) == 0:
		return routeShow(stdout, stderr)
	case len(rest) == 1 && rest[0] == "auto":
		return routePost(stdout, stderr, map[string]any{"clear": true}, "")
	case len(rest) == 1 && rest[0] == "jev":
		return routePost(stdout, stderr, map[string]any{"mode": "jev"}, jevHint)
	case len(rest) >= 1 && rest[0] == "pin":
		return routePin(rest[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "conduitctl: unknown route argument %q\n\n%s", strings.Join(rest, " "), usageText)
		return exitUsage
	}
}

const jevHint = "TYPESAFE_API_KEY goes in ~/.config/conduit/.env, then re-run ./setup.sh"

// routePin validates the provider before spending a request: the gateway would
// also reject it, but its 400 would arrive after a round trip and only lists
// the valid values in prose.
func routePin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || len(args) > 2 {
		fmt.Fprintf(stderr, "conduitctl: route pin takes a provider and an optional model\n\n%s", usageText)
		return exitUsage
	}
	provider := args[0]
	if !validProvider(provider) {
		fmt.Fprintf(stderr, "conduitctl: provider must be %s (got %q)\n",
			strings.Join(providers, "|"), provider)
		return exitUsage
	}
	body := map[string]any{"provider": provider}
	if len(args) == 2 {
		body["model"] = args[1]
	}
	return routePost(stdout, stderr, body, "")
}

func routePost(stdout, stderr io.Writer, body map[string]any, hint string) int {
	c := newClient()
	state, err := c.post("/_gateway/route", body)
	if err != nil {
		return reportPostError(err, hint, stderr)
	}
	printRouteState(stdout, state)
	return exitOK
}

func reportPostError(err error, hint string, stderr io.Writer) int {
	var api *apiError
	if errors.As(err, &api) {
		fmt.Fprintf(stderr, "conduitctl: %s\n", api.Msg)
		// Only the jev-disabled 400 earns the key hint: another 400 on this
		// shape (a future validation error, say) must not send the user off to
		// fix a key that is not the problem.
		if api.Status == 400 && hint != "" && strings.Contains(api.Msg, "TYPESAFE_API_KEY") {
			fmt.Fprintf(stderr, "conduitctl: %s\n", hint)
		}
		return exitError
	}
	fmt.Fprintf(stderr, "conduitctl: %v\n", err)
	return exitError
}

// routeShow reads both endpoints: route holds mode and the pin, status holds
// the routing decision and who actually served the last request.
func routeShow(stdout, stderr io.Writer) int {
	c := newClient()
	var info routeInfo
	if err := c.get("/_gateway/route", &info); err != nil {
		fmt.Fprintf(stderr, "conduitctl: %v\n", err)
		return exitError
	}
	var st statusInfo
	if err := c.get("/_gateway/status", &st); err != nil {
		fmt.Fprintf(stderr, "conduitctl: %v\n", err)
		return exitError
	}
	forced := "none"
	if info.ForcedProvider != "" {
		forced = info.ForcedProvider
		if info.ForcedModel != "" {
			forced += "/" + info.ForcedModel
		}
	}
	jev := "off"
	if info.Jev.Enabled {
		jev = "on"
	}
	fmt.Fprintf(stdout, "mode: %s  forced: %s  jev: %s\n", info.Mode, forced, jev)
	fmt.Fprintf(stdout, "routing: %s\n", st.Routing)
	fmt.Fprintf(stdout, "last request: %s\n", lastRequest(st.LastRequest))
	return exitOK
}

func lastRequest(lr map[string]string) string {
	if len(lr) == 0 || lr["provider"] == "" {
		return "none yet"
	}
	who := lr["provider"]
	if m := lr["upstream_model"]; m != "" {
		who += " " + m
	}
	if at := lr["at"]; at != "" {
		who += " at " + at
	}
	return who
}

func printRouteState(w io.Writer, s routeState) {
	forced := "none"
	if s.Provider != "" {
		forced = s.Provider
		if s.Model != "" {
			forced += "/" + s.Model
		}
	}
	fmt.Fprintf(w, "mode: %s  forced: %s\n", s.Mode, forced)
}
