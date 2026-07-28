package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
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
	payload := compressContextPayload(j.Payload)
	_, err := s.db.ExecContext(ctx, `INSERT INTO context_journal(response_id,affinity_hash,account_id,encrypted_payload,created_at,expires_at) VALUES(?,?,?,?,?,?) ON CONFLICT(response_id) DO UPDATE SET affinity_hash=excluded.affinity_hash,account_id=excluded.account_id,encrypted_payload=excluded.encrypted_payload,created_at=excluded.created_at,expires_at=excluded.expires_at`, j.ResponseID, j.AffinityHash, j.AccountID, s.sealToken(payload), j.CreatedAt, j.ExpiresAt)
	return err
}
func (s *Store) GetContextJournal(ctx context.Context, id string) (ContextJournal, error) {
	var j ContextJournal
	var p string
	err := s.rdb.QueryRowContext(ctx, `SELECT response_id,affinity_hash,account_id,encrypted_payload,created_at,expires_at FROM context_journal WHERE response_id=? AND expires_at>?`, id, Now()).Scan(&j.ResponseID, &j.AffinityHash, &j.AccountID, &p, &j.CreatedAt, &j.ExpiresAt)
	j.Payload = decompressContextPayload(s.openToken(p))
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

const compressedContextPrefix = "gz1:"

func compressContextPayload(payload string) string {
	if len(payload) < 1024 {
		return payload
	}
	var out bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestSpeed)
	_, _ = zw.Write([]byte(payload))
	_ = zw.Close()
	if base64.RawStdEncoding.EncodedLen(out.Len())+len(compressedContextPrefix) >= len(payload) {
		return payload
	}
	return compressedContextPrefix + base64.RawStdEncoding.EncodeToString(out.Bytes())
}
func decompressContextPayload(payload string) string {
	if !strings.HasPrefix(payload, compressedContextPrefix) {
		return payload
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(payload, compressedContextPrefix))
	if err != nil {
		return payload
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return payload
	}
	decoded, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		return payload
	}
	return string(decoded)
}
