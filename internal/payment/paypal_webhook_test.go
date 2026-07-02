package payment

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/supervisor"
)

func TestPayPalWebhookWritesJSONErrors(t *testing.T) {
	handler := NewPayPalWebhookHandler(nil, "")

	rec := requestPayPalWebhook(t, handler, http.MethodGet, "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q", got)
	}
	assertPayPalWebhookError(t, rec, "method_not_allowed")

	rec = requestPayPalWebhook(t, handler, http.MethodPost, "{", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	assertPayPalWebhookError(t, rec, "invalid_json")
}

func TestPayPalWebhookLimitsRequestBody(t *testing.T) {
	handler := NewPayPalWebhookHandler(nil, "")
	body := strings.Repeat("x", maxPayPalWebhookBodyBytes+1)

	rec := requestPayPalWebhook(t, handler, http.MethodPost, body, "")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", rec.Code)
	}
	assertPayPalWebhookError(t, rec, "body_too_large")
}

func TestPayPalWebhookRejectsInvalidSignature(t *testing.T) {
	handler := NewPayPalWebhookHandler(nil, "secret")

	rec := requestPayPalWebhook(t, handler, http.MethodPost, `{"id":"evt_1"}`, "not-the-signature")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	assertPayPalWebhookError(t, rec, "invalid_signature")
}

func TestPayPalWebhookAcceptsProcessingFailureWithoutPanic(t *testing.T) {
	handler := NewPayPalWebhookHandler(nil, "")
	rec := requestPayPalWebhook(t, handler, http.MethodPost, `{
		"id": "evt_1",
		"event_type": "BILLING.SUBSCRIPTION.ACTIVATED",
		"resource": {
			"id": "sub_1",
			"status": "ACTIVE",
			"custom_id": "acct_1"
		}
	}`, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	assertPayPalWebhookStatus(t, rec, "ok")
}

func TestPayPalWebhookRejectsNilHandlerAsJSON(t *testing.T) {
	var handler *PayPalWebhookHandler
	rec := requestPayPalWebhook(t, handler, http.MethodPost, `{"id":"evt_1"}`, "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	assertPayPalWebhookError(t, rec, "handler_not_configured")
}

func TestPayPalWebhookRecoversPanicAsJSON(t *testing.T) {
	handler := NewPayPalWebhookHandler(nil, "")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/paypal", nil)
	req.Body = panicReadCloser{}
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	assertPayPalWebhookError(t, rec, "internal_error")

	events := supervisor.RecentEvents()
	if len(events) == 0 {
		t.Fatal("supervisor events are empty, want latest paypal webhook panic")
	}
	if events[0].Module != "paypal-webhook" || events[0].Type != "panic" ||
		!strings.Contains(events[0].Panic, "panic=read failed") {
		t.Fatalf("latest supervisor event = %#v, want paypal webhook panic", events[0])
	}
}

func TestPayPalWebhookSignatureVerification(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	handler := NewPayPalWebhookHandler(nil, "secret")

	if !handler.verifySignature(signPayPalWebhook("secret", body), "tx-1", "2026-06-30T00:00:00Z", body) {
		t.Fatal("expected valid signature")
	}
	if handler.verifySignature(signPayPalWebhook("other", body), "tx-1", "2026-06-30T00:00:00Z", body) {
		t.Fatal("expected invalid signature")
	}
}

func TestPaymentSourcesDoNotUseHTTPError(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Error" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "http" {
				return true
			}
			t.Errorf("%s uses plain-text http.Error; use a structured package response helper instead", fset.Position(sel.Pos()))
			return true
		})
	}
}

func requestPayPalWebhook(t *testing.T, handler http.Handler, method, body, signature string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/webhooks/paypal", strings.NewReader(body))
	if signature != "" {
		req.Header.Set("Paypal-Transmission-Sig", signature)
		req.Header.Set("Paypal-Transmission-Id", "tx-1")
		req.Header.Set("Paypal-Transmission-Time", "2026-06-30T00:00:00Z")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func assertPayPalWebhookError(t *testing.T, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	assertPayPalWebhookJSONHeaders(t, rec)
	var response payPalWebhookResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if response.Error == nil || response.Error.Code != code {
		t.Fatalf("error code = %#v, want %q", response.Error, code)
	}
}

func assertPayPalWebhookStatus(t *testing.T, rec *httptest.ResponseRecorder, status string) {
	t.Helper()
	assertPayPalWebhookJSONHeaders(t, rec)
	var response payPalWebhookResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if response.Status != status {
		t.Fatalf("status payload = %q, want %q", response.Status, status)
	}
}

func assertPayPalWebhookJSONHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func signPayPalWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

type panicReadCloser struct{}

func (panicReadCloser) Read([]byte) (int, error) {
	panic("read failed")
}

func (panicReadCloser) Close() error {
	return nil
}
