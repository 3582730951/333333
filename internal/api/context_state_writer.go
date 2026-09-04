package api

import (
	"context"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

const (
	codexStateCommitQueueDepth = 4096
	codexStateCommitBatchMax   = 256
	codexStateCommitTimeout    = 30 * time.Second
)

type codexStateCommitResult struct {
	binding storage.CodexSessionBinding
	err     error
}

type codexStateCommitRequest struct {
	commit  storage.CodexSessionCommit
	compact bool
	done    chan codexStateCommitResult
}

func (s *Server) startCodexStateWriter() {
	s.codexStateCommits = make(chan codexStateCommitRequest, codexStateCommitQueueDepth)
	s.asyncWG.Add(1)
	supervisor.GoOnce("codex-state-writer", func() {
		defer s.asyncWG.Done()
		for first := range s.codexStateCommits {
			batch := make([]codexStateCommitRequest, 1, codexStateCommitBatchMax)
			batch[0] = first
			closed := false
			// Group-commit only what is ALREADY queued; never wait for arrivals.
			//
			// This writer used to wait codexStateCommitBatchWait for batch-mates. That
			// was free when the CPA commit ran after the terminal SSE frame had been
			// released. It is no longer: the commit now completes before
			// response.completed reaches the client, so every waiter is holding its own
			// terminal frame, and the wait is added directly to client latency.
			//
			// The amortization no longer pays for itself. One binding transaction
			// measures ~50us, so a full 256-request batch saves at most ~13ms of server
			// time while charging up to 20ms of latency to each of its 256 members.
			// Draining without waiting keeps the batching that actually matters --
			// under real load the queue is non-empty because requests accumulate while
			// the previous batch commits, so batches still form -- and removes the
			// artificial delay when this is the only in-flight turn.
			//
			// Ordering is unchanged: the transaction still completes before the terminal
			// frame is written, so this only moves the transaction's start earlier. It
			// never defers a commit past the frame.
		collect:
			for len(batch) < codexStateCommitBatchMax {
				select {
				case next, ok := <-s.codexStateCommits:
					if !ok {
						closed = true
						break collect
					}
					batch = append(batch, next)
				default:
					break collect
				}
			}
			s.persistCodexStateCommitBatch(batch)
			if closed {
				return
			}
		}
	})
}

func (s *Server) persistCodexStateCommitBatch(batch []codexStateCommitRequest) {
	// Existing trees and compactions carry epoch/window CAS and are continuity
	// critical, so service them before independent first-root creations.
	for index := range batch {
		request := &batch[index]
		if request.commit.Binding.ID == "" && !request.compact {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), codexStateCommitTimeout)
		binding, err := s.store.CommitCodexSessionBinding(ctx, request.commit)
		if err == nil && request.compact {
			binding, err = s.store.AdvanceCodexSessionWindowGeneration(ctx, binding.ID, binding.Epoch, binding.WindowGeneration, binding.ExpiresAt)
		}
		cancel()
		request.done <- codexStateCommitResult{binding: binding, err: err}
	}

	freshIndexes := make([]int, 0, len(batch))
	freshCommits := make([]storage.CodexSessionCommit, 0, len(batch))
	for index := range batch {
		if batch[index].commit.Binding.ID == "" && !batch[index].compact {
			freshIndexes = append(freshIndexes, index)
			freshCommits = append(freshCommits, batch[index].commit)
		}
	}
	if len(freshCommits) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexStateCommitTimeout)
	bindings, err := s.store.CommitFreshCodexSessionBindings(ctx, freshCommits)
	cancel()
	if err == nil {
		for resultIndex, batchIndex := range freshIndexes {
			batch[batchIndex].done <- codexStateCommitResult{binding: bindings[resultIndex]}
		}
		return
	}

	// A batch-level alias conflict rolls back every entry. Retry individually so
	// one damaged legacy hierarchy cannot discard unrelated successful terminals.
	for _, batchIndex := range freshIndexes {
		request := &batch[batchIndex]
		ctx, cancel := context.WithTimeout(context.Background(), codexStateCommitTimeout)
		binding, itemErr := s.store.CommitCodexSessionBinding(ctx, request.commit)
		cancel()
		request.done <- codexStateCommitResult{binding: binding, err: itemErr}
	}
}

func (s *Server) persistCodexStateCommit(ctx context.Context, commit storage.CodexSessionCommit, compact bool) (storage.CodexSessionBinding, error) {
	if s.codexStateCommits == nil {
		binding, err := s.store.CommitCodexSessionBinding(ctx, commit)
		if err == nil && compact {
			binding, err = s.store.AdvanceCodexSessionWindowGeneration(ctx, binding.ID, binding.Epoch, binding.WindowGeneration, binding.ExpiresAt)
		}
		return binding, err
	}
	request := codexStateCommitRequest{commit: commit, compact: compact, done: make(chan codexStateCommitResult, 1)}
	s.asyncMu.RLock()
	if s.asyncClosed {
		s.asyncMu.RUnlock()
		binding, err := s.store.CommitCodexSessionBinding(ctx, commit)
		if err == nil && compact {
			binding, err = s.store.AdvanceCodexSessionWindowGeneration(ctx, binding.ID, binding.Epoch, binding.WindowGeneration, binding.ExpiresAt)
		}
		return binding, err
	}
	select {
	case s.codexStateCommits <- request:
		s.asyncMu.RUnlock()
	case <-ctx.Done():
		s.asyncMu.RUnlock()
		return storage.CodexSessionBinding{}, ctx.Err()
	}
	// Once accepted, the state writer owns a bounded attempt. Waiting for its
	// result keeps response.completed behind the durable transaction boundary.
	result := <-request.done
	return result.binding, result.err
}
