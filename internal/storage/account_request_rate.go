package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/supervisor"

	"github.com/google/uuid"
)

const accountRequestRateV1SchemaSQL = `
CREATE TABLE IF NOT EXISTS account_request_rate_buckets (
  writer_id TEXT NOT NULL,
  account_id TEXT NOT NULL,
  bucket_start INTEGER NOT NULL,
  request_count BIGINT NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(writer_id, account_id, bucket_start)
);
CREATE INDEX IF NOT EXISTS idx_account_request_rate_account_bucket
  ON account_request_rate_buckets(account_id,bucket_start);
CREATE INDEX IF NOT EXISTS idx_account_request_rate_bucket_start
  ON account_request_rate_buckets(bucket_start);
`

// accountRequestRateV2SchemaSQL deliberately uses additive tables instead of
// widening the v1 bucket. During a hot upgrade an old worker can keep writing the
// v1 total while a new worker adds dimensions; any dimensional gap is reconciled
// to "unknown" at read time, so totals never disappear or double count.
const accountRequestRateV2SchemaSQL = `
CREATE TABLE IF NOT EXISTS account_request_rate_class_buckets (
  writer_id TEXT NOT NULL,
  account_id TEXT NOT NULL,
  bucket_start INTEGER NOT NULL,
  root_count BIGINT NOT NULL DEFAULT 0,
  subagent_count BIGINT NOT NULL DEFAULT 0,
  unknown_count BIGINT NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(writer_id, account_id, bucket_start)
);
CREATE INDEX IF NOT EXISTS idx_account_request_rate_class_account_bucket
  ON account_request_rate_class_buckets(account_id,bucket_start);
CREATE INDEX IF NOT EXISTS idx_account_request_rate_class_bucket_start
  ON account_request_rate_class_buckets(bucket_start);

CREATE TABLE IF NOT EXISTS account_usage_rate_events (
  event_id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  occurred_at INTEGER NOT NULL,
  input_tokens BIGINT NOT NULL DEFAULT 0,
  cached_input_tokens BIGINT NOT NULL DEFAULT 0,
  output_tokens BIGINT NOT NULL DEFAULT 0,
  total_tokens BIGINT NOT NULL DEFAULT 0,
  agent_class TEXT NOT NULL DEFAULT 'unknown',
  settlement_state TEXT NOT NULL DEFAULT 'unsettled',
  estimated INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_account_usage_rate_account_occurred
  ON account_usage_rate_events(account_id,occurred_at);
CREATE INDEX IF NOT EXISTS idx_account_usage_rate_occurred
  ON account_usage_rate_events(occurred_at);
`

const accountRequestRateSchemaSQL = accountRequestRateV1SchemaSQL + accountRequestRateV2SchemaSQL

const (
	AccountRequestRateWindowSeconds    = int64(60)
	accountRequestRateRetentionSeconds = int64(5 * 60)
	maxAccountRequestRateIDs           = 100
)

type AccountRequestRateBucket struct {
	WriterID      string
	AccountID     string
	BucketStart   int64
	RequestCount  int64
	RootCount     int64
	SubagentCount int64
	UnknownCount  int64
	UpdatedAt     int64
}

type AccountRequestRateAggregate struct {
	AttemptRPM         int64
	AttemptRootRPM     int64
	AttemptSubagentRPM int64
	AttemptUnknownRPM  int64
	LogicalRPM         int64
	RootRPM            int64
	SubagentRPM        int64
	UnknownRPM         int64
	InputTPM           int64
	CachedInputTPM     int64
	OutputTPM          int64
	TPM                int64
	LatestUpdatedAt    int64
}

type AccountRequestRate struct {
	// RPM is the legacy wire field and remains an alias for AttemptRPM so older
	// clients keep seeing the historical request-attempt counter. New clients
	// must use LogicalRPM for user-request accounting; retries/failover are exposed
	// separately through AttemptRPM.
	RPM                int64  `json:"rpm"`
	LogicalRPM         int64  `json:"logical_rpm"`
	AttemptRPM         int64  `json:"attempt_rpm"`
	AttemptRootRPM     int64  `json:"attempt_root_rpm"`
	AttemptSubagentRPM int64  `json:"attempt_subagent_rpm"`
	AttemptUnknownRPM  int64  `json:"attempt_unknown_rpm"`
	RootRPM            int64  `json:"root_rpm"`
	SubagentRPM        int64  `json:"subagent_rpm"`
	UnknownRPM         int64  `json:"unknown_rpm"`
	TPM                int64  `json:"tpm"`
	InputTPM           int64  `json:"input_tpm"`
	CachedInputTPM     int64  `json:"cached_input_tpm"`
	OutputTPM          int64  `json:"output_tpm"`
	WindowSeconds      int64  `json:"window_seconds"`
	SampledAt          int64  `json:"sampled_at"`
	State              string `json:"state"`
}

const (
	AgentClassRoot     = "root"
	AgentClassSubagent = "subagent"
	AgentClassUnknown  = "unknown"
)

func NormalizeAgentClass(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case AgentClassRoot:
		return AgentClassRoot
	case AgentClassSubagent:
		return AgentClassSubagent
	default:
		return AgentClassUnknown
	}
}

func normalizeRateAccountIDs(accountIDs []string) ([]string, error) {
	seen := make(map[string]struct{}, len(accountIDs))
	ids := make([]string, 0, len(accountIDs))
	for _, raw := range accountIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		if len(ids) >= maxAccountRequestRateIDs {
			return nil, fmt.Errorf("account request rate query exceeds %d unique account ids", maxAccountRequestRateIDs)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// UpsertAccountRequestRateBuckets persists cumulative absolute bucket counts.
// Replaying the same flush is idempotent; a late older flush can never lower a row.
func (s *Store) UpsertAccountRequestRateBuckets(ctx context.Context, buckets []AccountRequestRateBucket) error {
	if s == nil || s.db == nil || len(buckets) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO account_request_rate_buckets(writer_id,account_id,bucket_start,request_count,updated_at)
VALUES(?,?,?,?,?)
ON CONFLICT(writer_id,account_id,bucket_start) DO UPDATE SET
 request_count=CASE WHEN excluded.request_count>account_request_rate_buckets.request_count THEN excluded.request_count ELSE account_request_rate_buckets.request_count END,
 updated_at=CASE WHEN excluded.updated_at>account_request_rate_buckets.updated_at THEN excluded.updated_at ELSE account_request_rate_buckets.updated_at END`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	classStmt, err := tx.PrepareContext(ctx, `INSERT INTO account_request_rate_class_buckets(writer_id,account_id,bucket_start,root_count,subagent_count,unknown_count,updated_at)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(writer_id,account_id,bucket_start) DO UPDATE SET
 root_count=CASE WHEN excluded.root_count>account_request_rate_class_buckets.root_count THEN excluded.root_count ELSE account_request_rate_class_buckets.root_count END,
 subagent_count=CASE WHEN excluded.subagent_count>account_request_rate_class_buckets.subagent_count THEN excluded.subagent_count ELSE account_request_rate_class_buckets.subagent_count END,
 unknown_count=CASE WHEN excluded.unknown_count>account_request_rate_class_buckets.unknown_count THEN excluded.unknown_count ELSE account_request_rate_class_buckets.unknown_count END,
 updated_at=CASE WHEN excluded.updated_at>account_request_rate_class_buckets.updated_at THEN excluded.updated_at ELSE account_request_rate_class_buckets.updated_at END`)
	if err != nil {
		return err
	}
	defer classStmt.Close()
	for _, bucket := range buckets {
		if strings.TrimSpace(bucket.WriterID) == "" || strings.TrimSpace(bucket.AccountID) == "" || bucket.RequestCount < 0 {
			continue
		}
		if _, err = stmt.ExecContext(ctx, bucket.WriterID, bucket.AccountID, bucket.BucketStart, bucket.RequestCount, bucket.UpdatedAt); err != nil {
			return err
		}
		if bucket.RootCount < 0 || bucket.SubagentCount < 0 || bucket.UnknownCount < 0 {
			continue
		}
		if _, err = classStmt.ExecContext(ctx, bucket.WriterID, bucket.AccountID, bucket.BucketStart, bucket.RootCount, bucket.SubagentCount, bucket.UnknownCount, bucket.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AccountRequestRateAggregates performs one grouped range query for up to 100
// accounts. Integer-second buckets include now..now-59 and exclude now-60.
func (s *Store) AccountRequestRateAggregates(ctx context.Context, accountIDs []string, sampledAt int64) (map[string]AccountRequestRateAggregate, error) {
	ids, err := normalizeRateAccountIDs(accountIDs)
	if err != nil || len(ids) == 0 {
		return map[string]AccountRequestRateAggregate{}, err
	}
	if sampledAt <= 0 {
		sampledAt = Now()
	}
	args := make([]interface{}, 0, len(ids)+2)
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	args = append(args, sampledAt-(AccountRequestRateWindowSeconds-1), sampledAt)
	// Read individual buckets instead of asking the database to SUM BIGINTs. SQLite
	// (and PostgreSQL) raise an integer-overflow error when a pathological/corrupt
	// installation contains values whose sum exceeds int64; a metrics endpoint must
	// remain available and expose a clamped, non-negative value rather than wrap.
	query := `SELECT account_id,request_count,updated_at
FROM account_request_rate_buckets
WHERE account_id IN (` + strings.Join(placeholders, ",") + `) AND bucket_start>=? AND bucket_start<=?
`
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]AccountRequestRateAggregate, len(ids))
	for rows.Next() {
		var id string
		var count, updatedAt int64
		if err := rows.Scan(&id, &count, &updatedAt); err != nil {
			return nil, err
		}
		aggregate := result[id]
		addRateCounter(&aggregate.AttemptRPM, count)
		if updatedAt > aggregate.LatestUpdatedAt {
			aggregate.LatestUpdatedAt = updatedAt
		}
		result[id] = aggregate
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	classQuery := `SELECT account_id,root_count,subagent_count,unknown_count,updated_at
FROM account_request_rate_class_buckets
WHERE account_id IN (` + strings.Join(placeholders, ",") + `) AND bucket_start>=? AND bucket_start<=?
`
	classRows, err := s.rdb.QueryContext(ctx, classQuery, args...)
	if err != nil {
		return nil, err
	}
	for classRows.Next() {
		var id string
		var root, subagent, unknown, updatedAt int64
		if err := classRows.Scan(&id, &root, &subagent, &unknown, &updatedAt); err != nil {
			classRows.Close()
			return nil, err
		}
		aggregate := result[id]
		addRateCounter(&aggregate.AttemptRootRPM, root)
		addRateCounter(&aggregate.AttemptSubagentRPM, subagent)
		addRateCounter(&aggregate.AttemptUnknownRPM, unknown)
		if updatedAt > aggregate.LatestUpdatedAt {
			aggregate.LatestUpdatedAt = updatedAt
		}
		result[id] = aggregate
	}
	if err := classRows.Close(); err != nil {
		return nil, err
	}
	if err := classRows.Err(); err != nil {
		return nil, err
	}
	// Usage events are selected row-by-row for the same overflow-safe reason.
	usageQuery := `SELECT account_id,input_tokens,cached_input_tokens,output_tokens,total_tokens,agent_class,updated_at
FROM account_usage_rate_events
WHERE account_id IN (` + strings.Join(placeholders, ",") + `) AND occurred_at>=? AND occurred_at<=?
`
	usageRows, err := s.rdb.QueryContext(ctx, usageQuery, args...)
	if err != nil {
		return nil, err
	}
	for usageRows.Next() {
		var id string
		var input, cachedInput, output, total, updatedAt int64
		var agentClass string
		if err := usageRows.Scan(&id, &input, &cachedInput, &output, &total, &agentClass, &updatedAt); err != nil {
			usageRows.Close()
			return nil, err
		}
		aggregate := result[id]
		addRateCounter(&aggregate.LogicalRPM, 1)
		switch NormalizeAgentClass(agentClass) {
		case AgentClassRoot:
			addRateCounter(&aggregate.RootRPM, 1)
		case AgentClassSubagent:
			addRateCounter(&aggregate.SubagentRPM, 1)
		default:
			addRateCounter(&aggregate.UnknownRPM, 1)
		}
		addRateCounter(&aggregate.InputTPM, input)
		addRateCounter(&aggregate.CachedInputTPM, cachedInput)
		addRateCounter(&aggregate.OutputTPM, output)
		addRateCounter(&aggregate.TPM, total)
		if updatedAt > aggregate.LatestUpdatedAt {
			aggregate.LatestUpdatedAt = updatedAt
		}
		result[id] = aggregate
	}
	if err := usageRows.Close(); err != nil {
		return nil, err
	}
	if err := usageRows.Err(); err != nil {
		return nil, err
	}
	for id, aggregate := range result {
		reconcileRateClasses(aggregate.AttemptRPM, &aggregate.AttemptRootRPM, &aggregate.AttemptSubagentRPM, &aggregate.AttemptUnknownRPM)
		reconcileRateClasses(aggregate.LogicalRPM, &aggregate.RootRPM, &aggregate.SubagentRPM, &aggregate.UnknownRPM)
		result[id] = aggregate
	}
	return result, nil
}

func (s *Store) CleanupAccountRequestRateBuckets(ctx context.Context, before int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM account_request_rate_buckets WHERE bucket_start<?`, before)
	if err != nil {
		return 0, err
	}
	deleted, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return 0, rowsErr
	}
	if _, err = s.db.ExecContext(ctx, `DELETE FROM account_request_rate_class_buckets WHERE bucket_start<?`, before); err != nil {
		return deleted, err
	}
	if _, err = s.db.ExecContext(ctx, `DELETE FROM account_usage_rate_events WHERE occurred_at<?`, before); err != nil {
		return deleted, err
	}
	return deleted, nil
}

type accountRateBucketKey struct {
	accountID   string
	bucketStart int64
}

type localAccountRateBucket struct {
	count             int64
	rootCount         int64
	subagentCount     int64
	unknownCount      int64
	persisted         int64
	persistedRoot     int64
	persistedSubagent int64
	persistedUnknown  int64
}

// AccountRateMeter is a process-local hot-path counter with one durable writer
// identity. Multiple draining/active workers safely add through distinct writer IDs.
type AccountRateMeter struct {
	store    *Store
	writerID string
	now      func() time.Time

	mu      sync.Mutex
	buckets map[accountRateBucketKey]*localAccountRateBucket

	startOnce        sync.Once
	running          atomic.Bool
	lastFlushAttempt atomic.Int64
	lastFlushSuccess atomic.Int64
	lastError        atomic.Value
}

func NewAccountRateMeter(store *Store, releaseID string) *AccountRateMeter {
	releaseID = strings.TrimSpace(releaseID)
	if releaseID == "" {
		releaseID = "development"
	}
	return &AccountRateMeter{
		store: store, writerID: releaseID + ":" + uuid.NewString(), now: time.Now,
		buckets: make(map[accountRateBucketKey]*localAccountRateBucket),
	}
}

func (m *AccountRateMeter) WriterID() string {
	if m == nil {
		return ""
	}
	return m.writerID
}

// ObserveAttempt is allocation-free after an account's current-second bucket is
// created and never touches the database. Provider and routeKind are accepted at
// the unified protocol boundary for audit/test clarity but deliberately do not
// become database labels, keeping storage cardinality bounded by account and time.
func (m *AccountRateMeter) ObserveAttempt(accountID, provider, routeKind string, observedAt time.Time) {
	m.ObserveAttemptClass(accountID, provider, routeKind, AgentClassUnknown, observedAt)
}

func (m *AccountRateMeter) ObserveAttemptClass(accountID, provider, routeKind, agentClass string, observedAt time.Time) {
	if m == nil {
		return
	}
	_, _ = provider, routeKind
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return
	}
	if observedAt.IsZero() {
		observedAt = m.now()
	}
	key := accountRateBucketKey{accountID: accountID, bucketStart: observedAt.UTC().Unix()}
	m.mu.Lock()
	bucket := m.buckets[key]
	if bucket == nil {
		bucket = &localAccountRateBucket{}
		m.buckets[key] = bucket
	}
	bucket.count = saturatingRateIncrement(bucket.count)
	switch NormalizeAgentClass(agentClass) {
	case AgentClassRoot:
		bucket.rootCount = saturatingRateIncrement(bucket.rootCount)
	case AgentClassSubagent:
		bucket.subagentCount = saturatingRateIncrement(bucket.subagentCount)
	default:
		bucket.unknownCount = saturatingRateIncrement(bucket.unknownCount)
	}
	m.mu.Unlock()
}

const maxRateInt64 = int64(^uint64(0) >> 1)

func saturatingRateIncrement(value int64) int64 {
	if value >= maxRateInt64 {
		return maxRateInt64
	}
	if value < 0 {
		return 0
	}
	return value + 1
}

// addRateCounter adds a non-negative persisted counter without ever wrapping.
// A value larger than int64 cannot be represented in the API contract; clamping
// is preferable to returning a negative rate or taking the whole dashboard down.
func addRateCounter(destination *int64, value int64) {
	if destination == nil || value <= 0 {
		return
	}
	if *destination < 0 || value > maxRateInt64-*destination {
		*destination = maxRateInt64
		return
	}
	*destination += value
}

// reconcileRateClasses makes the class partition a deterministic partition of
// its authoritative total. Root and subagent are retained in that order; any
// missing or excess dimensional data is represented by unknown/remainder.
func reconcileRateClasses(total int64, root, subagent, unknown *int64) {
	if root == nil || subagent == nil || unknown == nil {
		return
	}
	if total < 0 {
		total = 0
	}
	remaining := total
	if *root < 0 {
		*root = 0
	}
	if *subagent < 0 {
		*subagent = 0
	}
	if *root > remaining {
		*root = remaining
	}
	remaining -= *root
	if *subagent > remaining {
		*subagent = remaining
	}
	remaining -= *subagent
	if remaining < 0 {
		remaining = 0
	}
	*unknown = remaining
}

func (m *AccountRateMeter) Start(ctx context.Context) {
	if m == nil || m.store == nil || ctx == nil {
		return
	}
	m.startOnce.Do(func() {
		m.running.Store(true)
		supervisor.Go(ctx, "account-rate-meter", m.run)
	})
}

func (m *AccountRateMeter) run(ctx context.Context) {
	m.running.Store(true)
	defer m.running.Store(false)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	cleanup := time.NewTicker(time.Minute)
	defer cleanup.Stop()
	var retryAfter time.Time
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = m.Flush(flushCtx)
			cancel()
			return
		case <-ticker.C:
			if m.now().Before(retryAfter) {
				continue
			}
			if err := m.Flush(ctx); err != nil {
				retryAfter = m.now().Add(backoff)
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			} else {
				retryAfter = time.Time{}
				backoff = time.Second
			}
		case <-cleanup.C:
			_, _ = m.store.CleanupAccountRequestRateBuckets(ctx, m.now().Unix()-accountRequestRateRetentionSeconds)
		}
	}
}

func (m *AccountRateMeter) Flush(ctx context.Context) error {
	if m == nil || m.store == nil {
		return errors.New("account rate meter storage unavailable")
	}
	now := m.now().Unix()
	m.lastFlushAttempt.Store(now)
	m.mu.Lock()
	keys := make([]accountRateBucketKey, 0, len(m.buckets))
	buckets := make([]AccountRequestRateBucket, 0, len(m.buckets))
	counts := make([][4]int64, 0, len(m.buckets))
	for key, local := range m.buckets {
		if key.bucketStart < now-accountRequestRateRetentionSeconds {
			delete(m.buckets, key)
			continue
		}
		if local.count <= local.persisted && local.rootCount <= local.persistedRoot && local.subagentCount <= local.persistedSubagent && local.unknownCount <= local.persistedUnknown {
			continue
		}
		keys = append(keys, key)
		counts = append(counts, [4]int64{local.count, local.rootCount, local.subagentCount, local.unknownCount})
		buckets = append(buckets, AccountRequestRateBucket{
			WriterID: m.writerID, AccountID: key.accountID, BucketStart: key.bucketStart,
			RequestCount: local.count, RootCount: local.rootCount, SubagentCount: local.subagentCount,
			UnknownCount: local.unknownCount, UpdatedAt: now,
		})
	}
	m.mu.Unlock()
	if err := m.store.UpsertAccountRequestRateBuckets(ctx, buckets); err != nil {
		m.lastError.Store(err.Error())
		return err
	}
	m.mu.Lock()
	for index, key := range keys {
		if local := m.buckets[key]; local != nil {
			if counts[index][0] > local.persisted {
				local.persisted = counts[index][0]
			}
			if counts[index][1] > local.persistedRoot {
				local.persistedRoot = counts[index][1]
			}
			if counts[index][2] > local.persistedSubagent {
				local.persistedSubagent = counts[index][2]
			}
			if counts[index][3] > local.persistedUnknown {
				local.persistedUnknown = counts[index][3]
			}
		}
	}
	m.mu.Unlock()
	m.lastFlushSuccess.Store(now)
	m.lastError.Store("")
	return nil
}

func (m *AccountRateMeter) Status() string {
	if m == nil || m.store == nil || !m.running.Load() {
		return "unavailable"
	}
	now := m.now().Unix()
	lastSuccess := m.lastFlushSuccess.Load()
	if lastSuccess == 0 {
		if m.lastFlushAttempt.Load() > 0 {
			return "unavailable"
		}
		return "live"
	}
	if now-lastSuccess > accountRequestRateRetentionSeconds {
		return "stale"
	}
	return "live"
}

func (m *AccountRateMeter) Rates(ctx context.Context, accountIDs []string, sampledAt int64) (map[string]AccountRequestRate, error) {
	ids, err := normalizeRateAccountIDs(accountIDs)
	if err != nil {
		return nil, err
	}
	if sampledAt <= 0 {
		if m != nil && m.now != nil {
			sampledAt = m.now().Unix()
		} else {
			sampledAt = Now()
		}
	}
	if m == nil || m.store == nil {
		result := make(map[string]AccountRequestRate, len(ids))
		for _, id := range ids {
			result[id] = AccountRequestRate{WindowSeconds: AccountRequestRateWindowSeconds, SampledAt: sampledAt, State: "unavailable"}
		}
		return result, nil
	}
	aggregates, queryErr := m.store.AccountRequestRateAggregates(ctx, ids, sampledAt)
	state := m.Status()
	if queryErr != nil {
		if m.lastFlushSuccess.Load() == 0 {
			state = "unavailable"
		} else {
			state = "stale"
		}
		aggregates = make(map[string]AccountRequestRateAggregate, len(ids))
	}
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	m.mu.Lock()
	for key, local := range m.buckets {
		if _, ok := wanted[key.accountID]; !ok || key.bucketStart < sampledAt-(AccountRequestRateWindowSeconds-1) || key.bucketStart > sampledAt {
			continue
		}
		aggregate := aggregates[key.accountID]
		if delta := local.count - local.persisted; delta > 0 {
			addRateCounter(&aggregate.AttemptRPM, delta)
		}
		if delta := local.rootCount - local.persistedRoot; delta > 0 {
			addRateCounter(&aggregate.AttemptRootRPM, delta)
		}
		if delta := local.subagentCount - local.persistedSubagent; delta > 0 {
			addRateCounter(&aggregate.AttemptSubagentRPM, delta)
		}
		if delta := local.unknownCount - local.persistedUnknown; delta > 0 {
			addRateCounter(&aggregate.AttemptUnknownRPM, delta)
		}
		aggregates[key.accountID] = aggregate
	}
	m.mu.Unlock()
	for id, aggregate := range aggregates {
		reconcileRateClasses(aggregate.AttemptRPM, &aggregate.AttemptRootRPM, &aggregate.AttemptSubagentRPM, &aggregate.AttemptUnknownRPM)
		reconcileRateClasses(aggregate.LogicalRPM, &aggregate.RootRPM, &aggregate.SubagentRPM, &aggregate.UnknownRPM)
		aggregates[id] = aggregate
	}
	result := make(map[string]AccountRequestRate, len(ids))
	for _, id := range ids {
		result[id] = AccountRequestRate{
			RPM: aggregates[id].AttemptRPM, LogicalRPM: aggregates[id].LogicalRPM, AttemptRPM: aggregates[id].AttemptRPM,
			AttemptRootRPM: aggregates[id].AttemptRootRPM, AttemptSubagentRPM: aggregates[id].AttemptSubagentRPM, AttemptUnknownRPM: aggregates[id].AttemptUnknownRPM,
			RootRPM: aggregates[id].RootRPM, SubagentRPM: aggregates[id].SubagentRPM, UnknownRPM: aggregates[id].UnknownRPM,
			TPM: aggregates[id].TPM, InputTPM: aggregates[id].InputTPM, CachedInputTPM: aggregates[id].CachedInputTPM, OutputTPM: aggregates[id].OutputTPM,
			WindowSeconds: AccountRequestRateWindowSeconds,
			SampledAt:     sampledAt, State: state,
		}
	}
	return result, queryErr
}

// SortedAccountRequestRateIDs is useful to stream hubs that need a stable union.
func SortedAccountRequestRateIDs(ids map[string]struct{}) []string {
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Strings(result)
	if len(result) > maxAccountRequestRateIDs {
		result = result[:maxAccountRequestRateIDs]
	}
	return result
}
