package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
	release := make(chan struct{})
	var once sync.Once
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) == int64(count) {
			once.Do(func() { close(allArrived) })
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
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
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
	case <-allArrived:
	case <-time.After(timeout):
		close(release)
		t.Fatalf("only %d/%d streams reached upstream within %s", arrived.Load(), count, timeout)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
