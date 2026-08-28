package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRecordHTTPRequestTimingSeparatesTTFBFromStreamLifetime(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	h.app.recordHTTPRequestTiming(
		"REQ-STREAM-TIMING", http.MethodGet, "v1.responses", http.StatusOK,
		17, 42, 15*time.Minute, 35*time.Millisecond, true,
	)
	rows := h.app.diagnosticHTTPRequests()
	if len(rows) == 0 {
		t.Fatal("missing HTTP diagnostic timing row")
	}
	row := rows[len(rows)-1]
	if row.DurationMS != 900000 || row.TTFBMS != 35 || row.StreamDurationMS != 899965 || !row.Streaming {
		t.Fatalf("timing row = %+v, want duration=900000 ttfb=35 stream=899965 streaming=true", row)
	}
	csvRows := httpRequestRows([]diagnosticHTTPRequest{row})
	if got := csvRows[0][6:10]; got[0] != "900000" || got[1] != "35" || got[2] != "899965" || got[3] != "true" {
		t.Fatalf("timing CSV fields = %v", got)
	}
}

func TestResponseRecorderCapturesFirstObservableSSEWrite(t *testing.T) {
	underlying := httptest.NewRecorder()
	recorder := &responseRecorder{ResponseWriter: underlying, status: http.StatusOK}
	if !recorder.firstResponseAt.IsZero() {
		t.Fatal("first response timestamp initialized before a write")
	}
	recorder.Header().Set("Content-Type", "text/event-stream")
	recorder.Flush()
	first := recorder.firstResponseAt
	if first.IsZero() || !recorder.wrote || recorder.status != http.StatusOK {
		t.Fatalf("flush did not commit first response: %+v", recorder)
	}
	time.Sleep(time.Millisecond)
	recorder.WriteHeader(http.StatusCreated)
	if !recorder.firstResponseAt.Equal(first) || recorder.status != http.StatusOK {
		t.Fatalf("later write changed first response timing/status: %+v", recorder)
	}
}

func TestRecordHTTPRequestTimingClampsInvalidDurations(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {})
	h.app.recordHTTPRequestTiming("REQ-CLAMP", http.MethodGet, "v1.responses", http.StatusOK, 0, 0, 10*time.Millisecond, time.Second, true)
	rows := h.app.diagnosticHTTPRequests()
	row := rows[len(rows)-1]
	if row.DurationMS != 10 || row.TTFBMS != 10 || row.StreamDurationMS != 0 {
		t.Fatalf("clamped timing row = %+v", row)
	}
}
