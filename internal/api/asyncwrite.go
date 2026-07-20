package api

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/storage"
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

type telemetryWrite struct {
	usage      *storage.UsageRecordWrite
	hold       *storage.BillingHoldWrite
	apiKeyHash string
	usedAt     int64
	audit      *storage.AuditLogRow
	barrier    chan error
}

// startAsyncWriter launches the single FIFO drainer. Called once from NewServer so the
// queue is live before any request is served. The drainer runs until FlushWrites closes
// the channel during shutdown (after in-flight requests have drained), then exits.
func (s *Server) startAsyncWriter() {
	s.asyncWrites = make(chan func(), asyncWriteQueueDepth)
	s.usageWrites = make(chan telemetryWrite, asyncWriteQueueDepth)
	s.asyncWG.Add(1)
	supervisor.GoOnce("async-write-drainer", func() {
		defer s.asyncWG.Done()
		for {
			fn, ok := <-s.asyncWrites
			if !ok {
				return
			}
			batch := []func(){fn}
			timer := time.NewTimer(5 * time.Millisecond)
		collect:
			for len(batch) < 64 {
				select {
				case next, open := <-s.asyncWrites:
					if !open {
						for _, f := range batch {
							runAsyncWrite(f)
						}
						timer.Stop()
						return
					}
					batch = append(batch, next)
				case <-timer.C:
					break collect
				}
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			for _, f := range batch {
				runAsyncWrite(f)
			}
		}
	})
	s.asyncWG.Add(1)
	supervisor.GoOnce("usage-batch-writer", func() {
		defer s.asyncWG.Done()
		for first := range s.usageWrites {
			if first.barrier != nil {
				first.barrier <- nil
				s.usagePending.Done()
				continue
			}
			batch := []telemetryWrite{first}
			var barrier chan error
			timer := time.NewTimer(5 * time.Millisecond)
		collect:
			for len(batch) < 64 {
				select {
				case next, ok := <-s.usageWrites:
					if !ok {
						break collect
					}
					if next.barrier != nil {
						barrier = next.barrier
						break collect
					}
					batch = append(batch, next)
				case <-timer.C:
					break collect
				}
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			usageBatch := make([]storage.UsageRecordWrite, 0, len(batch))
			holds := make([]storage.BillingHoldWrite, 0, len(batch))
			apiKeys := make(map[string]int64)
			audits := make([]storage.AuditLogRow, 0, len(batch))
			for _, write := range batch {
				if write.usage != nil {
					usageBatch = append(usageBatch, *write.usage)
				}
				if write.apiKeyHash != "" && write.usedAt > apiKeys[write.apiKeyHash] {
					apiKeys[write.apiKeyHash] = write.usedAt
				}
				if write.hold != nil {
					holds = append(holds, *write.hold)
				}
				if write.audit != nil {
					audits = append(audits, *write.audit)
				}
			}
			ctx, cancel := bgWriteContext()
			err := s.store.BatchWriteTelemetry(ctx, usageBatch, apiKeys, holds, audits)
			cancel()
			if err != nil {
				log.Printf("[USAGE-ERROR] batch insert count=%d: %v", len(batch), err)
			}
			for range batch {
				s.usagePending.Done()
			}
			if barrier != nil {
				barrier <- err
				s.usagePending.Done()
			}
		}
	})
}

// flushTelemetry inserts a FIFO boundary without waiting for requests that arrive
// after it. Every usage/hold/audit queued before the boundary is committed before
// this returns. A bounded caller context prevents dashboard reads from stalling.
func (s *Server) flushTelemetry(ctx context.Context) (timedOut bool) {
	done := make(chan error, 1)
	s.usagePending.Add(1)
	s.asyncMu.RLock()
	if s.asyncClosed {
		s.asyncMu.RUnlock()
		s.usagePending.Done()
		return false
	}
	select {
	case s.usageWrites <- telemetryWrite{barrier: done}:
		s.asyncMu.RUnlock()
	case <-ctx.Done():
		s.asyncMu.RUnlock()
		s.usagePending.Done()
		return true
	}
	select {
	case <-done:
		return false
	case <-ctx.Done():
		return true
	}
}

func (s *Server) enqueueUsage(write storage.UsageRecordWrite) {
	s.enqueueTelemetry(telemetryWrite{usage: &write})
}

func (s *Server) enqueueAPIKeyUsed(keyHash string, usedAt int64) {
	s.enqueueTelemetry(telemetryWrite{apiKeyHash: keyHash, usedAt: usedAt})
}

func (s *Server) enqueueBillingHold(write storage.BillingHoldWrite) {
	s.enqueueTelemetry(telemetryWrite{hold: &write})
}

func (s *Server) enqueueAudit(row storage.AuditLogRow) {
	s.enqueueTelemetry(telemetryWrite{audit: &row})
}

func (s *Server) enqueueTelemetry(write telemetryWrite) {
	s.usagePending.Add(1)
	s.asyncMu.RLock()
	if !s.asyncClosed {
		select {
		case s.usageWrites <- write:
			s.asyncMu.RUnlock()
			return
		default:
		}
	}
	s.asyncMu.RUnlock()
	ctx, cancel := bgWriteContext()
	var usage []storage.UsageRecordWrite
	if write.usage != nil {
		usage = append(usage, *write.usage)
	}
	keys := map[string]int64{}
	if write.apiKeyHash != "" {
		keys[write.apiKeyHash] = write.usedAt
	}
	var holds []storage.BillingHoldWrite
	if write.hold != nil {
		holds = append(holds, *write.hold)
	}
	var audits []storage.AuditLogRow
	if write.audit != nil {
		audits = append(audits, *write.audit)
	}
	err := s.store.BatchWriteTelemetry(ctx, usage, keys, holds, audits)
	cancel()
	if err != nil {
		log.Printf("[USAGE-ERROR] inline insert: %v", err)
	}
	s.usagePending.Done()
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
	close(s.usageWrites)
	s.asyncMu.Unlock()
	s.asyncWG.Wait()
	if s.scheduler != nil {
		s.scheduler.Close()
	}
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
	s.usagePending.Wait()
}
