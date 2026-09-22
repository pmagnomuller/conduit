package redact

import (
	"net/http"
	"regexp"
	"strings"
)

var sensitiveHeaderNames = map[string]struct{}{
	"authorization":       {},
	"x-api-key":           {},
	"anthropic-beta":      {}, // may carry oauth beta; redact value entirely when logging
	"cookie":              {},
	"set-cookie":          {},
	"proxy-authorization": {},
}

// Header names whose values are replaced with [REDACTED] in any persisted/log output.
func IsSensitiveHeader(name string) bool {
	_, ok := sensitiveHeaderNames[strings.ToLower(name)]
	return ok
}

// Headers returns a copy of h with sensitive values redacted.
func Headers(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vals := range h {
		if IsSensitiveHeader(k) {
			out[k] = []string{"[REDACTED]"}
			continue
		}
		cp := make([]string, len(vals))
		copy(cp, vals)
		out[k] = cp
	}
	return out
}

// HeadersMap returns a flat map suitable for JSON logging.
func HeadersMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if IsSensitiveHeader(k) {
			out[k] = "[REDACTED]"
			continue
		}
		out[k] = strings.Join(vals, ", ")
	}
	return out
}

var (
	bearerRE = regexp.MustCompile(`(?i)(Bearer\s+)[^\s"']+`)
	apiKeyRE = regexp.MustCompile(`(?i)(x-api-key["'\s:=]+)[^\s"',}]+`)
	skRE     = regexp.MustCompile(`(?i)\b(sk-ant-[a-z0-9\-_]+|sk-[a-z0-9]{20,})\b`)
)

// String redacts credential-like substrings from arbitrary text (log lines, bodies).
func String(s string) string {
	s = bearerRE.ReplaceAllString(s, "${1}[REDACTED]")
	s = apiKeyRE.ReplaceAllString(s, "${1}[REDACTED]")
	s = skRE.ReplaceAllString(s, "[REDACTED]")
	return s
}

// Bytes redacts credential-like substrings in a body destined for disk.
func Bytes(b []byte) []byte {
	return []byte(String(string(b)))
}

// ContainsCredential reports whether raw still appears to hold a secret value.
// Used by security tests.
func ContainsCredential(haystack, secret string) bool {
	if secret == "" {
		return false
	}
	return strings.Contains(haystack, secret)
}
