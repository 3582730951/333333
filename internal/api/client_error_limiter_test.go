package api

import (
	"testing"
	"time"
)

func TestClientErrorLimiterWindowAndClientCap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := newClientErrorLimiter(2, time.Second, 2)

	if !limiter.allow("a", now) || !limiter.allow("a", now.Add(100*time.Millisecond)) {
		t.Fatal("first two reports should be allowed")
	}
	if limiter.allow("a", now.Add(200*time.Millisecond)) {
		t.Fatal("third report in the same window should be limited")
	}
	if !limiter.allow("a", now.Add(time.Second)) {
		t.Fatal("next window should allow reports again")
	}

	if !limiter.allow("b", now) || !limiter.allow("c", now) {
		t.Fatal("new clients should be allowed")
	}
	if got := len(limiter.hits); got > 2 {
		t.Fatalf("tracked clients = %d, want <= 2", got)
	}
}

func TestClientErrorLimiterLogsLimitedOncePerWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := newClientErrorLimiter(1, time.Second, 8)

	if allowed, logLimited := limiter.allowWithLimitLog("client", now); !allowed || logLimited {
		t.Fatalf("first report allowed=%v logLimited=%v, want allowed without limited log", allowed, logLimited)
	}
	if allowed, logLimited := limiter.allowWithLimitLog("client", now.Add(100*time.Millisecond)); allowed || !logLimited {
		t.Fatalf("first limited report allowed=%v logLimited=%v, want limited log once", allowed, logLimited)
	}
	if allowed, logLimited := limiter.allowWithLimitLog("client", now.Add(200*time.Millisecond)); allowed || logLimited {
		t.Fatalf("second limited report allowed=%v logLimited=%v, want suppressed limited log", allowed, logLimited)
	}
	if allowed, logLimited := limiter.allowWithLimitLog("client", now.Add(time.Second)); !allowed || logLimited {
		t.Fatalf("next window allowed=%v logLimited=%v, want reset window", allowed, logLimited)
	}
}
