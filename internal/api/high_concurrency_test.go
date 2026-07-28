package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayDirectAllowsTwoHundredConcurrentStreams(t *testing.T) {
	testGatewayDirectConcurrentStreams(t, 200, 5*time.Second)
}

func TestGatewayDirectAllowsOneThousandConcurrentStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	testGatewayDirectConcurrentStreams(t, 1000, 20*time.Second)
}

func testGatewayDirectConcurrentStreams(t *testing.T, count int, timeout time.Duration) {
	t.Helper()
	var arrived atomic.Int64
	allArrived := make(chan struct{})
	capacityArrived := make(chan struct{})
	release := make(chan struct{})
	var allOnce, capacityOnce, releaseOnce sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer releaseOnce.Do(func() { close(release) })
	capacity := count
	if capacity > 512 {
		capacity = 512
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		n := arrived.Add(1)
		if n == int64(capacity) {
			capacityOnce.Do(func() { close(capacityArrived) })
		}
		if n == int64(count) {
			allOnce.Do(func() { close(allArrived) })
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"load\",\"model\":\"gpt\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\ndata: [DONE]\n\n")
	})
	h.importAccount(t, "load", "load-upstream", "load-token")
	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 0, MaxIdleConns: count, MaxIdleConnsPerHost: count}}
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"gpt","stream":true,"input":"load-%d"}`, i)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("status %d", resp.StatusCode)
				return
			}
			_, err = io.Copy(io.Discard, resp.Body)
			if err != nil {
				errs <- err
			}
		}(i)
	}
	select {
	case <-capacityArrived:
	case <-time.After(timeout):
		releaseOnce.Do(func() { close(release) })
		writeConcurrentGoroutineProfile()
		t.Fatalf("only %d/%d concurrent streams reached upstream within %s; early errors=%v", arrived.Load(), capacity, timeout, drainConcurrentErrors(errs, 10))
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-allArrived:
	case <-time.After(timeout):
		writeConcurrentGoroutineProfile()
		t.Fatalf("only %d/%d queued streams reached upstream within %s after capacity released; early errors=%v", arrived.Load(), count, timeout, drainConcurrentErrors(errs, 10))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func writeConcurrentGoroutineProfile() {
	if profile := pprof.Lookup("goroutine"); profile != nil {
		_ = profile.WriteTo(os.Stderr, 1)
	}
}

func drainConcurrentErrors(errs <-chan error, limit int) []error {
	out := make([]error, 0, limit)
	for len(out) < limit {
		select {
		case err := <-errs:
			if err != nil {
				out = append(out, err)
			}
		default:
			return out
		}
	}
	return out
}
