package api

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/storage"
)

const diagnosticRuntimeRecordLimit = 8192

// diagnosticRuntime holds bounded, metadata-only observations that do not belong
// in the durable business tables. Keeping them on Server makes a diagnostic ZIP
// describe the process that generated it, including failures that happen before
// account routing (and therefore cannot have an account-scoped audit row).
type diagnosticRuntime struct {
	mu               sync.Mutex
	routeAttempts    []diagnosticRouteAttempt
	routeAttemptHead int
	providerAttempts []diagnosticProviderAttempt
	providerHead     int
	diskRejects      atomic.Int64
	capacityRejects  atomic.Int64
}

type diagnosticRouteAttempt struct {
	RequestID      string
	Tier           int
	Target         string
	SelectionType  string
	StatusClass    string
	FallbackTarget string
	CreatedAt      int64
}

type diagnosticProviderAttempt struct {
	RequestID  string
	AccountID  string
	Provider   string
	Phase      string
	Status     int
	ErrorClass string
	BodyHash   string
	RetryAfter string
	CreatedAt  int64
}

func (s *Server) runtimeStorageDiagnostics() map[string]interface{} {
	budget := s.bodyBudgetSnapshot()
	var filesystem bodysource.DiskReserverSnapshot
	if s != nil && s.bodyDiskReserver != nil {
		filesystem = s.bodyDiskReserver.Snapshot()
	}
	diskRejects, capacityRejects := int64(0), int64(0)
	if s != nil {
		diskRejects = s.diagnostics.diskRejects.Load()
		capacityRejects = s.diagnostics.capacityRejects.Load()
	}
	return map[string]interface{}{
		"generated_at": storage.Now(),
		"budget":       budget,
		"filesystem":   filesystem,
		"policy": map[string]interface{}{
			"max_request_body_bytes":            s.cfg.MaxBodyBytes,
			"memory_spill_threshold_bytes":      s.cfg.BodyMemoryThresholdBytes,
			"unknown_length_growth_chunk_bytes": bodysource.DiskReservationChunkBytes,
		},
		"rejection_counts": map[string]int64{
			"request_body_storage_exhausted": diskRejects,
			"local_spool_capacity":           capacityRejects,
			"budget_spool_rejections":        budget.SpoolRejections,
			"disk_reserver_rejections":       filesystem.Rejections,
		},
	}
}

func appendBounded[T any](rows []T, head *int, row T) []T {
	if len(rows) < diagnosticRuntimeRecordLimit {
		return append(rows, row)
	}
	rows[*head] = row
	*head = (*head + 1) % len(rows)
	return rows
}

func snapshotBounded[T any](rows []T, head int) []T {
	if len(rows) < diagnosticRuntimeRecordLimit || head == 0 {
		return append([]T(nil), rows...)
	}
	out := make([]T, 0, len(rows))
	out = append(out, rows[head:]...)
	out = append(out, rows[:head]...)
	return out
}

// recordRouteAttempt records one UserGroup target decision or result. Callers
// pass only route metadata: request bodies, prompts, and raw affinity keys never
// enter this recorder.
func (s *Server) recordRouteAttempt(requestID string, tier int, target, selectionType, statusClass, fallbackTarget string) {
	if s == nil {
		return
	}
	row := diagnosticRouteAttempt{
		RequestID: strings.TrimSpace(requestID), Tier: tier,
		Target: strings.TrimSpace(target), SelectionType: strings.TrimSpace(selectionType),
		StatusClass: strings.TrimSpace(statusClass), FallbackTarget: strings.TrimSpace(fallbackTarget),
		CreatedAt: storage.Now(),
	}
	s.diagnostics.mu.Lock()
	s.diagnostics.routeAttempts = appendBounded(s.diagnostics.routeAttempts, &s.diagnostics.routeAttemptHead, row)
	s.diagnostics.mu.Unlock()
}

// recordProviderAttempt records metadata from one provider wire attempt. BodyHash
// must be a one-way digest generated at the wire boundary; body content and error
// bodies are deliberately outside this interface.
func (s *Server) recordProviderAttempt(requestID, accountID, provider, phase string, status int, errorClass, bodyHash, retryAfter string) {
	if s == nil {
		return
	}
	row := diagnosticProviderAttempt{
		RequestID: strings.TrimSpace(requestID), AccountID: strings.TrimSpace(accountID),
		Provider: strings.TrimSpace(provider), Phase: strings.TrimSpace(phase), Status: status,
		ErrorClass: strings.TrimSpace(errorClass), BodyHash: strings.TrimSpace(bodyHash),
		RetryAfter: strings.TrimSpace(retryAfter), CreatedAt: storage.Now(),
	}
	s.diagnostics.mu.Lock()
	s.diagnostics.providerAttempts = appendBounded(s.diagnostics.providerAttempts, &s.diagnostics.providerHead, row)
	s.diagnostics.mu.Unlock()
}

func (s *Server) diagnosticRouteAttempts() []diagnosticRouteAttempt {
	if s == nil {
		return nil
	}
	s.diagnostics.mu.Lock()
	defer s.diagnostics.mu.Unlock()
	return snapshotBounded(s.diagnostics.routeAttempts, s.diagnostics.routeAttemptHead)
}

func (s *Server) diagnosticProviderAttempts() []diagnosticProviderAttempt {
	if s == nil {
		return nil
	}
	s.diagnostics.mu.Lock()
	defer s.diagnostics.mu.Unlock()
	return snapshotBounded(s.diagnostics.providerAttempts, s.diagnostics.providerHead)
}

func (s *Server) recordBodyStorageRejection(err error) {
	if s == nil {
		return
	}
	var storageErr *bodysource.BodyStorageError
	if (errors.As(err, &storageErr) && storageErr.Class == bodysource.BodyStorageDiskReserve) || errors.Is(err, bodysource.ErrDiskReserve) {
		s.diagnostics.diskRejects.Add(1)
		return
	}
	if storageErr != nil || errors.Is(err, bodysource.ErrSpoolBudget) {
		s.diagnostics.capacityRejects.Add(1)
	}
}

func routeAttemptRows(rows []diagnosticRouteAttempt) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			row.RequestID, strconv.Itoa(row.Tier), row.Target, row.SelectionType,
			row.StatusClass, row.FallbackTarget, strconv.FormatInt(row.CreatedAt, 10),
		})
	}
	return out
}

func providerAttemptRows(rows []diagnosticProviderAttempt, codebook diagnosticCodebook) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, []string{
			row.RequestID, codebook.code(row.AccountID), row.Provider, row.Phase,
			strconv.Itoa(row.Status), row.ErrorClass, row.BodyHash, row.RetryAfter,
			strconv.FormatInt(row.CreatedAt, 10),
		})
	}
	return out
}
