package usage

import (
	"encoding/json"
	"strings"
)

type Normalized struct {
	Provider        string        `json:"provider"`
	InputTotal      *int64        `json:"input_total"`
	InputUncached   *int64        `json:"input_uncached"`
	CachedRead      *int64        `json:"cached_read"`
	CacheWrite      *int64        `json:"cache_write"`
	OutputTotal     *int64        `json:"output_total"`
	OutputReasoning *int64        `json:"output_reasoning"`
	TotalReported   *int64        `json:"total_reported"`
	Presence        FieldPresence `json:"usage_field_presence"`
	PresenceJSON    string        `json:"field_presence_json"`
	SettlementState string        `json:"settlement_state"`
	IntegrityError  string        `json:"integrity_error,omitempty"`
}

func int64Value(value int64) *int64 { return &value }

func Normalize(parsed Parsed, providerHint, usageSource string, estimated bool) Normalized {
	provider := strings.ToLower(strings.TrimSpace(providerHint))
	switch provider {
	case "codex", "openai", "openai_chat":
		provider = "openai"
	case "claude":
		provider = "anthropic"
	}
	if provider == "" {
		raw, _ := DecodeObject(parsed.RawUsage)
		if isAnthropicCacheUsage(raw) || strings.HasPrefix(strings.ToLower(parsed.Model), "claude") {
			provider = "anthropic"
		} else {
			provider = "openai"
		}
	}

	presence := parsed.Presence
	if len(parsed.RawUsage) > 0 {
		if raw, err := DecodeObject(parsed.RawUsage); err == nil {
			observed := usageFieldPresence(raw)
			presence.InputTotal = presence.InputTotal || observed.InputTotal
			presence.CachedRead = presence.CachedRead || observed.CachedRead
			presence.CacheWrite = presence.CacheWrite || observed.CacheWrite
			presence.OutputTotal = presence.OutputTotal || observed.OutputTotal
			presence.OutputReasoning = presence.OutputReasoning || observed.OutputReasoning
			presence.TotalReported = presence.TotalReported || observed.TotalReported
		}
	}
	normalized := Normalized{Provider: provider, Presence: presence, IntegrityError: parsed.IntegrityError}
	if parsed.PromptTokens < 0 || parsed.CompletionTokens < 0 || parsed.TotalTokens < 0 || parsed.CachedTokens < 0 || parsed.CacheReadTokens < 0 || parsed.CacheCreationTokens < 0 || parsed.OutputReasoningTokens < 0 {
		normalized.IntegrityError = "negative_usage_component"
	}
	if presence.InputTotal {
		normalized.InputTotal = int64Value(parsed.PromptTokens)
	}
	if presence.CachedRead {
		normalized.CachedRead = int64Value(parsed.CacheReadTokens)
	}
	if presence.CacheWrite {
		normalized.CacheWrite = int64Value(parsed.CacheCreationTokens)
	}
	if presence.OutputTotal {
		normalized.OutputTotal = int64Value(parsed.CompletionTokens)
	}
	if presence.OutputReasoning {
		normalized.OutputReasoning = int64Value(parsed.OutputReasoningTokens)
	}
	if presence.TotalReported {
		normalized.TotalReported = int64Value(parsed.TotalTokens)
	}

	switch provider {
	case "anthropic":
		if presence.InputTotal {
			normalized.InputUncached = int64Value(parsed.PromptTokens)
		}
		if presence.InputTotal && presence.CachedRead && presence.CacheWrite {
			if total, ok := addNonNegative(parsed.PromptTokens, parsed.CacheReadTokens, parsed.CacheCreationTokens); ok {
				normalized.InputTotal = int64Value(total)
			} else if normalized.IntegrityError == "" {
				normalized.IntegrityError = "cache_input_overflow"
			}
		}
	default:
		if presence.InputTotal && presence.CachedRead {
			uncached := parsed.PromptTokens - parsed.CacheReadTokens
			if uncached < 0 {
				uncached = 0
			}
			normalized.InputUncached = int64Value(uncached)
		}
	}

	if normalized.OutputReasoning != nil && normalized.OutputTotal != nil && *normalized.OutputReasoning > *normalized.OutputTotal && normalized.IntegrityError == "" {
		normalized.IntegrityError = "reasoning_exceeds_output"
	}
	if normalized.CachedRead != nil && normalized.InputTotal != nil && provider != "anthropic" && *normalized.CachedRead > *normalized.InputTotal && normalized.IntegrityError == "" {
		normalized.IntegrityError = "cached_read_exceeds_input"
	}
	if normalized.TotalReported != nil && normalized.InputTotal != nil && normalized.OutputTotal != nil {
		if rebuilt, ok := addNonNegative(*normalized.InputTotal, *normalized.OutputTotal); !ok {
			if normalized.IntegrityError == "" {
				normalized.IntegrityError = "reported_total_overflow"
			}
		} else if rebuilt != *normalized.TotalReported && normalized.IntegrityError == "" {
			normalized.IntegrityError = "reported_total_mismatch"
		}
	}
	presenceRaw, _ := json.Marshal(presence)
	normalized.PresenceJSON = string(presenceRaw)
	switch {
	case normalized.IntegrityError != "":
		normalized.SettlementState = "integrity_error"
	case estimated:
		normalized.SettlementState = "provisional"
	case strings.EqualFold(strings.TrimSpace(usageSource), "upstream") && normalized.InputTotal != nil && normalized.OutputTotal != nil:
		normalized.SettlementState = "settled"
	case normalized.InputTotal != nil || normalized.OutputTotal != nil:
		normalized.SettlementState = "partial"
	default:
		normalized.SettlementState = "unsettled"
	}
	return normalized
}
