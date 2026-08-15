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
	codexStateCommitBatchWait  = 20 * time.Millisecond
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
			timer := time.NewTimer(codexStateCommitBatchWait)
			closed := false
		collect:
			for len(batch) < codexStateCommitBatchMax {
				select {
				case next, ok := <-s.codexStateCommits:
					if !ok {
						closed = true
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
