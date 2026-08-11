package classify

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Kind is how the gateway should treat an upstream Anthropic response.
type Kind int

const (
	OK Kind = iota
	Quota       // open breaker, fail over to GLM
	Transient   // retry Anthropic; do NOT fail over
	Auth        // 401 / permission — surface to client; never fail over
	ClientError // 400 etc — surface; never fail over
	Other       // unexpected; surface; never fail over
)

func (k Kind) String() string {
	switch k {
	case OK:
		return "ok"
	case Quota:
		return "quota"
	case Transient:
		return "transient"
	case Auth:
		return "auth"
	case ClientError:
		return "client_error"
	default:
		return "other"
	}
}

type Result struct {
	Kind   Kind
	Reason string
	Until  time.Time // zero if unknown; caller applies default
}

type anthropicErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ClassifyAnthropic inspects status, headers, and body of a non-streaming or
// pre-stream Anthropic error response.
//
// Subscription finding (see FINDINGS.md): Claude Code distinguishes real plan
// quota 429s from transient capacity throttles by the presence of
// anthropic-ratelimit-unified-* headers. Throttles that say "not your usage
// limit" / lack unified headers must NOT open the breaker.
func ClassifyAnthropic(status int, hdr http.Header, body []byte) Result {
	if status >= 200 && status < 300 {
		return Result{Kind: OK, Reason: "2xx"}
	}

	var parsed anthropicErrorBody
	_ = json.Unmarshal(body, &parsed)
	errType := parsed.Error.Type
	msg := strings.ToLower(parsed.Error.Message)

	switch status {
	case 401:
		return Result{Kind: Auth, Reason: "401"}
	case 400:
		return Result{Kind: ClientError, Reason: "400"}
	case 403:
		switch errType {
		case "billing_error":
			return Result{Kind: Quota, Reason: "billing_error", Until: untilFromHeaders(hdr, time.Time{})}
		case "permission_error":
			return Result{Kind: Auth, Reason: "permission_error"}
		default:
			if strings.Contains(msg, "credit") || strings.Contains(msg, "billing") {
				return Result{Kind: Quota, Reason: "403_billingish", Until: untilFromHeaders(hdr, time.Time{})}
			}
			return Result{Kind: Auth, Reason: "403_" + errType}
		}
	case 429:
		return classify429(hdr, errType, msg)
	case 500, 502, 503, 529:
		reason := strconv.Itoa(status)
		if errType != "" {
			reason = errType
		}
		return Result{Kind: Transient, Reason: reason}
	default:
		if status >= 500 {
			return Result{Kind: Transient, Reason: strconv.Itoa(status)}
		}
		return Result{Kind: Other, Reason: strconv.Itoa(status) + "_" + errType}
	}
}

func classify429(hdr http.Header, errType, msg string) Result {
	// Explicit capacity throttle (Claude Code docs).
	if strings.Contains(msg, "not your usage limit") ||
		strings.Contains(msg, "temporarily limiting") {
		return Result{Kind: Transient, Reason: "capacity_throttle"}
	}

	hasUnified := hasUnifiedQuotaHeaders(hdr)
	until := untilFromHeaders(hdr, time.Time{})

	// Real subscription / plan quota responses carry unified headers.
	if hasUnified {
		reason := "rate_limit_error"
		if s := unifiedStatus(hdr); s != "" {
			reason = "unified_" + s
		}
		return Result{Kind: Quota, Reason: reason, Until: until}
	}

	// No unified headers: treat as transient capacity throttle (Claude Code
	// distinguishing rule), even when error.type is rate_limit_error.
	if errType == "rate_limit_error" || errType == "" {
		return Result{Kind: Transient, Reason: "429_no_unified_headers"}
	}

	return Result{Kind: Transient, Reason: "429_" + errType}
}

func hasUnifiedQuotaHeaders(hdr http.Header) bool {
	for k := range hdr {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "anthropic-ratelimit-unified-") {
			return true
		}
	}
	return false
}

func unifiedStatus(hdr http.Header) string {
	if v := hdr.Get("anthropic-ratelimit-unified-status"); v != "" {
		return strings.ToLower(v)
	}
	// Fall back to any window status that looks exceeded.
	for k, vals := range hdr {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "anthropic-ratelimit-unified-") && strings.HasSuffix(lk, "-status") {
			for _, v := range vals {
				lv := strings.ToLower(v)
				if lv == "exceeded" || lv == "rejected" || lv == "rate_limited" {
					return lv
				}
			}
		}
	}
	return ""
}

// ProactiveQuota reports whether response headers indicate the subscription
// quota is exhausted or below configured thresholds — without requiring a 429.
func ProactiveQuota(hdr http.Header, remainingThreshold int, utilizationThreshold float64) (bool, string, time.Time) {
	until := untilFromHeaders(hdr, time.Time{})

	status := strings.ToLower(hdr.Get("anthropic-ratelimit-unified-status"))
	if status == "exceeded" || status == "rejected" || status == "rate_limited" {
		return true, "unified_status_" + status, until
	}

	for k, vals := range hdr {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "anthropic-ratelimit-unified-") {
			continue
		}
		if strings.HasSuffix(lk, "-status") {
			for _, v := range vals {
				lv := strings.ToLower(v)
				if lv == "exceeded" || lv == "rejected" || lv == "rate_limited" {
					return true, lk + "=" + lv, until
				}
			}
		}
		if utilizationThreshold > 0 && strings.HasSuffix(lk, "-utilization") {
			for _, v := range vals {
				f, err := strconv.ParseFloat(v, 64)
				if err == nil && f >= utilizationThreshold {
					return true, lk + "=" + v, until
				}
			}
		}
	}

	if remainingThreshold > 0 {
		for k, vals := range hdr {
			lk := strings.ToLower(k)
			if !strings.HasSuffix(lk, "-remaining") {
				continue
			}
			if !(strings.HasPrefix(lk, "anthropic-ratelimit-") || strings.HasPrefix(lk, "x-ratelimit-")) {
				continue
			}
			for _, v := range vals {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err == nil && n < remainingThreshold {
					return true, lk + "=" + v, until
				}
			}
		}
	}

	return false, "", time.Time{}
}

// UntilFromHeaders is exported for breaker tests.
func UntilFromHeaders(hdr http.Header, fallback time.Time) time.Time {
	return untilFromHeaders(hdr, fallback)
}

func untilFromHeaders(hdr http.Header, fallback time.Time) time.Time {
	now := time.Now()

	if ra := hdr.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
			return now.Add(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(ra); err == nil && t.After(now) {
			return t
		}
	}

	// Prefer unified reset (unix epoch seconds).
	candidates := []string{
		"anthropic-ratelimit-unified-5h-reset",
		"anthropic-ratelimit-unified-7d-reset",
		"anthropic-ratelimit-unified-reset",
	}
	for _, name := range candidates {
		if v := hdr.Get(name); v != "" {
			if sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && sec > 0 {
				t := time.Unix(sec, 0)
				if t.After(now) {
					return t
				}
			}
		}
	}

	// Scan any *-reset header.
	for k, vals := range hdr {
		lk := strings.ToLower(k)
		if !strings.HasSuffix(lk, "-reset") {
			continue
		}
		if !(strings.HasPrefix(lk, "anthropic-ratelimit-") || strings.HasPrefix(lk, "x-ratelimit-")) {
			continue
		}
		for _, v := range vals {
			v = strings.TrimSpace(v)
			if sec, err := strconv.ParseInt(v, 10, 64); err == nil && sec > 1_000_000_000 {
				t := time.Unix(sec, 0)
				if t.After(now) {
					return t
				}
				continue
			}
			if t, err := time.Parse(time.RFC3339, v); err == nil && t.After(now) {
				return t
			}
		}
	}

	return fallback
}
