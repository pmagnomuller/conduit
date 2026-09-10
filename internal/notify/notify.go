package notify

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// RouteFile is the default path statusline / tools read for current provider.
func RouteFile() string {
	if v := os.Getenv("CONDUIT_ROUTE_PATH"); v != "" {
		return v
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "conduit", "route.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "conduit", "route.json")
}

type Route struct {
	Provider string    `json:"provider"` // anthropic | glm | deepseek
	Model    string    `json:"model,omitempty"`
	// UpstreamModel is the rewritten model actually sent to the provider
	// (e.g. glm-5.2, deepseek-v4-flash); equals Model on the anthropic path.
	UpstreamModel string    `json:"upstream_model,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	At            time.Time `json:"at"`
	Notice        string    `json:"notice,omitempty"` // short human string for statusline
}

var (
	mu       sync.Mutex
	desktop  = true
	chatOn   = true
	writeFn  = writeRouteFile
	notifyFn = desktopNotify

	lastDeepSeekToast time.Time
)

// SetEnabled controls desktop notifications and chat notice injection.
// CONDUIT_NOTIFY=0 disables both; CONDUIT_DESKTOP_NOTIFY=0 disables desktop only.
func SetEnabledFromEnv() {
	if os.Getenv("CONDUIT_NOTIFY") == "0" {
		desktop = false
		chatOn = false
		return
	}
	if os.Getenv("CONDUIT_DESKTOP_NOTIFY") == "0" {
		desktop = false
	}
	if os.Getenv("CONDUIT_CHAT_NOTICE") == "0" {
		chatOn = false
	}
}

func ChatNoticeEnabled() bool { return chatOn }

// FailoverToGLM records GLM routing and fires a one-shot desktop notification.
func FailoverToGLM(model, upstreamModel, reason string) {
	mu.Lock()
	defer mu.Unlock()
	notice := fmt.Sprintf("GLM · %s", shortReason(reason))
	_ = writeFn(Route{
		Provider:      "glm",
		Model:         model,
		UpstreamModel: upstreamModel,
		Reason:        reason,
		At:            time.Now().UTC(),
		Notice:        notice,
	})
	if desktop {
		msg := fmt.Sprintf("Switched to GLM (Z.ai)\nmodel=%s\n%s", upstreamModel, reason)
		notifyFn("conduit", msg)
	}
}

// FailoverToDeepSeek records DeepSeek routing. Unlike the GLM path there is no
// breaker gating it, so the desktop toast is throttled to one per 5 minutes to
// avoid spamming while GLM is hard-down. The route file is written every time.
func FailoverToDeepSeek(model, upstreamModel, reason string) {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now().UTC()
	notice := fmt.Sprintf("DeepSeek · %s", shortReason(reason))
	_ = writeFn(Route{
		Provider:      "deepseek",
		Model:         model,
		UpstreamModel: upstreamModel,
		Reason:        reason,
		At:            now,
		Notice:        notice,
	})
	if desktop && now.Sub(lastDeepSeekToast) > 5*time.Minute {
		lastDeepSeekToast = now
		msg := fmt.Sprintf("Switched to DeepSeek (GLM failed)\nmodel=%s\n%s", upstreamModel, reason)
		notifyFn("conduit", msg)
	}
}

// BackToAnthropic records Anthropic routing (no desktop toast by default).
func BackToAnthropic(model string) {
	mu.Lock()
	defer mu.Unlock()
	_ = writeFn(Route{
		Provider:      "anthropic",
		Model:         model,
		UpstreamModel: model,
		At:            time.Now().UTC(),
		Notice:        "Anthropic",
	})
}

func shortReason(reason string) string {
	if reason == "" {
		return "quota"
	}
	if len(reason) > 40 {
		return reason[:40]
	}
	return reason
}

func writeRouteFile(r Route) error {
	path := RouteFile()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func desktopNotify(title, message string) {
	switch runtime.GOOS {
	case "darwin":
		// Escape for AppleScript string literals.
		esc := func(s string) string {
			out := make([]rune, 0, len(s))
			for _, r := range s {
				switch r {
				case '\\', '"':
					out = append(out, '\\', r)
				default:
					out = append(out, r)
				}
			}
			return string(out)
		}
		script := fmt.Sprintf(`display notification "%s" with title "%s"`, esc(message), esc(title))
		_ = exec.Command("osascript", "-e", script).Run()
	case "linux":
		_ = exec.Command("notify-send", title, message).Run()
	}
}
