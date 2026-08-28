// Package plantier normalizes the many raw subscription-plan spellings that reach
// the pool into a small closed set of tiers.
//
// Raw plan strings arrive from mutually inconsistent sources: ChatGPT JWT claims
// (`chatgpt_plan_type`), `subscription_type` on /wham/usage, Codex stream rate-limit
// headers, Kiro and Cursor imports, and third-party account bundles. The same
// entitlement is spelled `pro`, `Pro`, `PRO`, `KIRO PRO`, `chatgpt-pro` and
// `Pro Plus` depending on who produced it. Storing the raw value is still correct —
// operators need to see what upstream actually said — but *comparing* or
// *authorizing* on it is not, which is what this package exists to stop.
//
// Deliberately not done here: any policy. This package answers "which tier is this
// string", never "what may this tier do". Entitlement decisions stay next to the
// data they guard (internal/capability), so a vocabulary change cannot silently
// move an authorization boundary.
package plantier

import "strings"

// Tier is a normalized subscription tier. The zero value means "not recognized",
// which callers must treat as "no information", never as a denial — an unknown
// spelling is far more likely to be a vocabulary gap than a free account.
type Tier string

const (
	Unknown    Tier = ""
	API        Tier = "api"
	Free       Tier = "free"
	Plus       Tier = "plus"
	Pro        Tier = "pro"
	Max        Tier = "max"
	Team       Tier = "team"
	Business   Tier = "business"
	Enterprise Tier = "enterprise"
)

func (t Tier) String() string { return string(t) }

// Known reports whether normalization actually recognized a tier.
func (t Tier) Known() bool { return t != Unknown }

// tierRank orders tiers by entitlement so a compound name resolves to its
// strongest component: `Plus Team` is a team plan that happens to say "plus", and
// resolving it downward would under-grant. Ranking is only used to break ties
// within one string; it is not an ordering callers should authorize against.
var tierRank = map[Tier]int{
	API:        1,
	Free:       2,
	Plus:       3,
	Pro:        4,
	Max:        5,
	Team:       6,
	Business:   7,
	Enterprise: 8,
}

// tokenTier maps a single whole token to a tier. Matching is by *exact token*
// rather than substring on purpose: `strings.Contains(plan, "free")` also fires on
// `freedom`, and `Contains(plan, "pro")` fires on `proxy` and `provisional`. A
// paid account misread as free loses model access with no log line explaining it.
var tokenTier = map[string]Tier{
	"api":        API,
	"apikey":     API,
	"payg":       API,
	"paygo":      API,
	"free":       Free,
	"plus":       Plus,
	"pro":        Pro,
	"max":        Max,
	"team":       Team,
	"teams":      Team,
	"business":   Business,
	"enterprise": Enterprise,
}

// Normalize resolves a raw plan string to a tier, returning Unknown when nothing
// in it is recognizable. It is total and allocation-light: no error path, because
// every caller's correct response to an unrecognized plan is the same as to an
// empty one.
func Normalize(raw string) Tier {
	best := Unknown
	bestRank := 0
	for _, token := range tokens(raw) {
		tier, ok := tokenTier[token]
		if !ok {
			continue
		}
		if rank := tierRank[tier]; rank > bestRank {
			best, bestRank = tier, rank
		}
	}
	return best
}

// SameTier reports whether two raw plan strings denote the same tier.
//
// When either side is unrecognized the comparison falls back to a case-insensitive
// match on the raw text. That fallback matters: it keeps a genuinely new value
// (including the first value ever seen for an account) looking "different" so it
// still gets persisted, instead of two unknown-but-unequal spellings collapsing
// into "no change" and freezing the stored plan forever.
func SameTier(a, b string) bool {
	left, right := Normalize(a), Normalize(b)
	if left.Known() && right.Known() {
		return left == right
	}
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// tokens splits a raw plan string into lowercase alphanumeric-run tokens, breaking
// on separators, on lower→upper transitions (`proPlus`) and between letters and
// digits (`max20x`). Those three cases cover every spelling shape observed from
// upstreams without needing a per-source table.
func tokens(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make([]string, 0, 4)
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	var prev rune
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if cur.Len() > 0 && isDigit(prev) != isDigit(r) {
				flush()
			}
			cur.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			if cur.Len() > 0 && !isUpper(prev) {
				flush()
			}
			cur.WriteRune(r - 'A' + 'a')
		default:
			flush()
		}
		prev = r
	}
	flush()
	return out
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isDigit(r rune) bool { return r >= '0' && r <= '9' }
