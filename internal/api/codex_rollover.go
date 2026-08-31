package api

import (
	"context"
	"strings"

	"codex-account-pool/internal/refusaldetect"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

// observeCodexTerminalRollover is deliberately post-terminal and side-effect
// bounded: it only latches metadata for the following turn. It never changes the
// response bytes, status, headers, transport attempts, or retry decision.
func (s *Server) observeCodexTerminalRollover(ctx context.Context, mapping *codexSessionMapping, lease scheduler.Lease, model string, isChat bool, terminal []byte, checkpointRef string) refusaldetect.Decision {
	if mapping == nil || !mapping.enabled || isChat {
		return refusaldetect.Decision{Kind: refusaldetect.KindNone, Reason: "surface_ineligible", Version: refusaldetect.Version}
	}
	provider := strings.ToLower(strings.TrimSpace(lease.Account.Provider))
	if provider == "" {
		provider = "codex"
	}
	surface := ResolveInferenceSurface(provider, model, "responses", true)
	if !surface.Eligible {
		return refusaldetect.Decision{Kind: refusaldetect.KindNone, Reason: surface.RejectReason, Version: refusaldetect.Version}
	}
	decision := refusaldetect.DetectResponseCompleted(terminal)
	checkpointRef = strings.TrimSpace(checkpointRef)
	wantsRollover := false
	cause := ""
	switch {
	case decision.Kind == refusaldetect.KindHighConfidence && decision.Reason == refusaldetect.ReasonProtocolSafety:
		if mapping.rotateUpstreamSessionOnSafety && mapping.rolloverRuntimeEnabled {
			wantsRollover = true
			cause = "protocol_safety_buffering"
		}
	case decision.Kind == refusaldetect.KindHighConfidence:
		if mapping.rolloverTextRefusalEnabled && mapping.rolloverRuntimeEnabled {
			wantsRollover = true
			cause = "text_high_confidence_refusal"
		}
	}
	if wantsRollover {
		if checkpointRef == "" {
			// A terminal is already downstream-visible at this point. A missing
			// durable replay is therefore a safe suppression, never a fallback to
			// mutating the old upstream chain.
			s.enqueueAudit(storage.AuditLogRow{Action: "rollover_suppressed_checkpoint_unavailable", State: "suppressed", Reason: cause, Detail: "detector=" + decision.Version + ";family=" + surface.Family})
		} else if cause == "protocol_safety_buffering" {
			mapping.noteSafetyBuffering(checkpointRef)
		} else {
			mapping.noteTextRefusal(decision.Version, checkpointRef)
		}
	}
	// Audit is intentionally closed-set metadata only: no terminal body, match
	// phrase, HMAC, raw model, or raw upstream identifier is recorded.
	if decision.Kind == refusaldetect.KindHighConfidence || decision.Kind == refusaldetect.KindAmbiguous {
		s.enqueueAudit(storage.AuditLogRow{
			Action: "codex_rollover_terminal_observed", State: decision.Kind,
			Reason: decision.Reason, Detail: "detector=" + decision.Version + ";family=" + surface.Family,
		})
	}
	return decision
}
