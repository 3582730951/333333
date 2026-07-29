package api

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/usagejournal"
)

// asyncWriteBudgetBytes bounds the total approximate payload bytes queued for async
// writing. Beyond it, a sized write (e.g. a large virtual-ledger body) runs inline
// instead, so a stalled drainer can never pin unbounded memory holding request bodies.
const asyncWriteBudgetBytes = 64 << 20

// asyncWriteQueueDepth is the channel capacity. Journaled telemetry that exceeds this
// bounded in-memory queue remains on disk for the replay worker; it never blocks the
// response path on a database write.
const asyncWriteQueueDepth = 2048

type telemetryWrite struct {
	usage      *storage.UsageRecordWrite
	hold       *storage.BillingHoldWrite
	apiKeyHash string
	usedAt     int64
	audit      *storage.AuditLogRow
	journalSeq uint64
	barrier    chan error
}

// startAsyncWriter launches the single FIFO drainer. Called once from NewServer so the
// queue is live before any request is served. The drainer runs until FlushWrites closes
// the channel during shutdown (after in-flight requests have drained), then exits.
func (s *Server) startAsyncWriter() {
	s.asyncWriteCtx, s.asyncWriteCancel = context.WithCancel(context.Background())
	s.asyncFlushDone = make(chan struct{})
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
				first.barrier <- s.replayUsageJournal(context.Background())
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
			err := s.persistTelemetryBatch(batch)
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
	if s.usageJournal != nil {
		s.asyncWG.Add(1)
		supervisor.GoOnce("usage-journal-replayer", func() {
			defer s.asyncWG.Done()
			delay := time.Second
			for {
				timer := time.NewTimer(delay)
				select {
				case <-s.usageJournalStop:
					timer.Stop()
					return
				case <-s.usageJournalWake:
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
				case <-timer.C:
				}
				if err := s.replayUsageJournal(context.Background()); err != nil {
					log.Printf("[USAGE-JOURNAL] replay failed: %v", err)
					delay *= 2
					if delay > 30*time.Second {
						delay = 30 * time.Second
					}
					continue
				}
				delay = time.Second
			}
		})
	}
}

func (s *Server) persistTelemetryBatch(batch []telemetryWrite) error {
	ctx, cancel := s.bgWriteContext()
	defer cancel()
	return s.persistTelemetryBatchContext(ctx, batch)
}

func (s *Server) persistTelemetryBatchDetached(batch []telemetryWrite) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.persistTelemetryBatchContext(ctx, batch)
}

func (s *Server) persistTelemetryBatchContext(ctx context.Context, batch []telemetryWrite) error {
	usageBatch := make([]storage.UsageRecordWrite, 0, len(batch))
	holds := make([]storage.BillingHoldWrite, 0, len(batch))
	apiKeys := make(map[string]int64)
	audits := make([]storage.AuditLogRow, 0, len(batch))
	journalSequences := make([]uint64, 0, len(batch))
	for _, write := range batch {
		if write.usage != nil {
			usageBatch = append(usageBatch, *write.usage)
		}
		if write.hold != nil {
			holds = append(holds, *write.hold)
		}
		if write.journalSeq > 0 {
			journalSequences = append(journalSequences, write.journalSeq)
		}
		if write.apiKeyHash != "" && write.usedAt > apiKeys[write.apiKeyHash] {
			apiKeys[write.apiKeyHash] = write.usedAt
		}
		if write.audit != nil {
			audits = append(audits, *write.audit)
		}
	}
	if len(usageBatch) == 0 && len(holds) == 0 && len(apiKeys) == 0 && len(audits) == 0 {
		return nil
	}
	directErr := s.store.BatchWriteTelemetry(ctx, usageBatch, apiKeys, holds, audits)
	if directErr != nil || len(journalSequences) == 0 {
		return directErr
	}
	s.usageJournalCommitMu.Lock()
	defer s.usageJournalCommitMu.Unlock()
	current := s.usageJournalAcked.Load()
	first := 0
	for first < len(journalSequences) && journalSequences[first] <= current {
		first++
	}
	if first == len(journalSequences) {
		return nil
	}
	journalSequences = journalSequences[first:]
	contiguous := journalSequences[0] == current+1
	for index := 1; contiguous && index < len(journalSequences); index++ {
		contiguous = journalSequences[index] == journalSequences[index-1]+1
	}
	if !contiguous {
		return s.replayUsageJournalLocked(ctx)
	}
	last := journalSequences[len(journalSequences)-1]
	if err := s.usageJournal.Ack(last); err != nil {
		return err
	}
	s.usageJournalAcked.Store(last)
	return nil
}

func (s *Server) replayUsageJournal(ctx context.Context) error {
	if s.usageJournal == nil {
		return nil
	}
	s.usageJournalCommitMu.Lock()
	defer s.usageJournalCommitMu.Unlock()
	return s.replayUsageJournalLocked(ctx)
}

func (s *Server) replayUsageJournalLocked(ctx context.Context) error {
	snapshot, err := s.usageJournal.Snapshot()
	if err != nil || snapshot.Pending == 0 {
		if err == nil {
			s.usageJournalAcked.Store(snapshot.AckedSequence)
		}
		return err
	}
	target := snapshot.NextSequence - 1
	if err = s.usageJournal.Sync(); err != nil {
		return err
	}
	for {
		records, err := s.usageJournal.Replay(64)
		if err != nil || len(records) == 0 {
			return err
		}
		end := len(records)
		for end > 0 && records[end-1].Sequence > target {
			end--
		}
		if end == 0 {
			return nil
		}
		records = records[:end]
		usageBatch := make([]storage.UsageRecordWrite, 0, len(records))
		holds := make([]storage.BillingHoldWrite, 0, len(records))
		for _, record := range records {
			if record.Usage != nil {
				usageBatch = append(usageBatch, *record.Usage)
			}
			if record.Hold != nil {
				holds = append(holds, *record.Hold)
			}
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err = s.store.BatchWriteTelemetry(writeCtx, usageBatch, nil, holds, nil)
		cancel()
		if err != nil {
			return err
		}
		if err = s.usageJournal.Ack(records[len(records)-1].Sequence); err != nil {
			return err
		}
		s.usageJournalAcked.Store(records[len(records)-1].Sequence)
		if records[len(records)-1].Sequence >= target {
			return nil
		}
	}
}

func (s *Server) usageJournalMetrics() map[string]interface{} {
	out := map[string]interface{}{"enabled": s != nil && s.usageJournal != nil}
	if s == nil || s.usageJournal == nil {
		return out
	}
	snapshot, err := s.usageJournal.Snapshot()
	out["healthy"] = err == nil
	if err != nil {
		return out
	}
	out["acked_sequence"] = snapshot.AckedSequence
	out["next_sequence"] = snapshot.NextSequence
	out["pending_records"] = snapshot.Pending
	out["bytes"] = snapshot.Bytes
	out["segments"] = snapshot.Segments
	return out
}

// flushTelemetry inserts a FIFO boundary without waiting for requests that arrive
// after it. Every usage/hold/audit queued before the boundary is committed before
// this returns. A bounded caller context prevents dashboard reads from stalling.
func (s *Server) flushTelemetry(ctx context.Context) (timedOut bool, err error) {
	done := make(chan error, 1)
	s.asyncMu.RLock()
	if s.asyncClosed {
		s.asyncMu.RUnlock()
		return false, nil
	}
	s.usagePending.Add(1)
	select {
	case s.usageWrites <- telemetryWrite{barrier: done}:
		s.asyncMu.RUnlock()
	case <-ctx.Done():
		s.asyncMu.RUnlock()
		s.usagePending.Done()
		return true, nil
	}
	select {
	case err = <-done:
		return false, err
	case <-ctx.Done():
		return true, nil
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
	if s.usageDirectWrites.Load() && (write.usage != nil || write.hold != nil) {
		if err := s.persistTelemetryBatch([]telemetryWrite{write}); err == nil {
			return
		} else {
			log.Printf("[USAGE-ERROR] direct pressure-mode insert failed; retaining journal fallback: %v", err)
		}
	}
	s.asyncMu.RLock()
	if s.asyncClosed {
		s.asyncMu.RUnlock()
		if err := s.persistTelemetryBatchDetached([]telemetryWrite{write}); err != nil {
			log.Printf("[USAGE-ERROR] inline insert after close: %v", err)
		}
		return
	}
	s.usagePending.Add(1)
	journalLocked := false
	journaled := false
	if s.usageJournal != nil && (write.usage != nil || write.hold != nil) {
		s.usageEnqueueMu.Lock()
		journalLocked = true
		sequence, err := s.usageJournal.Append(usagejournal.Record{Usage: write.usage, Hold: write.hold})
		if err != nil {
			s.usageEnqueueMu.Unlock()
			s.asyncMu.RUnlock()
			log.Printf("[USAGE-JOURNAL] append failed, applying synchronous backpressure: %v", err)
			if persistErr := s.persistTelemetryBatch([]telemetryWrite{write}); persistErr != nil {
				log.Printf("[USAGE-ERROR] journal fallback insert: %v", persistErr)
			}
			s.usagePending.Done()
			return
		}
		write.journalSeq = sequence
		journaled = true
	}
	select {
	case s.usageWrites <- write:
		if journalLocked {
			s.usageEnqueueMu.Unlock()
		}
		s.asyncMu.RUnlock()
		return
	default:
	}
	if journalLocked {
		s.usageEnqueueMu.Unlock()
	}
	s.asyncMu.RUnlock()
	if journaled {
		s.signalUsageJournalReplay()
		s.usagePending.Done()
		return
	}
	err := s.persistTelemetryBatch([]telemetryWrite{write})
	if err != nil {
		log.Printf("[USAGE-ERROR] inline insert: %v", err)
	}
	s.usagePending.Done()
}

func (s *Server) signalUsageJournalReplay() {
	if s.usageJournalWake == nil {
		return
	}
	select {
	case s.usageJournalWake <- struct{}{}:
	default:
	}
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

// FlushWritesContext stops admission to async queues and drains them within the caller's
// deadline. If the deadline expires, the current journal is synced and left replayable
// rather than blocking process shutdown indefinitely.
func (s *Server) FlushWritesContext(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	s.asyncMu.Lock()
	if s.asyncFlushDone == nil {
		s.asyncFlushDone = make(chan struct{})
	}
	s.asyncMu.Unlock()
	s.asyncFlushOnce.Do(func() {
		s.asyncMu.Lock()
		s.asyncClosed = true
		if s.usageJournalStop != nil {
			close(s.usageJournalStop)
		}
		if s.asyncWrites != nil {
			close(s.asyncWrites)
		}
		if s.usageWrites != nil {
			close(s.usageWrites)
		}
		s.asyncMu.Unlock()
		s.stopGoalCompactionWorkers()
		go func() {
			defer supervisor.Recover("bounded-async-flush")
			s.asyncWG.Wait()
			s.usagePending.Wait()
			if s.usageJournal != nil {
				if err := s.replayUsageJournal(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					log.Printf("[USAGE-JOURNAL] final replay failed; records remain durable: %v", err)
				}
				if err := s.usageJournal.Close(); err != nil {
					log.Printf("[USAGE-JOURNAL] close failed: %v", err)
				}
			}
			if s.scheduler != nil {
				s.scheduler.Close()
			}
			if s.asyncWriteCancel != nil {
				s.asyncWriteCancel()
			}
			close(s.asyncFlushDone)
		}()
	})
	select {
	case <-s.asyncFlushDone:
		return nil
	case <-ctx.Done():
		if s.asyncWriteCancel != nil {
			s.asyncWriteCancel()
		}
		if s.usageJournal != nil {
			_ = s.usageJournal.Sync()
		}
		return ctx.Err()
	}
}

// FlushWrites preserves the test and embedding API while still imposing a finite
// ceiling. Production supplies its own shorter shutdown deadline.
func (s *Server) FlushWrites() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.FlushWritesContext(ctx); err != nil {
		log.Printf("[SHUTDOWN] async flush incomplete; durable journal retained: %v", err)
	}
}

// bgWriteContext detaches a write from its originating request but remains cancellable
// by the bounded shutdown flush.
func (s *Server) bgWriteContext() (context.Context, context.CancelFunc) {
	base := s.asyncWriteCtx
	if base == nil {
		base = context.Background()
	}
	return context.WithTimeout(base, 30*time.Second)
}

// WaitForAsyncWrites blocks until every write enqueued before this call has been
// processed. The drainer is FIFO, so enqueuing a sentinel and awaiting it guarantees all
// earlier queued writes have run. Unlike FlushWrites it does not stop the writer, so it
// is safe to call repeatedly — used by tests that assert on rows a request just enqueued.
func (s *Server) WaitForAsyncWrites() {
	done := make(chan struct{})
	s.enqueueWrite(func() { close(done) })
	<-done
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if timedOut, err := s.flushTelemetry(ctx); timedOut {
		log.Printf("[USAGE-ERROR] telemetry flush timed out")
	} else if err != nil {
		log.Printf("[USAGE-ERROR] telemetry flush failed: %v", err)
	}
}
