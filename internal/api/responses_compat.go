package api

import (
	"encoding/json"
	"net/http"
)

const responsesCompatibilityLossesHeader = "X-Pool-Compatibility-Losses"

func responsesCompatibilityLossesJSON(losses []string) (string, []string) {
	merged := mergeCompatibilityLosses(losses)
	raw, _ := json.Marshal(merged)
	return string(raw), merged
}

func responsesCompatibilityLossesValue(losses []string) string {
	raw, merged := responsesCompatibilityLossesJSON(losses)
	if len(merged) == 0 {
		return "none"
	}
	return raw
}

func withResponsesCompatibilityLosses(r *http.Request, losses []string) *http.Request {
	raw, _ := responsesCompatibilityLossesJSON(losses)
	diagnostics := usageDiagnosticsFromCtx(r.Context())
	diagnostics.CompatibilityLossesJSON = raw
	return r.WithContext(withUsageDiagnostics(r.Context(), diagnostics))
}

func setResponsesCompatibilityHeader(w http.ResponseWriter, losses []string) {
	w.Header().Set(responsesCompatibilityLossesHeader, responsesCompatibilityLossesValue(losses))
}

func declareResponsesCompatibilityTrailer(w http.ResponseWriter) {
	w.Header().Add("Trailer", responsesCompatibilityLossesHeader)
}

func setResponsesCompatibilityTrailer(w http.ResponseWriter, losses []string) {
	w.Header().Set(http.TrailerPrefix+responsesCompatibilityLossesHeader, responsesCompatibilityLossesValue(losses))
}
