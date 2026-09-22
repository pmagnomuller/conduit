package classify_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pedro-mueller/conduit/internal/classify"
)

func TestClassifyTable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		hdr    http.Header
		body   string
		want   classify.Kind
		reason string // substring
	}{
		{
			name:   "429 quota with unified headers",
			status: 429,
			hdr: http.Header{
				"Anthropic-Ratelimit-Unified-Status":   []string{"rejected"},
				"Anthropic-Ratelimit-Unified-5h-Reset": []string{"9999999999"},
				"Retry-After":                          []string{"1847"},
			},
			body:   `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`,
			want:   classify.Quota,
			reason: "unified",
		},
		{
			name:   "429 capacity throttle without unified headers",
			status: 429,
			hdr:    http.Header{"X-Should-Retry": []string{"true"}},
			body:   `{"type":"error","error":{"type":"rate_limit_error","message":"Server is temporarily limiting requests (not your usage limit)"}}`,
			want:   classify.Transient,
			reason: "capacity_throttle",
		},
		{
			name:   "429 rate_limit_error no unified headers treated transient",
			status: 429,
			hdr:    http.Header{},
			body:   `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited. Please try again later."}}`,
			want:   classify.Transient,
			reason: "no_unified",
		},
		{
			name:   "403 billing_error",
			status: 403,
			hdr:    http.Header{},
			body:   `{"type":"error","error":{"type":"billing_error","message":"Credit balance too low"}}`,
			want:   classify.Quota,
			reason: "billing_error",
		},
		{
			name:   "403 permission_error",
			status: 403,
			hdr:    http.Header{},
			body:   `{"type":"error","error":{"type":"permission_error","message":"Not allowed"}}`,
			want:   classify.Auth,
			reason: "permission_error",
		},
		{
			name:   "401",
			status: 401,
			hdr:    http.Header{},
			body:   `{"type":"error","error":{"type":"authentication_error","message":"invalid token"}}`,
			want:   classify.Auth,
			reason: "401",
		},
		{
			name:   "529 overloaded",
			status: 529,
			hdr:    http.Header{},
			body:   `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			want:   classify.Transient,
			reason: "overloaded_error",
		},
		{
			name:   "500",
			status: 500,
			hdr:    http.Header{},
			body:   `{"type":"error","error":{"type":"api_error","message":"boom"}}`,
			want:   classify.Transient,
			reason: "api_error",
		},
		{
			name:   "502",
			status: 502,
			hdr:    http.Header{},
			body:   `bad gateway`,
			want:   classify.Transient,
			reason: "502",
		},
		{
			name:   "503",
			status: 503,
			hdr:    http.Header{},
			body:   `unavailable`,
			want:   classify.Transient,
			reason: "503",
		},
		{
			name:   "400 client error",
			status: 400,
			hdr:    http.Header{},
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`,
			want:   classify.ClientError,
			reason: "400",
		},
		{
			name:   "2xx ok",
			status: 200,
			hdr:    http.Header{},
			body:   `{}`,
			want:   classify.OK,
			reason: "2xx",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify.ClassifyAnthropic(tc.status, tc.hdr, []byte(tc.body))
			if got.Kind != tc.want {
				t.Fatalf("kind=%v want=%v reason=%s", got.Kind, tc.want, got.Reason)
			}
			if tc.reason != "" && !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("reason=%q want substring %q", got.Reason, tc.reason)
			}
		})
	}
}

func TestRetryAfterUntil(t *testing.T) {
	hdr := http.Header{"Retry-After": []string{"120"}}
	until := classify.UntilFromHeaders(hdr, time.Time{})
	if until.IsZero() {
		t.Fatal("expected until from retry-after")
	}
	delta := time.Until(until)
	if delta < 100*time.Second || delta > 130*time.Second {
		t.Fatalf("until delta=%s", delta)
	}
}

func TestUnifiedResetUntil(t *testing.T) {
	reset := time.Now().Add(45 * time.Minute).Unix()
	hdr := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Reset": []string{itoa(reset)},
	}
	until := classify.UntilFromHeaders(hdr, time.Time{})
	if until.Unix() != reset {
		t.Fatalf("until=%d want=%d", until.Unix(), reset)
	}
}

func TestProactiveUtilization(t *testing.T) {
	hdr := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.99"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       []string{itoa(time.Now().Add(time.Hour).Unix())},
	}
	ok, reason, _ := classify.ProactiveQuota(hdr, 0, 0.95)
	if !ok {
		t.Fatalf("expected proactive open, reason=%s", reason)
	}
}

func TestProactiveRemainingDisabled(t *testing.T) {
	hdr := http.Header{"Anthropic-Ratelimit-Requests-Remaining": []string{"0"}}
	ok, _, _ := classify.ProactiveQuota(hdr, 0, 0)
	if ok {
		t.Fatal("proactive_threshold=0 should disable remaining check")
	}
}

func TestProactiveStatusHeaderIgnored(t *testing.T) {
	hdr := http.Header{
		"Anthropic-Ratelimit-Unified-Status":   []string{"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Reset": []string{itoa(time.Now().Add(time.Hour).Unix())},
	}
	ok, reason, _ := classify.ProactiveQuota(hdr, 0, 0)
	if ok {
		t.Fatalf("2xx -status header must not open breaker, got reason=%s", reason)
	}
}

func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}
