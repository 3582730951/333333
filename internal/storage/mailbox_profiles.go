package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
)

// mailboxProfileSchemaSQL is intentionally separate from schemaSQL and
// teamManagementSchemaSQL. Both of those schemas have immutable PostgreSQL
// checksums in released installations.
const mailboxProfileSchemaSQL = `
CREATE TABLE IF NOT EXISTS mailbox_provider_health(
  provider_key TEXT PRIMARY KEY,
  last_status TEXT NOT NULL DEFAULT 'unknown',
  last_checked_at INTEGER NOT NULL DEFAULT 0,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  success_count INTEGER NOT NULL DEFAULT 0,
  failure_count INTEGER NOT NULL DEFAULT 0,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  last_error_class TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_mailbox_provider_health_status
  ON mailbox_provider_health(last_status, updated_at);
`

type MailboxProviderHealth struct {
	ProviderKey        string `json:"provider_key"`
	LastStatus         string `json:"last_status"`
	LastCheckedAt      int64  `json:"last_checked_at"`
	LatencyMS          int64  `json:"latency_ms"`
	SuccessCount       int64  `json:"success_count"`
	FailureCount       int64  `json:"failure_count"`
	ConsecutiveFailure int64  `json:"consecutive_failures"`
	LastErrorClass     string `json:"last_error_class,omitempty"`
	UpdatedAt          int64  `json:"updated_at"`
}

// NormalizeMailboxDomain accepts DNS host names (including punycode) but not
// URLs, ports, wildcard labels, or IP literals. Keeping one canonical form makes
// parent/child same-domain comparisons deterministic.
func NormalizeMailboxDomain(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(value, "@")))
	value = strings.TrimSuffix(value, ".")
	if value == "" {
		return "", nil
	}
	if len(value) > 253 || strings.ContainsAny(value, "/:\\ \t\r\n") || net.ParseIP(value) != nil {
		return "", errors.New("mailbox domain must be a DNS host name")
	}
	labels := strings.Split(value, ".")
	if len(labels) < 2 {
		return "", errors.New("mailbox domain must contain at least two labels")
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("mailbox domain contains an invalid label")
		}
		for _, char := range label {
			if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
				continue
			}
			return "", fmt.Errorf("mailbox domain contains unsupported character %q", char)
		}
	}
	return value, nil
}

func EmailDomain(address string) string {
	address = strings.TrimSpace(address)
	index := strings.LastIndexByte(address, '@')
	if index <= 0 || index == len(address)-1 {
		return ""
	}
	domain, err := NormalizeMailboxDomain(address[index+1:])
	if err != nil {
		return ""
	}
	return domain
}

func (s *Store) GetMailboxProviderHealth(ctx context.Context, providerKey string) (MailboxProviderHealth, bool, error) {
	var item MailboxProviderHealth
	err := s.rdb.QueryRowContext(ctx, `
SELECT provider_key,last_status,last_checked_at,latency_ms,success_count,
       failure_count,consecutive_failures,last_error_class,updated_at
FROM mailbox_provider_health WHERE provider_key=?`,
		strings.TrimSpace(providerKey),
	).Scan(
		&item.ProviderKey, &item.LastStatus, &item.LastCheckedAt, &item.LatencyMS,
		&item.SuccessCount, &item.FailureCount, &item.ConsecutiveFailure,
		&item.LastErrorClass, &item.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return MailboxProviderHealth{}, false, nil
	}
	return item, err == nil, err
}

func (s *Store) ListMailboxProviderHealth(ctx context.Context) (map[string]MailboxProviderHealth, error) {
	rows, err := s.rdb.QueryContext(ctx, `
SELECT provider_key,last_status,last_checked_at,latency_ms,success_count,
       failure_count,consecutive_failures,last_error_class,updated_at
FROM mailbox_provider_health ORDER BY provider_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]MailboxProviderHealth)
	for rows.Next() {
		var item MailboxProviderHealth
		if err := rows.Scan(
			&item.ProviderKey, &item.LastStatus, &item.LastCheckedAt, &item.LatencyMS,
			&item.SuccessCount, &item.FailureCount, &item.ConsecutiveFailure,
			&item.LastErrorClass, &item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out[item.ProviderKey] = item
	}
	return out, rows.Err()
}

// RecordMailboxProviderHealth records one bounded connection check. A success
// clears only the consecutive-failure counter; lifetime counters remain useful
// for reliability comparisons and capacity planning.
func (s *Store) RecordMailboxProviderHealth(
	ctx context.Context,
	providerKey string,
	success bool,
	latencyMS int64,
	errorClass string,
) error {
	providerKey = strings.TrimSpace(providerKey)
	if providerKey == "" {
		return errors.New("mailbox provider key is required")
	}
	if latencyMS < 0 {
		latencyMS = 0
	}
	now := Now()
	status := "healthy"
	successDelta, failureDelta := 1, 0
	if !success {
		status = "unhealthy"
		successDelta, failureDelta = 0, 1
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO mailbox_provider_health(
  provider_key,last_status,last_checked_at,latency_ms,success_count,
  failure_count,consecutive_failures,last_error_class,updated_at
) VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(provider_key) DO UPDATE SET
  last_status=excluded.last_status,
  last_checked_at=excluded.last_checked_at,
  latency_ms=excluded.latency_ms,
  success_count=mailbox_provider_health.success_count+?,
  failure_count=mailbox_provider_health.failure_count+?,
  consecutive_failures=CASE
    WHEN excluded.last_status='healthy' THEN 0
    ELSE mailbox_provider_health.consecutive_failures+1
  END,
  last_error_class=excluded.last_error_class,
  updated_at=excluded.updated_at`,
		providerKey, status, now, latencyMS, successDelta, failureDelta,
		failureDelta, strings.TrimSpace(errorClass), now,
		successDelta, failureDelta,
	)
	return err
}
