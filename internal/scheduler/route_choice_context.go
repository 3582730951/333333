package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// RouteChoiceClaim atomically resolves a speculative target choice. It returns
// the choice key every concurrent caller must follow. User-group routing uses it
// to implement claim-if-absent conversation bindings without teaching the
// scheduler about user-group storage tables.
type RouteChoiceClaim func(context.Context, string) (string, error)

type routeChoiceContextKey struct{}
type routeChoiceBypassKey struct{}
type cooldownTrialsRestrictedKey struct{}

// WithCooldownTrialsRestrictedToOverrides prevents a non-final routing target
// from using an ordinary cooled account as an intelligent-routing last-resort
// probe. Accounts carrying the explicit IgnoreRateLimitControls operator
// override remain probe-eligible. The API enables this only while another
// replay-safe, explicitly authorized target is available; the final target
// retains the normal all-targets-exhausted behavior.
func WithCooldownTrialsRestrictedToOverrides(ctx context.Context) context.Context {
	return context.WithValue(ctx, cooldownTrialsRestrictedKey{}, true)
}

func cooldownTrialsRestrictedToOverrides(ctx context.Context) bool {
	restricted, _ := ctx.Value(cooldownTrialsRestrictedKey{}).(bool)
	return restricted
}

// RouteChoiceState exposes the target actually leased by the most recent Select
// call. A handler may retry account selection several times; each retry updates
// SelectedChoice while preserving one atomic cross-target decision per call.
type RouteChoiceState struct {
	mu        sync.Mutex
	templates []RouteChoice
	claim     RouteChoiceClaim
	// primary is the choice key of a strict first tier. When set, a schedulable
	// account in that group is selected directly without fallback competition, and
	// the remaining choices engage only when it has none. Group fallbacks use this
	// so a healthy primary group keeps serving instantly.
	primary   string
	selected  string
	claimed   string
	claimDone bool
	used      bool
}

// WithRouteChoices makes ordinary Scheduler.Select calls evaluate the supplied
// same-tier targets through SelectAcross. Handler code continues to construct the
// authoritative Route (model, provider, affinity, exclusions and token estimate);
// each template contributes only its target group and optional explicit fields.
func WithRouteChoices(ctx context.Context, choices []RouteChoice, claim RouteChoiceClaim) (context.Context, *RouteChoiceState) {
	state := &RouteChoiceState{templates: append([]RouteChoice(nil), choices...), claim: claim}
	return context.WithValue(ctx, routeChoiceContextKey{}, state), state
}

// WithPrimaryRouteChoices behaves like WithRouteChoices but treats the named
// choice as a strict first tier (see RouteChoiceState.primary): a schedulable
// account there is selected via the ordinary single-group path so a healthy
// primary group keeps serving instantly, and the fallback choices only engage
// when the primary group has no schedulable account.
func WithPrimaryRouteChoices(ctx context.Context, primary string, choices []RouteChoice, claim RouteChoiceClaim) (context.Context, *RouteChoiceState) {
	state := &RouteChoiceState{templates: append([]RouteChoice(nil), choices...), claim: claim, primary: primary}
	return context.WithValue(ctx, routeChoiceContextKey{}, state), state
}

func (s *RouteChoiceState) SelectedChoice() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selected
}

func (s *RouteChoiceState) Used() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

// ClaimedChoice is the durable first-selection winner. SelectedChoice may differ
// after a request-local retry moves replayable work away from a failed account or
// temporarily saturated target.
func (s *RouteChoiceState) ClaimedChoice() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimed
}

func (s *Scheduler) selectFromRouteChoiceContext(ctx context.Context, route Route) (Lease, bool, error) {
	if bypass, _ := ctx.Value(routeChoiceBypassKey{}).(bool); bypass {
		return Lease{}, false, nil
	}
	state, _ := ctx.Value(routeChoiceContextKey{}).(*RouteChoiceState)
	if state == nil || len(state.templates) == 0 {
		return Lease{}, false, nil
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	state.used = true
	if strings.TrimSpace(route.RequiredAccountID) != "" || strings.TrimSpace(route.RequiredEgressID) != "" {
		// Durable server-side session identity pins account+egress exactly.
		// The choice machinery (SelectAcross) evaluates whole account pools and
		// returns an arbitrary account with the group-selected egress, which the
		// session identity check rejects as an epoch conflict (409 "Codex session
		// identity is no longer available"). Delegate to the strict path that
		// honors the pins; the remaining choices engage only when it has no
		// schedulable account on the pinned outlet.
		bypassCtx := context.WithValue(ctx, routeChoiceBypassKey{}, true)
		lease, selectErr := s.Select(bypassCtx, route)
		if selectErr != nil {
			return Lease{}, true, selectErr
		}
		return lease, true, nil
	}
	bypassCtx := context.WithValue(ctx, routeChoiceBypassKey{}, true)
	choices := make([]RouteChoice, 0, len(state.templates))
	for _, template := range state.templates {
		merged := route
		// Route-level safety flags are part of the target template as well as the
		// caller's base route. Preserve them when the choice context swaps groups;
		// otherwise a pinned user-group route could accidentally lose its immutable
		// account/primary-egress boundary at the SelectAcross hand-off.
		if template.Route.NoEgressFallback {
			merged.NoEgressFallback = true
		}
		if template.Route.ImmutableAffinity {
			merged.ImmutableAffinity = true
		}
		if group := strings.TrimSpace(template.Route.Group); group != "" {
			merged.Group = group
			// Outlet order belongs to the target group. Do not copy an order resolved
			// for the dispatch placeholder into every cross-target candidate.
			merged.PreferredEgressIDs = nil
		}
		if provider := strings.TrimSpace(template.Route.Provider); provider != "" {
			merged.Provider = provider
			merged.AllowedProviders = nil
		}
		if len(template.Route.AllowedProviders) > 0 {
			merged.Provider = ""
			merged.AllowedProviders = append([]string(nil), template.Route.AllowedProviders...)
		}
		choices = append(choices, RouteChoice{ChoiceKey: template.ChoiceKey, Route: merged})
	}
	if state.primary != "" {
		// Primary-first: a schedulable account in the primary group is selected via
		// the ordinary single-group path so it serves instantly without competing
		// against fallback targets in the across coordinator's round-robin. Only a
		// NoAccountError (empty, quarantined, or cooldown-stuck past its trial)
		// falls through to the across evaluation where fallback groups step in.
		var primaryRoute *Route
		for i := range choices {
			if choices[i].ChoiceKey == state.primary {
				primaryRoute = &choices[i].Route
				break
			}
		}
		if primaryRoute != nil {
			lease, selectErr := s.Select(bypassCtx, *primaryRoute)
			if selectErr == nil {
				state.claimDone = true
				state.claimed = state.primary
				state.selected = state.primary
				return lease, true, nil
			}
			var nae *NoAccountError
			if !errors.As(selectErr, &nae) {
				return Lease{}, true, selectErr
			}
		}
	}
	routed, err := s.SelectAcross(bypassCtx, choices)
	if err != nil {
		return Lease{}, true, err
	}
	selected := routed.ChoiceKey
	if state.claim != nil && !state.claimDone {
		winner, claimErr := state.claim(bypassCtx, selected)
		winner = strings.TrimSpace(winner)
		if claimErr != nil || winner == "" {
			routed.Release()
			if claimErr != nil {
				return Lease{}, true, claimErr
			}
			return Lease{}, true, fmt.Errorf("route choice claim returned an empty winner")
		}
		if winner != selected {
			routed.Release()
			var winningRoute *Route
			for i := range choices {
				if choices[i].ChoiceKey == winner {
					winningRoute = &choices[i].Route
					break
				}
			}
			if winningRoute == nil {
				return Lease{}, true, fmt.Errorf("route choice claim selected unavailable winner %q", winner)
			}
			lease, selectErr := s.Select(bypassCtx, *winningRoute)
			if selectErr != nil {
				return Lease{}, true, selectErr
			}
			routed = RoutedLease{Lease: lease, ChoiceKey: winner}
			selected = winner
		}
		state.claimDone = true
		state.claimed = selected
	}
	state.selected = selected
	return routed.Lease, true, nil
}
