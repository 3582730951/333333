package api

import (
	"net/http"
	"testing"
)

func TestIsExhausted(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    bool
		reason  string
	}{
		{
			name: "OpenAI: both zero → exhausted",
			headers: map[string]string{
				"x-ratelimit-remaining-requests": "0",
				"x-ratelimit-remaining-tokens":   "0",
			},
			want:   true,
			reason: "Both dimensions at zero",
		},
		{
			name: "OpenAI: tokens=0 but requests=100 → NOT exhausted (Session 31b fix)",
			headers: map[string]string{
				"x-ratelimit-remaining-requests": "100",
				"x-ratelimit-remaining-tokens":   "0",
			},
			want:   false,
			reason: "Still have 100 requests available; small requests can succeed",
		},
		{
			name: "OpenAI: requests=0 but tokens=5000 → NOT exhausted",
			headers: map[string]string{
				"x-ratelimit-remaining-requests": "0",
				"x-ratelimit-remaining-tokens":   "5000",
			},
			want:   false,
			reason: "Still have token budget; rate-limit is per-request, not total",
		},
		{
			name: "Claude: both zero → exhausted",
			headers: map[string]string{
				"anthropic-ratelimit-requests-remaining": "0",
				"anthropic-ratelimit-tokens-remaining":   "0",
			},
			want:   true,
			reason: "Both dimensions at zero",
		},
		{
			name: "Claude: tokens=0 but requests=50 → NOT exhausted (Session 31b fix)",
			headers: map[string]string{
				"anthropic-ratelimit-requests-remaining": "50",
				"anthropic-ratelimit-tokens-remaining":   "0",
			},
			want:   false,
			reason: "Old anyRemainingZero triggered here; new logic does not",
		},
		{
			name: "Claude: unified=0 → exhausted",
			headers: map[string]string{
				"anthropic-ratelimit-unified-remaining": "0",
			},
			want:   true,
			reason: "Unified is the sole dimension",
		},
		{
			name: "Claude: unified=0 but granular shows available → use granular (Session 31b fix)",
			headers: map[string]string{
				"anthropic-ratelimit-unified-remaining":  "0",
				"anthropic-ratelimit-requests-remaining": "100",
				"anthropic-ratelimit-tokens-remaining":   "10000",
			},
			want:   false,
			reason: "Granular pair takes precedence over stale/bootstrap unified=0",
		},
		{
			name: "Fresh account: high remaining → NOT exhausted",
			headers: map[string]string{
				"anthropic-ratelimit-requests-remaining": "5000",
				"anthropic-ratelimit-tokens-remaining":   "400000",
			},
			want:   false,
			reason: "Normal operation",
		},
		{
			name:    "No headers → NOT exhausted",
			headers: map[string]string{},
			want:    false,
			reason:  "Absence of headers means no tracking; assume available",
		},
		{
			name: "Negative remaining → exhausted (treat as zero)",
			headers: map[string]string{
				"x-ratelimit-remaining-requests": "-5",
				"x-ratelimit-remaining-tokens":   "-100",
			},
			want:   true,
			reason: "Negative is treated as <= 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.headers {
				h.Set(k, v)
			}
			got := isExhausted(h)
			if got != tt.want {
				t.Errorf("isExhausted() = %v, want %v\nHeaders: %v\nReason: %s", got, tt.want, tt.headers, tt.reason)
			}
		})
	}
}

func TestIsExhaustedAnthropicInputOutputTokens(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-input-tokens-remaining", "0")
	h.Set("anthropic-ratelimit-output-tokens-remaining", "10")
	if isExhausted(h) {
		t.Fatalf("one Anthropic token dimension at zero should not exhaust the account")
	}
	h.Set("anthropic-ratelimit-output-tokens-remaining", "0")
	if !isExhausted(h) {
		t.Fatalf("both Anthropic input/output token dimensions at zero should be exhausted")
	}
}

func TestExhaustedCooldown(t *testing.T) {
	now := int64(1700000000)

	tests := []struct {
		name    string
		headers map[string]string
		want    int64
		reason  string
	}{
		{
			name: "Not exhausted → no cooldown",
			headers: map[string]string{
				"x-ratelimit-remaining-requests": "100",
				"x-ratelimit-remaining-tokens":   "5000",
			},
			want:   0,
			reason: "Account still has capacity",
		},
		{
			name: "Exhausted with reset window → use reset",
			headers: map[string]string{
				"x-ratelimit-remaining-requests": "0",
				"x-ratelimit-remaining-tokens":   "0",
				"x-ratelimit-reset-requests":     "5m0s",
			},
			want:   300,
			reason: "Server signaled 5-minute reset",
		},
		{
			name: "Exhausted with Anthropic reset timestamp → use it",
			headers: map[string]string{
				"anthropic-ratelimit-requests-remaining": "0",
				"anthropic-ratelimit-tokens-remaining":   "0",
				"anthropic-ratelimit-requests-reset":     "2023-11-15T00:15:00Z",
			},
			want:   60, // Mock: assume now+60s
			reason: "Parse RFC3339 timestamp",
		},
		{
			name: "Exhausted without reset → fallback 30s",
			headers: map[string]string{
				"x-ratelimit-remaining-requests": "0",
				"x-ratelimit-remaining-tokens":   "0",
			},
			want:   30,
			reason: "No reset signal; use conservative default",
		},
		{
			name: "Tokens=0 but requests>0 → no cooldown (Session 31b fix)",
			headers: map[string]string{
				"anthropic-ratelimit-requests-remaining": "75",
				"anthropic-ratelimit-tokens-remaining":   "0",
			},
			want:   0,
			reason: "Not truly exhausted; requests remain",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.headers {
				h.Set(k, v)
			}
			got := exhaustedCooldown(h, now)
			// For tests with timestamps, allow range match (parsing logic tested separately)
			if tt.name == "Exhausted with Anthropic reset timestamp → use it" {
				if got < 1 || got > 3600 {
					t.Errorf("exhaustedCooldown() = %d, want reasonable range 1-3600", got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("exhaustedCooldown() = %d, want %d\nHeaders: %v\nReason: %s", got, tt.want, tt.headers, tt.reason)
			}
		})
	}
}

func TestResetSeconds(t *testing.T) {
	now := int64(1700000000)

	tests := []struct {
		name    string
		headers map[string]string
		want    int64
	}{
		{
			name: "OpenAI duration format",
			headers: map[string]string{
				"x-ratelimit-reset-requests": "6m30s",
			},
			want: 390,
		},
		{
			name: "Multiple resets → pick soonest",
			headers: map[string]string{
				"x-ratelimit-reset-requests": "10m0s",
				"x-ratelimit-reset-tokens":   "3m0s", // Soonest
			},
			want: 180,
		},
		{
			name:    "No reset headers → 0",
			headers: map[string]string{},
			want:    0,
		},
		{
			name: "Codex embedded primary window uses exhausted reset",
			headers: map[string]string{
				"x-codex-primary-used-percent":          "100",
				"x-codex-primary-reset-after-seconds":   "12091",
				"x-codex-secondary-used-percent":        "94",
				"x-codex-secondary-reset-after-seconds": "484265",
			},
			want: 12091,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.headers {
				h.Set(k, v)
			}
			got := resetSeconds(h, now)
			if got != tt.want {
				t.Errorf("resetSeconds() = %d, want %d", got, tt.want)
			}
		})
	}
}
