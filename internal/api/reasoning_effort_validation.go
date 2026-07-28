package api

import (
	"fmt"
	"net/http"
	"strings"

	"codex-account-pool/internal/capability"
	"github.com/tidwall/gjson"
)

// requestedResponsesReasoningEffort reads only the small reasoning leaf. It is
// deliberately used before scheduling so an impossible local Codex setting cannot
// acquire an account lease, mutate tool/session state, or reach an upstream.
func requestedResponsesReasoningEffort(raw []byte) string {
	effort := gjson.GetBytes(raw, "reasoning.effort")
	if effort.Type != gjson.String {
		return ""
	}
	return normalizeEffort(effort.String())
}

func unsupportedCodexReasoningEffort(model, effort string) bool {
	effort = normalizeEffort(effort)
	if effort != "ultra" {
		return false
	}
	return !capability.CodexSupportsReasoningEffort(model, effort)
}

func writeCodexReasoningEffortUnsupported(w http.ResponseWriter, model, effort string) {
	writeJSON(w, http.StatusBadRequest, map[string]interface{}{
		"error": map[string]interface{}{
			"message": fmt.Sprintf("reasoning effort %q is unsupported for model %q", strings.TrimSpace(effort), strings.TrimSpace(model)),
			"type":    "invalid_request_error",
			"param":   "reasoning.effort",
			"code":    "reasoning_effort_unsupported",
		},
	})
}
