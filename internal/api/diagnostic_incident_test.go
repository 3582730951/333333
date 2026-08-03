package api

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/incident"
	"codex-account-pool/internal/supervisor"
)

func TestTriggeredPanicAndHandled500PersistIntoCorrelatedDiagnosticZip(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	reporter, err := incident.Open(filepath.Join(t.TempDir(), "exception-events"), h.store)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := supervisor.RegisterEventCallback(reporter.CallbackOptions("api-diagnostic-trigger-test"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registration.Unregister)

	const rawPanic = "Bearer panic-secret-token@example.test"
	h.app.mux.HandleFunc("/__test/diagnostic-panic", func(http.ResponseWriter, *http.Request) {
		panic(rawPanic)
	})
	h.app.mux.HandleFunc("/__test/diagnostic-500", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "fixture failure")
	})

	panicResponse, err := h.pool.Client().Get(h.pool.URL + "/__test/diagnostic-panic")
	if err != nil {
		t.Fatal(err)
	}
	panicRequestID := panicResponse.Header.Get(requestIDHeader)
	_ = panicResponse.Body.Close()
	if panicResponse.StatusCode != http.StatusServiceUnavailable || !diagnosticPublicRequestIDRE.MatchString(panicRequestID) {
		t.Fatalf("panic response status=%d request_id=%q", panicResponse.StatusCode, panicRequestID)
	}

	errorResponse, err := h.pool.Client().Get(h.pool.URL + "/__test/diagnostic-500")
	if err != nil {
		t.Fatal(err)
	}
	errorRequestID := errorResponse.Header.Get(requestIDHeader)
	_ = errorResponse.Body.Close()
	if errorResponse.StatusCode != http.StatusInternalServerError || !diagnosticPublicRequestIDRE.MatchString(errorRequestID) {
		t.Fatalf("500 response status=%d request_id=%q", errorResponse.StatusCode, errorRequestID)
	}

	rows, err := h.store.DB().QueryContext(context.Background(), `
SELECT event_type,entity_alias,detail_json FROM diagnostic_events
WHERE entity_alias IN (?,?) ORDER BY created_at,id`, panicRequestID, errorRequestID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	persisted := map[string]string{}
	for rows.Next() {
		var eventType, requestID, detail string
		if err = rows.Scan(&eventType, &requestID, &detail); err != nil {
			t.Fatal(err)
		}
		persisted[eventType+":"+requestID] = detail
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	panicDetail := persisted["panic:"+panicRequestID]
	errorDetail := persisted["http_error:"+errorRequestID]
	if !strings.Contains(panicDetail, `"component":"http-request"`) || !strings.Contains(panicDetail, `"route":"test.diagnostic-panic"`) {
		t.Fatalf("panic detail=%s all=%v", panicDetail, persisted)
	}
	if !strings.Contains(errorDetail, `"error_class":"http_status_5xx"`) || !strings.Contains(errorDetail, `"route":"test.diagnostic-500"`) {
		t.Fatalf("500 detail=%s all=%v", errorDetail, persisted)
	}
	for _, detail := range []string{panicDetail, errorDetail} {
		if strings.Contains(detail, rawPanic) || strings.Contains(detail, "panic-secret") {
			t.Fatalf("raw panic leaked into durable detail: %s", detail)
		}
	}

	rawZip := awaitLegacyDiagnosticExport(t, h)
	if output := strings.TrimSpace(os.Getenv("CODEX_INCIDENT_DIAGNOSTIC_OUT")); output != "" {
		if err = os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(output, rawZip, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files := readZipFiles(t, rawZip)
	eventsCSV := files["diagnostic_events.csv"]
	httpCSV := files["http_requests.csv"]
	for _, expected := range []string{
		panicRequestID, errorRequestID, "panic", "http_error",
		"http-request", "test.diagnostic-panic", "test.diagnostic-500",
	} {
		if !strings.Contains(eventsCSV+httpCSV, expected) {
			t.Fatalf("diagnostic ZIP missing %q\n--- events ---\n%s\n--- http ---\n%s", expected, eventsCSV, httpCSV)
		}
	}
	if strings.Contains(eventsCSV, rawPanic) || strings.Contains(string(rawZip), rawPanic) {
		t.Fatal("raw panic leaked into diagnostic ZIP")
	}
	if !strings.Contains(httpCSV, panicRequestID+",GET,__test.diagnostic-panic,503") ||
		!strings.Contains(httpCSV, errorRequestID+",GET,__test.diagnostic-500,500") {
		t.Fatalf("HTTP correlation rows missing\n%s", httpCSV)
	}
}
