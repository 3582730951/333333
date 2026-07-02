package batch

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaybeBatchRecoversPanicAndReleasesWaiter(t *testing.T) {
	b := New(Config{Window: time.Millisecond, MaxBatchSize: 8})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	resp, ok := b.MaybeBatch(ctx, Request{Key: "panic"}, func(context.Context, []Request) ([]*Response, error) {
		panic("batch failed")
	})
	if ok || resp != nil {
		t.Fatalf("panic batch returned resp=%v ok=%v, want nil false", resp, ok)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("waiter timed out instead of being released: %v", err)
	}

	resp, ok = b.MaybeBatch(context.Background(), Request{Key: "panic"}, func(context.Context, []Request) ([]*Response, error) {
		return []*Response{{StatusCode: 200, Body: []byte("ok")}}, nil
	})
	if !ok || resp == nil || resp.StatusCode != 200 || string(resp.Body) != "ok" {
		t.Fatalf("batcher did not recover for next request: resp=%#v ok=%v", resp, ok)
	}
}

func TestMaybeBatchStartsNewGroupWhenCurrentGroupIsFull(t *testing.T) {
	b := New(Config{
		Window:              100 * time.Millisecond,
		MaxBatchSize:        1,
		EnableDynamicWindow: false,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var calls atomic.Int32
	fn := func(ctx context.Context, reqs []Request) ([]*Response, error) {
		calls.Add(1)
		responses := make([]*Response, len(reqs))
		for i, req := range reqs {
			responses[i] = &Response{StatusCode: 200, Body: append([]byte(nil), req.Payload...)}
		}
		return responses, nil
	}

	firstDone := make(chan *Response, 1)
	go func() {
		resp, ok := b.MaybeBatch(ctx, Request{Key: "full", Payload: []byte("first")}, fn)
		if !ok {
			firstDone <- nil
			return
		}
		firstDone <- resp
	}()
	waitForPendingSize(t, b, "full", 1)

	secondResp, ok := b.MaybeBatch(ctx, Request{Key: "full", Payload: []byte("second")}, fn)
	if !ok || secondResp == nil || string(secondResp.Body) != "second" {
		t.Fatalf("second request resp=%#v ok=%v, want response from a new group", secondResp, ok)
	}

	select {
	case firstResp := <-firstDone:
		if firstResp == nil || string(firstResp.Body) != "first" {
			t.Fatalf("first request response = %#v", firstResp)
		}
	case <-ctx.Done():
		t.Fatalf("first request timed out: %v", ctx.Err())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("batch calls = %d, want 2", got)
	}
}

func TestMaybeBatchDoesNotLeaveEmptyPendingGroupAfterSeal(t *testing.T) {
	b := New(Config{
		Window:              time.Millisecond,
		MaxBatchSize:        4,
		EnableDynamicWindow: false,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	resp, ok := b.MaybeBatch(ctx, Request{Key: "single", Payload: []byte("ok")}, func(ctx context.Context, reqs []Request) ([]*Response, error) {
		return []*Response{{StatusCode: 200, Body: []byte("ok")}}, nil
	})
	if !ok || resp == nil {
		t.Fatalf("single request resp=%#v ok=%v", resp, ok)
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	if _, ok := b.pending["single"]; ok {
		t.Fatal("sealed batch group remained in pending map")
	}
}

func TestStatsReturnsPlainSnapshot(t *testing.T) {
	b := New(Config{Window: time.Millisecond, MaxBatchSize: 4})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	resp, ok := b.MaybeBatch(ctx, Request{Key: "stats", Payload: []byte("ok")}, func(ctx context.Context, reqs []Request) ([]*Response, error) {
		return []*Response{{StatusCode: 200, Body: []byte("ok")}}, nil
	})
	if !ok || resp == nil {
		t.Fatalf("request resp=%#v ok=%v, want success", resp, ok)
	}

	stats := b.Stats()
	if stats.Total != 1 || stats.Batched != 1 || stats.Missed != 0 {
		t.Fatalf("stats = %+v, want total=1 batched=1 missed=0", stats)
	}
}

func TestMaybeBatchPreservesResponseOrderWithConcurrentOverflow(t *testing.T) {
	b := New(Config{
		Window:              50 * time.Millisecond,
		MaxBatchSize:        2,
		EnableDynamicWindow: false,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	fn := func(ctx context.Context, reqs []Request) ([]*Response, error) {
		responses := make([]*Response, len(reqs))
		for i, req := range reqs {
			responses[i] = &Response{StatusCode: 200, Body: append([]byte(nil), req.Payload...)}
		}
		return responses, nil
	}

	const n = 5
	start := make(chan struct{})
	results := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, ok := b.MaybeBatch(ctx, Request{Key: "burst", Payload: []byte(fmt.Sprintf("req-%d", i))}, fn)
			if !ok || resp == nil {
				t.Errorf("request %d did not receive a response: resp=%#v ok=%v", i, resp, ok)
				return
			}
			results[i] = string(resp.Body)
		}()
	}
	close(start)
	wg.Wait()

	for i, got := range results {
		want := fmt.Sprintf("req-%d", i)
		if got != want {
			t.Fatalf("response[%d] = %q, want %q; all=%v", i, got, want, results)
		}
	}
}

func waitForPendingSize(t *testing.T, b *Batcher, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		b.mu.RLock()
		g := b.pending[key]
		b.mu.RUnlock()
		if g != nil {
			g.mu.Lock()
			got := g.size
			g.mu.Unlock()
			if got == want {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending group %q did not reach size %d", key, want)
}
