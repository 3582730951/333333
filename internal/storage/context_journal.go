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
	_, _ = s.db.ExecContext(ctx, `PRAGMA secure_delete=OFF`)
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
		if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			return total, err
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
