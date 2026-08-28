package virtual

import (
	"strings"
	"testing"
)

// Ground truth captured on 2026-08-28 against the live upstream, by holding a
// /v1/responses request constant and varying one block of text so that fixed request
// overhead cancels out of the delta.
const (
	measuredEnglishRunes  = 5920
	measuredEnglishTokens = 1040
	measuredChineseRunes  = 4560
	measuredChineseTokens = 3599
)

// The estimator sizes billing holds and the scheduler's per-request weight, so it must
// never come in UNDER the real token count. Before this was script-aware it undercounted
// Chinese by ~3.2x, meaning CJK traffic reserved a third of what it actually spent.
func TestEstimateStaysAboveMeasuredRealTokenCounts(t *testing.T) {
	english := strings.Repeat("The quick brown fox jumps over the lazy dog while the "+
		"maintainer reviews a pull request. ", 1)
	english = strings.Repeat(english, measuredEnglishRunes/len(english)+1)[:measuredEnglishRunes]

	chineseUnit := "这个网关需要在不破坏上下文的前提下提高缓存命中率"
	var zh strings.Builder
	for len([]rune(zh.String())) < measuredChineseRunes {
		zh.WriteString(chineseUnit)
	}
	chinese := string([]rune(zh.String())[:measuredChineseRunes])

	for _, tc := range []struct {
		name string
		text string
		real int64
	}{
		{"english", english, measuredEnglishTokens},
		{"chinese", chinese, measuredChineseTokens},
	} {
		got := EstimateTokensText(tc.text)
		if got < tc.real {
			t.Errorf("%s: estimate %d is BELOW the measured real count %d; an admission "+
				"gate that under-reserves is the failure this exists to prevent",
				tc.name, got, tc.real)
		}
		// Guard the other side too: a wildly inflated estimate would reject traffic that
		// would have fit. 2x is generous but keeps the rule honest.
		if got > 2*tc.real {
			t.Errorf("%s: estimate %d is more than 2x the measured real count %d",
				tc.name, got, tc.real)
		}
		t.Logf("%-8s runes=%d real=%d estimate=%d margin=%.2fx",
			tc.name, len([]rune(tc.text)), tc.real, got, float64(got)/float64(tc.real))
	}
}

// Pure-ASCII bodies must estimate exactly as they did before the change, so the fix
// cannot move English routing or holds.
func TestEstimateIsUnchangedForASCII(t *testing.T) {
	for _, s := range []string{
		"",
		"a",
		strings.Repeat("x", 4000),
		`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"hello"}]}`,
	} {
		want := int64(len(s)/4 + 1)
		if s == "" {
			want = 0
		}
		if got := EstimateTokensText(s); got != want {
			t.Errorf("ASCII estimate changed for len=%d: got %d want %d", len(s), got, want)
		}
	}
}

// CJK must cost roughly one token per character, not a quarter of one.
func TestCJKCostsAboutOneTokenPerRune(t *testing.T) {
	text := strings.Repeat("缓存命中率", 400) // 2000 runes, 6000 bytes
	got := EstimateTokensText(text)
	perRune := float64(got) / 2000.0
	if perRune < 0.8 || perRune > 1.3 {
		t.Errorf("CJK estimate is %.3f tokens/rune (total %d); measured reality is ~0.79, "+
			"so a correct conservative estimate lands near 1.0", perRune, got)
	}
}

func TestEstimateTokensJSONMatchesTextForSameBytes(t *testing.T) {
	body := []byte(`{"text":"混合 mixed content 内容"}`)
	if a, b := EstimateTokensJSON(body), EstimateTokensText(string(body)); a != b {
		t.Errorf("JSON and text estimates disagree for identical bytes: %d vs %d", a, b)
	}
	if EstimateTokensJSON(nil) != 1 {
		t.Errorf("empty body estimate = %d, want 1", EstimateTokensJSON(nil))
	}
}
