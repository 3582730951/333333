package storage

import "context"

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
	_, err := s.db.ExecContext(ctx, `INSERT INTO context_journal(response_id,affinity_hash,account_id,encrypted_payload,created_at,expires_at) VALUES(?,?,?,?,?,?) ON CONFLICT(response_id) DO UPDATE SET affinity_hash=excluded.affinity_hash,account_id=excluded.account_id,encrypted_payload=excluded.encrypted_payload,created_at=excluded.created_at,expires_at=excluded.expires_at`, j.ResponseID, j.AffinityHash, j.AccountID, s.sealToken(j.Payload), j.CreatedAt, j.ExpiresAt)
	return err
}
func (s *Store) GetContextJournal(ctx context.Context, id string) (ContextJournal, error) {
	var j ContextJournal
	var p string
	err := s.rdb.QueryRowContext(ctx, `SELECT response_id,affinity_hash,account_id,encrypted_payload,created_at,expires_at FROM context_journal WHERE response_id=? AND expires_at>?`, id, Now()).Scan(&j.ResponseID, &j.AffinityHash, &j.AccountID, &p, &j.CreatedAt, &j.ExpiresAt)
	j.Payload = s.openToken(p)
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
	r, err := s.db.ExecContext(ctx, `DELETE FROM context_journal`)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
