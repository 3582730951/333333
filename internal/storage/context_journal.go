package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type ContextJournal struct {
	ResponseID   string
	AffinityHash string
	AccountID    string
	Payload      string
	CreatedAt    int64
	ExpiresAt    int64
}

func (s *Store) PutContextJournal(ctx context.Context, j ContextJournal) error {
	now := Now()
	if j.CreatedAt == 0 {
		j.CreatedAt = now
	}
	if int64(len(j.Payload)) > maxStoredContextPayloadBytes {
		return fmt.Errorf("context journal payload contains %d bytes, limit is %d", len(j.Payload), maxStoredContextPayloadBytes)
	}
	payload := compressContextPayload(j.Payload)
	_, err := s.db.ExecContext(ctx, `INSERT INTO context_journal(response_id,affinity_hash,account_id,encrypted_payload,created_at,expires_at) VALUES(?,?,?,?,?,?) ON CONFLICT(response_id) DO UPDATE SET affinity_hash=excluded.affinity_hash,account_id=excluded.account_id,encrypted_payload=excluded.encrypted_payload,created_at=excluded.created_at,expires_at=excluded.expires_at`, j.ResponseID, j.AffinityHash, j.AccountID, s.sealToken(payload), j.CreatedAt, j.ExpiresAt)
	return err
}
func (s *Store) GetContextJournal(ctx context.Context, id string) (ContextJournal, error) {
	var j ContextJournal
	var p string
	err := s.rdb.QueryRowContext(ctx, `SELECT response_id,affinity_hash,account_id,encrypted_payload,created_at,expires_at FROM context_journal WHERE response_id=? AND expires_at>?`, id, Now()).Scan(&j.ResponseID, &j.AffinityHash, &j.AccountID, &p, &j.CreatedAt, &j.ExpiresAt)
	if err != nil {
		return j, err
	}
	j.Payload, err = s.openContextPayload(p, maxStoredContextPayloadBytes)
	return j, err
}

// TouchContextJournal slides a live journal row's expiry forward on successful read, so
// an actively-resumed conversation tail survives indefinitely (arbitrary-duration /goal
// tasks) with ZERO extra disk — only the latest full-context row stays warm while older
// turns still expire on their own schedule. The `expires_at>?` guard refuses to
// resurrect an already-expired row. Uses the single-writer pool; callers treat failure
// as best-effort (the read still succeeds).
func (s *Store) TouchContextJournal(ctx context.Context, id string, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE context_journal SET expires_at=? WHERE response_id=? AND expires_at>?`, expiresAt, id, Now())
	return err
}

func (s *Store) CleanupContextJournal(ctx context.Context) (int64, error) {
	r, e := s.db.ExecContext(ctx, `DELETE FROM context_journal WHERE expires_at<=?`, Now())
	if e != nil {
		return 0, e
	}
	return r.RowsAffected()
}

// ClearContextJournal atomically removes every encrypted replay context. SQLite
// compaction is intentionally performed by the API after this transaction commits.
func (s *Store) ClearContextJournal(ctx context.Context) (int64, error) {
	if s.driver != "postgres" {
		_, _ = s.db.ExecContext(ctx, `PRAGMA secure_delete=OFF`)
	}
	var total int64
	for {
		r, err := s.db.ExecContext(ctx, `DELETE FROM context_journal WHERE rowid IN (SELECT rowid FROM context_journal LIMIT 8)`)
		if err != nil {
			return total, err
		}
		n, _ := r.RowsAffected()
		total += n
		if n == 0 {
			return total, nil
		}
		if s.driver != "postgres" {
			if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				return total, err
			}
		}
	}
}

func (s *Store) CleanupContextJournalCreatedBefore(ctx context.Context, cutoff int64) (int64, error) {
	var total int64
	for {
		r, err := s.db.ExecContext(ctx, `DELETE FROM context_journal WHERE rowid IN (SELECT rowid FROM context_journal WHERE created_at<=? LIMIT 64)`, cutoff)
		if err != nil {
			return total, err
		}
		n, _ := r.RowsAffected()
		total += n
		if n == 0 {
			return total, nil
		}
	}
}

// EvictContextJournalToBudget bounds the journal table on low-config VPS hosts: while the
// row count exceeds maxRows OR the stored payload bytes exceed maxBytes, it deletes the
// rows with the LOWEST expires_at first. Because sliding TTL (TouchContextJournal) keeps
// an actively-resumed chain's expires_at far in the future, the lowest-expires_at rows are
// the least-recently-used chains — so the chains most likely to resume are preserved. A
// non-positive value disables that dimension; both non-positive is a no-op. Returns the
// number of rows evicted.
func (s *Store) EvictContextJournalToBudget(ctx context.Context, maxRows, maxBytes int64) (int64, error) {
	if maxRows <= 0 && maxBytes <= 0 {
		return 0, nil
	}
	var total int64
	for {
		var rows, bytesUsed int64
		if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(LENGTH(encrypted_payload)),0) FROM context_journal`).Scan(&rows, &bytesUsed); err != nil {
			return total, err
		}
		overRows := maxRows > 0 && rows > maxRows
		overBytes := maxBytes > 0 && bytesUsed > maxBytes
		if !overRows && !overBytes {
			return total, nil
		}
		// Delete exactly the row excess when the row bound is what is exceeded; when only
		// the byte bound is over, delete a small batch and re-check (row sizes vary, so the
		// exact count is unknown). Cap the batch so a huge excess is chipped away safely.
		var batch int64
		if overRows {
			batch = rows - maxRows
		}
		if overBytes && batch < 64 {
			batch = 64
		}
		if batch < 1 {
			batch = 1
		}
		if batch > 4096 {
			batch = 4096
		}
		r, err := s.db.ExecContext(ctx, `DELETE FROM context_journal WHERE rowid IN (SELECT rowid FROM context_journal ORDER BY expires_at ASC LIMIT ?)`, batch)
		if err != nil {
			return total, err
		}
		n, _ := r.RowsAffected()
		total += n
		if n == 0 {
			return total, nil
		}
	}
}

const (
	// ctx2 is an unambiguous envelope: every new non-empty application payload,
	// including a literal string beginning with "gz1:" or "ctx2:", is placed
	// inside either the raw or gzip form. The byte count is authenticated by the
	// outer storage encryption where enabled and is always verified before use.
	compressedContextPrefix       = "ctx2:g:"
	rawContextPrefix              = "ctx2:r:"
	legacyCompressedContextPrefix = "gz1:"

	// This mirrors the project's 1 GiB request-body ceiling without importing the
	// config package into storage. Call sites with tighter metadata (goal chunks)
	// pass that smaller bound instead.
	maxStoredContextPayloadBytes = int64(1 << 30)
)

func contextEnvelope(prefix string, plainBytes int, body string) string {
	return prefix + strconv.Itoa(plainBytes) + ":" + body
}

func compressContextPayload(payload string) string {
	if payload == "" {
		return ""
	}
	rawEnvelope := contextEnvelope(rawContextPrefix, len(payload), payload)
	if len(payload) < 1024 {
		return rawEnvelope
	}
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, gzip.BestSpeed)
	if err != nil {
		return rawEnvelope
	}
	if _, err = zw.Write([]byte(payload)); err != nil {
		return rawEnvelope
	}
	if err = zw.Close(); err != nil {
		return rawEnvelope
	}
	compressedEnvelope := contextEnvelope(compressedContextPrefix, len(payload), base64.RawStdEncoding.EncodeToString(out.Bytes()))
	if len(compressedEnvelope) >= len(rawEnvelope) {
		return rawEnvelope
	}
	return compressedEnvelope
}

func splitContextEnvelope(payload, prefix string) (int64, string, error) {
	rest := strings.TrimPrefix(payload, prefix)
	colon := strings.IndexByte(rest, ':')
	if colon <= 0 {
		return 0, "", errors.New("context envelope is missing its plaintext length")
	}
	plainBytes, err := strconv.ParseInt(rest[:colon], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("invalid context envelope length: %w", err)
	}
	if plainBytes < 0 {
		return 0, "", errors.New("context envelope has a negative plaintext length")
	}
	return plainBytes, rest[colon+1:], nil
}

func readGzipContext(encoded string, expectedBytes, maxBytes int64) (string, error) {
	if maxBytes < 0 {
		return "", errors.New("invalid context decompression limit")
	}
	if expectedBytes >= 0 && expectedBytes > maxBytes {
		return "", fmt.Errorf("context payload declares %d bytes, limit is %d", expectedBytes, maxBytes)
	}
	zr, err := gzip.NewReader(base64.NewDecoder(base64.RawStdEncoding, strings.NewReader(encoded)))
	if err != nil {
		return "", err
	}
	limit := maxBytes
	if expectedBytes >= 0 {
		limit = expectedBytes
	}
	decoded, readErr := io.ReadAll(io.LimitReader(zr, limit+1))
	closeErr := zr.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if int64(len(decoded)) > limit {
		return "", fmt.Errorf("context payload exceeds %d-byte decompression limit", limit)
	}
	if expectedBytes >= 0 && int64(len(decoded)) != expectedBytes {
		return "", fmt.Errorf("context payload decoded to %d bytes, expected %d", len(decoded), expectedBytes)
	}
	return string(decoded), nil
}

func decompressContextPayloadChecked(payload string, maxBytes int64) (string, error) {
	if maxBytes < 0 {
		return "", errors.New("invalid context decompression limit")
	}
	switch {
	case payload == "":
		return "", nil
	case strings.HasPrefix(payload, rawContextPrefix):
		expectedBytes, raw, err := splitContextEnvelope(payload, rawContextPrefix)
		if err != nil {
			return "", err
		}
		if expectedBytes > maxBytes {
			return "", fmt.Errorf("context payload declares %d bytes, limit is %d", expectedBytes, maxBytes)
		}
		if int64(len(raw)) != expectedBytes {
			return "", fmt.Errorf("raw context payload contains %d bytes, expected %d", len(raw), expectedBytes)
		}
		return raw, nil
	case strings.HasPrefix(payload, compressedContextPrefix):
		expectedBytes, encoded, err := splitContextEnvelope(payload, compressedContextPrefix)
		if err != nil {
			return "", err
		}
		return readGzipContext(encoded, expectedBytes, maxBytes)
	case strings.HasPrefix(payload, legacyCompressedContextPrefix):
		// Legacy gz1 did not carry its plaintext size. The streaming limit still
		// prevents an old/corrupt value from allocating past the caller's bound.
		return readGzipContext(strings.TrimPrefix(payload, legacyCompressedContextPrefix), -1, maxBytes)
	default:
		if int64(len(payload)) > maxBytes {
			return "", fmt.Errorf("legacy context payload contains %d bytes, limit is %d", len(payload), maxBytes)
		}
		return payload, nil
	}
}

func (s *Store) openContextPayload(encrypted string, maxBytes int64) (string, error) {
	plain := s.openToken(encrypted)
	if encrypted != "" && plain == "" {
		if err := s.CryptoError(); err != nil {
			return "", err
		}
	}
	return decompressContextPayloadChecked(plain, maxBytes)
}
