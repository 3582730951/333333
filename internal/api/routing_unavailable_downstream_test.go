package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeRoutingError(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var payload struct {
		Error map[string]interface{} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("response is not a JSON error envelope: %v\nbody=%s", err, body)
	}
	if payload.Error == nil {
		t.Fatalf("no error object in response: %s", body)
	}
	return payload.Error
}

// The scheduler already knows why routing failed; the reason was simply dropped on
// the way to the client, so every failure answered "Please retry." with type
// server_error. Claude Code and Codex CLI both retry that shape, which turns a
// permanent misconfiguration into an unbounded retry loop.
func TestRoutingUnavailableTellsTheClientWhetherRetryingCanHelp(t *testing.T) {
	for _, tc := range []struct {
		name           string
		reason         string
		status         int
		wantStatus     int
		wantType       string
		wantCode       string
		wantRetryable  bool
		messageMustSay string
	}{
		{
			name:   "unservable model is a request error, not saturation",
			reason: "model_unsupported", status: http.StatusServiceUnavailable,
			wantStatus: http.StatusBadRequest, wantType: "invalid_request_error",
			wantCode: "model_unsupported", wantRetryable: false,
			messageMustSay: "not available",
		},
		{
			name:   "empty group needs an operator, not a retry",
			reason: "group_has_no_accounts", status: http.StatusServiceUnavailable,
			wantStatus: http.StatusServiceUnavailable, wantType: "server_error",
			wantCode: "no_accounts_configured", wantRetryable: false,
			messageMustSay: "operator action",
		},
		{
			name:   "genuine saturation still advertises a retry",
			reason: "no_public_account_detail", status: http.StatusTooManyRequests,
			wantStatus: http.StatusTooManyRequests, wantType: "server_error",
			wantCode: "capacity_saturated", wantRetryable: true,
			messageMustSay: "retry",
		},
		{
			name:   "sticky route conflict still advertises a retry",
			reason: "no_public_account_detail", status: http.StatusConflict,
			wantStatus: http.StatusConflict, wantType: "server_error",
			wantCode: "sticky_route_unavailable", wantRetryable: true,
			messageMustSay: "retry",
		},
		{
			name:   "unclassified failure keeps the generic shape",
			reason: "", status: http.StatusServiceUnavailable,
			wantStatus: http.StatusServiceUnavailable, wantType: "server_error",
			wantCode: "service_unavailable", wantRetryable: true,
			messageMustSay: "retry",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			// The caller sets Retry-After from noAccountHTTPStatus before the reason is
			// known, so a non-retryable reason has to clear it.
			rec.Header().Set("Retry-After", "3")

			writePublicRoutingUnavailable(rec, tc.status, tc.reason)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			errBody := decodeRoutingError(t, rec.Body.String())
			if errBody["type"] != tc.wantType {
				t.Errorf("type = %v, want %q", errBody["type"], tc.wantType)
			}
			if errBody["code"] != tc.wantCode {
				t.Errorf("code = %v, want %q", errBody["code"], tc.wantCode)
			}
			message, _ := errBody["message"].(string)
			if !strings.Contains(strings.ToLower(message), tc.messageMustSay) {
				t.Errorf("message %q does not mention %q", message, tc.messageMustSay)
			}
			if got := rec.Header().Get("Retry-After"); tc.wantRetryable != (got != "") {
				t.Errorf("Retry-After = %q, but retryable = %v: a delay must not be advertised "+
					"for a condition that retrying cannot clear", got, tc.wantRetryable)
			}
		})
	}
}

// The audit row carries the diagnosable detail. The response must not: a downstream
// caller learns the shape of the failure, never the pool's topology.
func TestRoutingUnavailableResponseDoesNotLeakPoolInternals(t *testing.T) {
	// The fields the audit row records, which must never cross to the client.
	internals := []string{
		"group_hash", "counters", "allowed_providers", "kiro_accounts",
		"normalized_model", "requested_model", "modelunsupported",
	}
	allowedCodes := map[string]bool{
		"model_unsupported": true, "no_accounts_configured": true,
		"capacity_saturated": true, "sticky_route_unavailable": true,
		"service_unavailable": true,
	}

	for _, reason := range []string{"model_unsupported", "group_has_no_accounts", "no_public_account_detail", ""} {
		rec := httptest.NewRecorder()
		writePublicRoutingUnavailable(rec, http.StatusServiceUnavailable, reason)

		body := strings.ToLower(rec.Body.String())
		for _, leak := range internals {
			if strings.Contains(body, leak) {
				t.Errorf("reason %q leaked %q into the downstream response: %s", reason, leak, body)
			}
		}

		// The error object is a closed shape, so a future field cannot quietly start
		// carrying internals.
		errBody := decodeRoutingError(t, rec.Body.String())
		for key := range errBody {
			switch key {
			case "message", "type", "code", "request_id":
			default:
				t.Errorf("reason %q added unexpected error field %q: %+v", reason, key, errBody)
			}
		}
		code, _ := errBody["code"].(string)
		if !allowedCodes[code] {
			t.Errorf("reason %q produced code %q, which is outside the published vocabulary",
				reason, code)
		}
	}
}

// writePublicUnavailable is still used by six other call sites that have no routing
// reason; they must keep behaving exactly as before.
func TestWritePublicUnavailableKeepsItsGenericContract(t *testing.T) {
	rec := httptest.NewRecorder()
	writePublicUnavailable(rec, http.StatusServiceUnavailable)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
	errBody := decodeRoutingError(t, rec.Body.String())
	if errBody["type"] != "server_error" || errBody["code"] != "service_unavailable" {
		t.Errorf("generic shape changed: %+v", errBody)
	}
}
