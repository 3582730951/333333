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

const accountRequestRateSchemaSQL = `
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

const (
	AccountRequestRateWindowSeconds    = int64(60)
	accountRequestRateRetentionSeconds = int64(5 * 60)
	maxAccountRequestRateIDs           = 100
)

type AccountRequestRateBucket struct {
	WriterID     string
	AccountID    string
	BucketStart  int64
	RequestCount int64
	UpdatedAt    int64
}

type AccountRequestRateAggregate struct {
	RPM             int64
	LatestUpdatedAt int64
}

type AccountRequestRate struct {
	RPM           int64  `json:"rpm"`
	WindowSeconds int64  `json:"window_seconds"`
	SampledAt     int64  `json:"sampled_at"`
	State         string `json:"state"`
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
	for _, bucket := range buckets {
		if strings.TrimSpace(bucket.WriterID) == "" || strings.TrimSpace(bucket.AccountID) == "" || bucket.RequestCount < 0 {
			continue
		}
		if _, err = stmt.ExecContext(ctx, bucket.WriterID, bucket.AccountID, bucket.BucketStart, bucket.RequestCount, bucket.UpdatedAt); err != nil {
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
	query := `SELECT account_id,COALESCE(SUM(request_count),0),COALESCE(MAX(updated_at),0)
FROM account_request_rate_buckets
WHERE account_id IN (` + strings.Join(placeholders, ",") + `) AND bucket_start>=? AND bucket_start<=?
GROUP BY account_id`
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]AccountRequestRateAggregate, len(ids))
	for rows.Next() {
		var id string
		var aggregate AccountRequestRateAggregate
		if err := rows.Scan(&id, &aggregate.RPM, &aggregate.LatestUpdatedAt); err != nil {
			return nil, err
		}
		result[id] = aggregate
	}
	return result, rows.Err()
}

func (s *Store) CleanupAccountRequestRateBuckets(ctx context.Context, before int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM account_request_rate_buckets WHERE bucket_start<?`, before)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

type accountRateBucketKey struct {
	accountID   string
	bucketStart int64
}

type localAccountRateBucket struct {
	count     int64
	persisted int64
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
	bucket.count++
	m.mu.Unlock()
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
	counts := make([]int64, 0, len(m.buckets))
	for key, local := range m.buckets {
		if key.bucketStart < now-accountRequestRateRetentionSeconds {
			delete(m.buckets, key)
			continue
		}
		if local.count <= local.persisted {
			continue
		}
		keys = append(keys, key)
		counts = append(counts, local.count)
		buckets = append(buckets, AccountRequestRateBucket{
			WriterID: m.writerID, AccountID: key.accountID, BucketStart: key.bucketStart,
			RequestCount: local.count, UpdatedAt: now,
		})
	}
	m.mu.Unlock()
	if err := m.store.UpsertAccountRequestRateBuckets(ctx, buckets); err != nil {
		m.lastError.Store(err.Error())
		return err
	}
	m.mu.Lock()
	for index, key := range keys {
		if local := m.buckets[key]; local != nil && counts[index] > local.persisted {
			local.persisted = counts[index]
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
			aggregate.RPM += delta
		}
		aggregates[key.accountID] = aggregate
	}
	m.mu.Unlock()
	result := make(map[string]AccountRequestRate, len(ids))
	for _, id := range ids {
		result[id] = AccountRequestRate{
			RPM: aggregates[id].RPM, WindowSeconds: AccountRequestRateWindowSeconds,
			SampledAt: sampledAt, State: state,
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
