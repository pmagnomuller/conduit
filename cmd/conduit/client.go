package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultGatewayURL = "http://127.0.0.1:8787"

// client is the loopback HTTP client. The timeout is generous enough for a
// status call on a busy gateway but short enough that a hung gateway fails
// instead of hanging the terminal.
type client struct {
	base string
	http *http.Client
}

func newClient() *client {
	base := os.Getenv("CONDUIT_GATEWAY_URL")
	if base == "" {
		base = defaultGatewayURL
	}
	return &client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// unreachable is the message shown when nothing answers on the port. The raw
// dial error ("dial tcp 127.0.0.1:8787: connect: connection refused") buries
// the only part a user can act on.
func (c *client) unreachable() error {
	return fmt.Errorf("gateway not reachable at %s — is it running? (./status.sh)", c.base)
}

// transportError maps a failed round trip onto the unreachable message when it
// is a dial failure (refused, no route, DNS, connect timeout), and otherwise
// passes the error through.
func (c *client) transportError(err error) error {
	var opErr *net.OpError
	var dnsErr *net.DNSError
	if errors.As(err, &opErr) || errors.As(err, &dnsErr) {
		return c.unreachable()
	}
	return fmt.Errorf("gateway %s: %w", c.base, err)
}

// apiError is a non-2xx reply, carrying the status so callers can special-case
// one (jev mode's 400) and show the gateway's own error text.
type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return e.Msg }

func (c *client) getRaw(path string) ([]byte, error) {
	resp, err := c.http.Get(c.base + path)
	if err != nil {
		return nil, c.transportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("gateway %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &apiError{Status: resp.StatusCode, Msg: gatewayError(path, resp.StatusCode, body)}
	}
	return body, nil
}

func (c *client) get(path string, out any) error {
	body, err := c.getRaw(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("gateway %s: unexpected payload: %w", path, err)
	}
	return nil
}

// post sends a JSON body. application/json is one of the two content types
// routePostAllowed accepts (the other is application/x-www-form-urlencoded,
// what a bare `curl -d '{...}'` sends); anything else is refused with 415. No
// Origin header is set, so the foreign-origin check passes — that guard exists
// to stop a web page, and a local process is not one.
func (c *client) post(path string, body any) (routeState, error) {
	var out routeState
	buf, err := json.Marshal(body)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(buf))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return out, c.transportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, fmt.Errorf("gateway %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return out, &apiError{Status: resp.StatusCode, Msg: gatewayError(path, resp.StatusCode, reply)}
	}
	if err := json.Unmarshal(reply, &out); err != nil {
		return out, fmt.Errorf("gateway %s: unexpected payload: %w", path, err)
	}
	return out, nil
}

// gatewayError turns an error reply into one line. The gateway's errors are
// JSON ({"error":"..."}) so the envelope is unwrapped when present, leaving the
// bare message; anything else is passed through as text.
func gatewayError(path string, status int, body []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != "" {
		msg = parsed.Error
	}
	return fmt.Sprintf("%s: HTTP %d: %s", path, status, msg)
}

// routeState is the reply to POST /_gateway/route.
type routeState struct {
	Status   string `json:"status"`
	Mode     string `json:"mode"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// routeInfo is the subset of GET /_gateway/route this client reads.
type routeInfo struct {
	Mode           string              `json:"mode"`
	ForcedProvider string              `json:"forced_provider"`
	ForcedModel    string              `json:"forced_model"`
	Available      map[string][]string `json:"available"`
	Jev            struct {
		Enabled bool `json:"enabled"`
	} `json:"jev"`
	LastRequest map[string]string `json:"last_request"`
}

// statusInfo is the subset of GET /_gateway/status this client reads.
type statusInfo struct {
	Listen         string `json:"listen"`
	Routing        string `json:"routing"`
	Mode           string `json:"mode"`
	ForcedProvider string `json:"forced_provider"`
	ForcedModel    string `json:"forced_model"`
	JevEnabled     bool   `json:"jev_enabled"`
	Breaker        struct {
		Entries map[string]struct {
			State  string    `json:"state"`
			Until  time.Time `json:"until"`
			Reason string    `json:"reason"`
		} `json:"entries"`
	} `json:"breaker"`
	Counts struct {
		AnthropicRequests int64 `json:"anthropic_requests"`
		GLMRequests       int64 `json:"glm_requests"`
		DeepSeekRequests  int64 `json:"deepseek_requests"`
		Failovers         int64 `json:"failovers"`
		TransientRetries  int64 `json:"transient_retries"`
		JevDecisions      int64 `json:"jev_decisions"`
		JevFailOpen       int64 `json:"jev_fail_open"`
	} `json:"counts"`
	LastRequest map[string]string `json:"last_request"`
}
