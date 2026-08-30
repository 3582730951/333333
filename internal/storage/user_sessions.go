package storage

import (
	"context"
	"strings"
)

func (s *Store) ListUserSessions(ctx context.Context, userID string) ([]UserSession, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT token_hash,user_id,COALESCE(user_agent,''),created_at,expires_at
FROM user_sessions WHERE user_id=? AND (expires_at=0 OR expires_at>=?) ORDER BY created_at DESC`, strings.TrimSpace(userID), Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]UserSession, 0)
	for rows.Next() {
		var session UserSession
		if err := rows.Scan(&session.TokenHash, &session.UserID, &session.UserAgent, &session.CreatedAt, &session.ExpiresAt); err != nil {
			return nil, err
		}
		result = append(result, session)
	}
	return result, rows.Err()
}

func (s *Store) DeleteUserSessionOwned(ctx context.Context, tokenHash, userID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE token_hash=? AND user_id=?`, strings.TrimSpace(tokenHash), strings.TrimSpace(userID))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
