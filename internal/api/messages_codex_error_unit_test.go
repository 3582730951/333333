package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesErrorToAnthropicEnvelopeMapsStatusToType(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantType string
		wantMsg  string
		wantCode string
	}{
		{
			name: "openai shape becomes anthropic envelope", status: 400,
			body:     `{"error":{"message":"unsupported parameter","type":"invalid_request_error","code":"unsupported_parameter"}}`,
			wantType: "invalid_request_error", wantMsg: "unsupported parameter", wantCode: "unsupported_parameter",
		},
		{
			name: "rate limit maps from status", status: 429,
			body:     `{"error":{"message":"slow down","type":"server_error"}}`,
			wantType: "rate_limit_error", wantMsg: "slow down",
		},
		{
			name: "overloaded", status: 529,
			body:     `{"error":{"message":"at capacity"}}`,
			wantType: "overloaded_error", wantMsg: "at capacity",
		},
		{
			name: "5xx becomes api_error", status: 502,
			body:     `{"error":{"message":"bad gateway"}}`,
			wantType: "api_error", wantMsg: "bad gateway",
		},
		{
			name: "auth", status: 401,
			body:     `{"error":{"message":"no key"}}`,
			wantType: "authentication_error", wantMsg: "no key",
		},
		{
			name: "bare detail field", status: 400,
			body:     `{"detail":"malformed input"}`,
			wantType: "invalid_request_error", wantMsg: "malformed input",
		},
		{
			name: "empty body falls back to safe text", status: 400,
			body:     ``,
			wantType: "invalid_request_error",
		},
		{
			name: "plain text upstream failure", status: 502,
			body:     `upstream connection reset`,
			wantType: "api_error", wantMsg: "upstream connection reset",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := responsesErrorToAnthropicEnvelope(tc.status, []byte(tc.body))
			var got map[string]interface{}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, out)
			}
			if got["type"] != "error" {
				t.Fatalf("top-level type = %v, want error: %s", got["type"], out)
			}
			errObj, ok := got["error"].(map[string]interface{})
			if !ok {
				t.Fatalf("no error object: %s", out)
			}
			if errObj["type"] != tc.wantType {
				t.Fatalf("error.type = %v, want %s: %s", errObj["type"], tc.wantType, out)
			}
			if tc.wantMsg != "" && errObj["message"] != tc.wantMsg {
				t.Fatalf("error.message = %v, want %q: %s", errObj["message"], tc.wantMsg, out)
			}
			if message, _ := errObj["message"].(string); strings.TrimSpace(message) == "" {
				t.Fatalf("error.message must never be empty: %s", out)
			}
			if tc.wantCode != "" && errObj["code"] != tc.wantCode {
				t.Fatalf("error.code = %v, want %s: %s", errObj["code"], tc.wantCode, out)
			}
		})
	}
}

// An already-Anthropic envelope must pass through untouched, so a path that emits one
// before the bridge is not double-wrapped into error.error.
func TestResponsesErrorToAnthropicEnvelopeIsIdempotent(t *testing.T) {
	original := `{"type":"error","error":{"type":"rate_limit_error","message":"Please retry."}}`
	out := responsesErrorToAnthropicEnvelope(429, []byte(original))
	if string(out) != original {
		t.Fatalf("already-enveloped body was rewritten:\n got %s\nwant %s", out, original)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	errObj, _ := got["error"].(map[string]interface{})
	if _, nested := errObj["error"]; nested {
		t.Fatalf("envelope was double-wrapped: %s", out)
	}
}

// The converter must not invent upstream detail. An empty body yields the fixed safe
// text for that status, never a fabricated reason.
func TestResponsesErrorToAnthropicEnvelopeDoesNotInventDetail(t *testing.T) {
	out := responsesErrorToAnthropicEnvelope(403, nil)
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	errObj, _ := got["error"].(map[string]interface{})
	safe, _ := safeClientError(403)
	if errObj["message"] != safe {
		t.Fatalf("message = %v, want the safe text %q", errObj["message"], safe)
	}
	if errObj["type"] != "permission_error" {
		t.Fatalf("type = %v, want permission_error", errObj["type"])
	}
}
