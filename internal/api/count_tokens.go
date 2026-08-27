package api

import (
	"codex-account-pool/internal/tokenizer"
	"codex-account-pool/internal/virtual"
)

// countInputTokens returns the input-token count for a request body, using the embedded
// o200k_base tokenizer and falling back to the historical rune/4 estimate only when the
// vocabulary cannot be loaded — so a tokenizer problem degrades the number instead of
// failing the request.
//
// How exact the result is depends on the target family:
//
//   - GPT-family models: o200k_base IS the tokenizer those models use, so the count is
//     exact (framing aside).
//   - Claude served through a relay, Kiro, Antigravity: o200k is NOT that provider's
//     tokenizer, so the result is an approximation. It is still far closer than what it
//     replaces, and the routes that report an approximation already say so on the wire
//     (Kiro sets "estimated": true / usage_source=estimated).
//
// The first-party Claude route deliberately does NOT use this: it forwards
// /v1/messages/count_tokens upstream and returns Anthropic's own exact number. These
// callers cannot do that, because counting must keep working without acquiring an
// account or egress lease — a provider with no account, cooling down, or saturated must
// still be able to answer a count.
//
// Why this replaced virtual.EstimateTokensJSON: that estimate ran utf8.RuneCount over
// the RAW JSON and divided by four, counting field names, braces and \uXXXX escapes as
// content while charging one token per four characters regardless of script. Measured on
// real shapes it undercounts Chinese text ~2.5x and overcounts a tool-heavy ASCII body
// ~1.1x. The errors run in opposite directions, so they do not cancel — they make the
// number unpredictable, which is how totals drifted tens of thousands of tokens from the
// provider's own accounting.
func countInputTokens(raw []byte) int64 {
	if exact, ok := tokenizer.CountRequestTokens(raw); ok {
		return exact
	}
	return virtual.EstimateTokensJSON(raw)
}

// countCodexInputTokens is countInputTokens on a route that is always served by a
// GPT-family model, where the count is exact rather than an approximation.
func countCodexInputTokens(raw []byte) int64 { return countInputTokens(raw) }
