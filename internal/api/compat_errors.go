package api

import (
	"net/http"
	"strings"

	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/storage"
)

const recentIncompatibilityLimit = 50

type compatIncompatibilityRecord struct {
	RequestedCapability string `json:"requested_capability"`
	ChosenRoute         string `json:"chosen_route"`
	FailureReason       string `json:"failure_reason"`
	FixHint             string `json:"fix_hint"`
	CreatedAt           int64  `json:"created_at"`
}

func writeCapabilityUnavailable(w http.ResponseWriter, status int, message string, requested []string, requiredTier, currentRoute, fixHint string) {
	if len(requested) == 0 {
		requested = []string{"unknown"}
	}
	errorBody := map[string]interface{}{
		"message":                message,
		"type":                   "capability_unavailable",
		"requested_capabilities": requested,
		"required_tier":          requiredTier,
		"current_route":          currentRoute,
		"fix_hint":               fixHint,
	}
	if requestID := strings.TrimSpace(w.Header().Get(requestIDHeader)); requestID != "" {
		errorBody["request_id"] = requestID
	}
	writeJSON(w, status, map[string]interface{}{"error": errorBody})
}

func (s *Server) writeCapabilityUnavailable(w http.ResponseWriter, status int, message string, requested []string, requiredTier, currentRoute, fixHint string) {
	s.recordCompatibilityIncompatibility(requested, currentRoute, message, fixHint)
	writeCapabilityUnavailable(w, status, message, requested, requiredTier, currentRoute, fixHint)
}

func (s *Server) writePromptCompatibilityError(w http.ResponseWriter, err error, requiredTier, currentRoute, fixHint string) bool {
	ce, ok := prompt.AsCompatibilityError(err)
	if !ok {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(ce.Protocol))
	kind := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(ce.Kind)), " ", "_")
	value := strings.TrimSpace(ce.Value)
	if name == "" {
		name = "request"
	}
	if kind == "" {
		kind = "capability"
	}
	requested := name + "_" + kind
	if value != "" {
		requested += ":" + value
	}
	s.writeCapabilityUnavailable(w, http.StatusBadRequest, ce.Error(), []string{requested}, requiredTier, currentRoute, fixHint)
	return true
}

func (s *Server) recordCompatibilityIncompatibility(requested []string, currentRoute, failureReason, fixHint string) {
	if s == nil {
		return
	}
	capability := strings.Join(requested, ",")
	if strings.TrimSpace(capability) == "" {
		capability = "unknown"
	}
	rec := compatIncompatibilityRecord{
		RequestedCapability: capability,
		ChosenRoute:         strings.TrimSpace(currentRoute),
		FailureReason:       strings.TrimSpace(failureReason),
		FixHint:             strings.TrimSpace(fixHint),
		CreatedAt:           storage.Now(),
	}
	s.compatMu.Lock()
	defer s.compatMu.Unlock()
	s.compatRecent = append(s.compatRecent, rec)
	if over := len(s.compatRecent) - recentIncompatibilityLimit; over > 0 {
		copy(s.compatRecent, s.compatRecent[over:])
		s.compatRecent = s.compatRecent[:recentIncompatibilityLimit]
	}
}

func (s *Server) recentCompatibilityIncompatibilities() []compatIncompatibilityRecord {
	if s == nil {
		return nil
	}
	s.compatMu.Lock()
	defer s.compatMu.Unlock()
	out := make([]compatIncompatibilityRecord, len(s.compatRecent))
	copy(out, s.compatRecent)
	return out
}
