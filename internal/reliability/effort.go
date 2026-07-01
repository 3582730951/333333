package reliability

import "strings"

// Policy is the routing decision derived from a request's risk level: the reasoning
// effort floor and whether the model must plan / self-review before finishing. It is
// the gateway's equivalent of the spec's risk→model/effort table, with the model
// axis kept separate (configurable in the api layer) so this package never assumes a
// specific model exists in the pool.
type Policy struct {
	EffortFloor   string
	RequirePlan   bool
	RequireReview bool
}

// PolicyFor maps a risk level to its reasoning-effort floor and plan/review flags:
//
//	critical -> xhigh, plan + review
//	high     -> high,  plan
//	medium   -> medium,plan
//	low      -> low
func PolicyFor(risk RiskLevel) Policy {
	switch risk {
	case RiskCritical:
		return Policy{EffortFloor: "xhigh", RequirePlan: true, RequireReview: true}
	case RiskHigh:
		return Policy{EffortFloor: "high", RequirePlan: true}
	case RiskMedium:
		return Policy{EffortFloor: "medium", RequirePlan: true}
	default:
		return Policy{EffortFloor: "low"}
	}
}

// effortRank gives reasoning-effort tiers a total order so a floor can be applied
// without ever lowering an operator's explicit choice.
var effortRank = map[string]int{
	"minimal": 0,
	"low":     1,
	"medium":  2,
	"high":    3,
	"xhigh":   4,
}

// MaxEffort returns the stronger (higher) of two reasoning-effort tiers. Unknown /
// empty tiers are treated as "no constraint" and lose to any known tier. This is how
// a risk-derived floor combines with an operator-forced effort: the floor can RAISE
// the effort but a forced HIGHER effort is preserved, and a high-risk task can never
// be silently downgraded below its floor.
func MaxEffort(a, b string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	ra, aok := effortRank[a]
	rb, bok := effortRank[b]
	switch {
	case !aok && !bok:
		return ""
	case !aok:
		return b
	case !bok:
		return a
	case ra >= rb:
		return a
	default:
		return b
	}
}
