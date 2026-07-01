package api

import (
	"context"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/supervisor"
)

// asyncWriteBudgetBytes bounds the total approximate payload bytes queued for async
// writing. Beyond it, a sized write (e.g. a large virtual-ledger body) runs inline
// instead, so a stalled drainer can never pin unbounded memory holding request bodies.
const asyncWriteBudgetBytes = 64 << 20

// asyncWriteQueueDepth is the channel capacity. Small fire-and-forget writes (usage
// rows) dominate; a full queue falls back to an inline write (backpressure), never a
// drop.
const asyncWriteQueueDepth = 2048

// startAsyncWriter launches the single FIFO drainer. Called once from NewServer so the
// queue is live before any request is served. The drainer runs until FlushWrites closes
// the channel during shutdown (after in-flight requests have drained), then exits.
func (s *Server) startAsyncWriter() {
	s.asyncWrites = make(chan func(), asyncWriteQueueDepth)
	s.asyncWG.Add(1)
	supervisor.GoOnce("async-write-drainer", func() {
		defer s.asyncWG.Done()
		for fn := range s.asyncWrites {
			runAsyncWrite(fn)
		}
	})
}

func runAsyncWrite(fn func()) {
	defer supervisor.Recover("async-write")
	fn()
}

// enqueueWrite schedules a fire-and-forget DB write off the request path. Use it only
// for writes whose result the caller ignores (usage/ledger rows). The closure MUST use
// its own context — bgWriteContext — because the request context is cancelled the moment
// the request returns, which would otherwise abort the deferred write.
func (s *Server) enqueueWrite(fn func()) {
	s.enqueueWriteSized(0, fn)
}

// enqueueWriteSized is enqueueWrite with an approximate payload size charged against the
// byte budget. When the budget is exceeded, or the queue is full or already closed, the
// write runs inline so nothing is ever dropped and memory stays bounded.
func (s *Server) enqueueWriteSized(approxBytes int, fn func()) {
	run := fn
	if approxBytes > 0 {
		if atomic.AddInt64(&s.asyncBytes, int64(approxBytes)) > asyncWriteBudgetBytes {
			atomic.AddInt64(&s.asyncBytes, -int64(approxBytes))
			runAsyncWrite(fn)
			return
		}
		run = func() {
			defer atomic.AddInt64(&s.asyncBytes, -int64(approxBytes))
			fn()
		}
	}
	// RLock spans the send so it cannot race with FlushWrites' close (which takes the
	// write lock first). Many enqueuers hold the RLock concurrently — channel sends are
	// safe to do in parallel.
	s.asyncMu.RLock()
	if s.asyncClosed {
		s.asyncMu.RUnlock()
		runAsyncWrite(run)
		return
	}
	select {
	case s.asyncWrites <- run:
		s.asyncMu.RUnlock()
	default:
		s.asyncMu.RUnlock()
		runAsyncWrite(run) // queue full -> inline (backpressure)
	}
}

// FlushWrites stops accepting new async writes and blocks until the queue has drained.
// Call it during shutdown AFTER the HTTP server has drained in-flight requests (so no
// further writes are enqueued) and BEFORE the store is closed. Idempotent.
func (s *Server) FlushWrites() {
	s.asyncMu.Lock()
	if s.asyncClosed {
		s.asyncMu.Unlock()
		return
	}
	s.asyncClosed = true
	close(s.asyncWrites)
	s.asyncMu.Unlock()
	s.asyncWG.Wait()
}

// bgWriteContext returns a detached context with a generous timeout for an async write;
// the originating request's context is already gone by the time the drainer runs it.
func bgWriteContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// WaitForAsyncWrites blocks until every write enqueued before this call has been
// processed. The drainer is FIFO, so enqueuing a sentinel and awaiting it guarantees all
// earlier queued writes have run. Unlike FlushWrites it does not stop the writer, so it
// is safe to call repeatedly — used by tests that assert on rows a request just enqueued.
func (s *Server) WaitForAsyncWrites() {
	done := make(chan struct{})
	s.enqueueWrite(func() { close(done) })
	<-done
}
