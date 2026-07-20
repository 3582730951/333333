// Package ban classifies an upstream response into an account-liveness verdict,
// separating a HARD ban/deactivation (the account is dead and must be removed or
// quarantined) from recoverable conditions (rate limit, expired token, region
// block, transient error). The ban heuristics are ported verbatim from the
// reference Codex Manager's account_status classifier so the relay reacts to
// exactly the signals OpenAI/Anthropic actually emit — and, crucially, never
// deletes an account for a merely transient/recoverable reason.
package ban

import (
	"net/http"
	"strconv"
	"strings"
)

type State string

const (
	Alive            State = "alive"             // 2xx, or an error unrelated to account validity
	Banned           State = "banned"            // deactivated/suspended — TERMINAL, high confidence
	RateLimited      State = "rate_limited"      // usage/quota/429 — recoverable after cooldown
	AuthExpired      State = "auth_expired"      // token expired/revoked/invalid — refresh
	PermissionDenied State = "permission_denied" // valid token/account, but missing role/scope for this API
	RegionBlocked    State = "region_blocked"    // geo / CF region block — change egress, NOT a ban
	Unknown          State = "unknown"           // could not classify
)

type Verdict struct {
	State  State
	Reason string
}

// IsBanned reports the high-confidence terminal verdict that authorizes an
// automated destructive action (delete/quarantine).
func (v Verdict) IsBanned() bool { return v.State == Banned }

// IsAccountLevel reports whether the verdict represents an account-level failure
// (ban, auth expiry) vs a function-level restriction (permission denied, region
// block). Account-level failures justify account removal/quarantine; function-level
// restrictions should only trigger failover or transparent error surfacing.
func (v Verdict) IsAccountLevel() bool {
	return v.State == Banned || v.State == AuthExpired
}

// Classify inspects an upstream response. ok marks a 2xx success (always Alive).
// For errors it scans the body plus the x-error-json header (where OpenAI surfaces
// identity error codes) in priority order: ban → region → auth → rate-limit, so a
// recoverable condition is never mis-read as a ban.
func Classify(ok bool, status int, header http.Header, body []byte) Verdict {
	if ok {
		return Verdict{Alive, ""}
	}
	hay := strings.ToLower(string(body))
	if header != nil {
		if x := header.Get("x-error-json"); x != "" {
			hay += "\n" + strings.ToLower(x)
		}
	}

	// 1) Hard ban / deactivation — terminal, highest confidence. (Ported from the
	// reference manager's deactivation_reason_from_message + is_banned_status_reason.)
	//
	// Kiro/AWS can return the Builder/User ID security suspension as HTTP 503 even
	// though it is an account-level terminal condition.  Do not key off the bare
	// word "suspended": model/error text can contain it incidentally.  The awkward
	// exact suspension wording plus the security-lock / identity-support context is
	// the high-confidence fingerprint AWS sends from generateAssistantResponse.
	if awsUserSuspended(status, hay) {
		return Verdict{Banned, "aws_user_suspended"}
	}
	for _, sig := range []string{"workspace_deactivated", "deactivated_workspace", "workspace deactivated", "workspace-deactivated", "deactivated workspace"} {
		if strings.Contains(hay, sig) {
			return Verdict{Banned, "workspace_deactivated"}
		}
	}
	for _, sig := range []string{"account_deactivated", "account deactivated"} {
		if strings.Contains(hay, sig) {
			return Verdict{Banned, "account_deactivated"}
		}
	}
	for _, sig := range []string{"account_suspended", "account suspended", "permanently banned", "your account has been disabled", "has been deactivated"} {
		if strings.Contains(hay, sig) {
			return Verdict{Banned, "account_suspended"}
		}
	}
	// Bare "deactivated" is a ban signal in the reference, but only trust it on an
	// auth/permission failure so the word can't match unrelated model output.
	if (status == 401 || status == 403) && strings.Contains(hay, "deactivated") {
		return Verdict{Banned, "account_deactivated"}
	}

	// 2) Region / geo block — recoverable by changing egress, NOT a ban.
	for _, sig := range []string{"unsupported_country_region_territory", "unsupported_country", "region_restricted", "country, region, or territory not supported"} {
		if strings.Contains(hay, sig) {
			return Verdict{RegionBlocked, sig}
		}
	}

	// 3) Permission / scope failures — the credential is real but cannot perform
	// this operation. Refreshing the same grant does not fix a missing scope.
	if status == 401 || status == 403 {
		for _, sig := range []string{"api.responses.write", "missing scopes", "missing scope", "insufficient_scope", "insufficient permissions"} {
			if strings.Contains(hay, sig) {
				return Verdict{PermissionDenied, sig}
			}
		}
	}

	// 4) Refresh/auth-token problems — recoverable by refresh, NOT a ban.
	if status == 401 || status == 403 {
		for _, sig := range []string{"invalid_grant", "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated", "token has expired", "token expired", "invalid api key", "invalid_api_key", "authentication_error"} {
			if strings.Contains(hay, sig) {
				return Verdict{AuthExpired, sig}
			}
		}
	}

	// 5) Usage / quota / rate limit — recoverable after cooldown.
	for _, sig := range []string{"you've hit your usage limit", "you have hit your usage limit", "usage limit has been reached", "usage limit", "insufficient_quota", "quota exceeded", "exceeded your current quota", "usage exhausted", "rate_limit_exceeded", "too many requests", "overloaded"} {
		if strings.Contains(hay, sig) {
			return Verdict{RateLimited, "usage_limit"}
		}
	}
	if status == 429 {
		return Verdict{RateLimited, "http_429"}
	}
	if status == 401 || status == 403 {
		return Verdict{AuthExpired, "http_" + strconv.Itoa(status)}
	}
	return Verdict{Unknown, ""}
}

func awsUserSuspended(status int, hay string) bool {
	if status < 400 {
		return false
	}
	awsIdentity := containsAny(hay,
		"aws user id", "aws builder id", "builder id temporarily", "builderid temporarily")
	temporarilySuspended := strings.Contains(hay, "temporarily is suspended")
	securityLock := strings.Contains(hay, "locked your account as a security precaution")
	identityVerification := containsAny(hay,
		"verify your identity", "identity verification", "verification of your identity")
	awsSupport := containsAny(hay,
		"contact aws support", "aws support", "support.aws.amazon.com", "aws.amazon.com/contact-us")

	return (awsIdentity && temporarilySuspended && securityLock) ||
		(securityLock && identityVerification && awsSupport) ||
		(awsIdentity && temporarilySuspended && identityVerification && awsSupport)
}

func containsAny(hay string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(hay, needle) {
			return true
		}
	}
	return false
}
