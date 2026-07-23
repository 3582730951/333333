package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// InsertEmailAccount inserts a single email account into the pool.
func (s *Store) InsertEmailAccount(ctx context.Context, a EmailAccount) error {
	now := Now()
	if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	if a.UpdatedAt == 0 {
		a.UpdatedAt = now
	}
	if a.ID == "" {
		a.ID = fmt.Sprintf("email_%x", now)
	}
	if a.Status == "" {
		a.Status = "idle"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO email_pool(id, email, password, client_id, refresh_token, status, group_name, error_message, last_used_at, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Email, a.Password, a.ClientID, a.RefreshToken, a.Status, a.GroupName, a.ErrorMessage, a.LastUsedAt, a.CreatedAt, a.UpdatedAt)
	return err
}

// BulkInsertEmailAccounts imports multiple email accounts, skipping duplicates.
// Returns the number of newly inserted rows.
func (s *Store) BulkInsertEmailAccounts(ctx context.Context, accounts []EmailAccount) (int, error) {
	now := Now()
	inserted := 0
	for i := range accounts {
		if accounts[i].ID == "" {
			accounts[i].ID = fmt.Sprintf("email_%s_%d", strings.ReplaceAll(accounts[i].Email, "@", "_"), now)
		}
		if accounts[i].Status == "" {
			accounts[i].Status = "idle"
		}
		if accounts[i].CreatedAt == 0 {
			accounts[i].CreatedAt = now
		}
		if accounts[i].UpdatedAt == 0 {
			accounts[i].UpdatedAt = now
		}
		_, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO email_pool(id, email, password, client_id, refresh_token, status, group_name, error_message, last_used_at, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			accounts[i].ID, accounts[i].Email, accounts[i].Password, accounts[i].ClientID, accounts[i].RefreshToken,
			accounts[i].Status, accounts[i].GroupName, accounts[i].ErrorMessage, accounts[i].LastUsedAt,
			accounts[i].CreatedAt, accounts[i].UpdatedAt)
		if err != nil {
			return inserted, err
		}
		inserted++
	}
	return inserted, nil
}

// ListEmailAccounts returns a paginated list of email accounts.
func (s *Store) ListEmailAccounts(ctx context.Context, page, pageSize int, search, status string) ([]EmailAccount, int, error) {
	where := "1=1"
	args := []interface{}{}
	if search != "" {
		where += " AND email LIKE ?"
		args = append(args, "%"+search+"%")
	}
	if status != "" {
		where += " AND status = ?"
		args = append(args, status)
	}

	var total int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM email_pool WHERE %s", where)
	if err := s.rdb.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	offset := (page - 1) * pageSize
	query := fmt.Sprintf(
		"SELECT id, email, password, client_id, refresh_token, status, group_name, error_message, last_used_at, created_at, updated_at FROM email_pool WHERE %s ORDER BY created_at DESC LIMIT ? OFFSET ?",
		where)
	rows, err := s.rdb.QueryContext(ctx, query, append(args, pageSize, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []EmailAccount
	for rows.Next() {
		var a EmailAccount
		if err := rows.Scan(&a.ID, &a.Email, &a.Password, &a.ClientID, &a.RefreshToken,
			&a.Status, &a.GroupName, &a.ErrorMessage, &a.LastUsedAt, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// GetEmailAccount looks up a single email account by ID.
func (s *Store) GetEmailAccount(ctx context.Context, id string) (EmailAccount, bool, error) {
	var a EmailAccount
	err := s.rdb.QueryRowContext(ctx,
		`SELECT id, email, password, client_id, refresh_token, status, group_name, error_message, last_used_at, created_at, updated_at FROM email_pool WHERE id = ?`,
		id).Scan(&a.ID, &a.Email, &a.Password, &a.ClientID, &a.RefreshToken,
		&a.Status, &a.GroupName, &a.ErrorMessage, &a.LastUsedAt, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return EmailAccount{}, false, nil
	}
	if err != nil {
		return EmailAccount{}, false, err
	}
	return a, true, nil
}

// UpdateEmailAccount updates an email account's mutable fields.
func (s *Store) UpdateEmailAccount(ctx context.Context, a EmailAccount) error {
	a.UpdatedAt = Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE email_pool SET password=?, client_id=?, refresh_token=?, status=?, group_name=?, error_message=?, last_used_at=?, updated_at=? WHERE id=?`,
		a.Password, a.ClientID, a.RefreshToken, a.Status, a.GroupName, a.ErrorMessage, a.LastUsedAt, a.UpdatedAt, a.ID)
	return err
}

// DeleteEmailAccount removes an email account from the pool by ID.
func (s *Store) DeleteEmailAccount(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM email_pool WHERE id = ?`, id)
	return err
}

// ReserveEmailAccount atomically picks an idle email account and marks it as in_use.
// Returns sql.ErrNoRows if no idle accounts are available.
func (s *Store) ReserveEmailAccount(ctx context.Context, groupName string) (EmailAccount, error) {
	var a EmailAccount
	query := `SELECT id, email, password, client_id, refresh_token, status, group_name, error_message, last_used_at, created_at, updated_at FROM email_pool WHERE status = 'idle'`
	args := []interface{}{}
	if groupName != "" {
		query += " AND group_name = ?"
		args = append(args, groupName)
	}
	query += " ORDER BY last_used_at ASC LIMIT 1"
	if err := s.rdb.QueryRowContext(ctx, query, args...).Scan(
		&a.ID, &a.Email, &a.Password, &a.ClientID, &a.RefreshToken,
		&a.Status, &a.GroupName, &a.ErrorMessage, &a.LastUsedAt, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return EmailAccount{}, err
	}
	now := Now()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE email_pool SET status = 'in_use', last_used_at = ?, updated_at = ? WHERE id = ? AND status = 'idle'`,
		now, now, a.ID); err != nil {
		return EmailAccount{}, err
	}
	a.Status = "in_use"
	a.LastUsedAt = now
	return a, nil
}

// ReleaseEmailAccount sets an email account's status back after a registration attempt.
func (s *Store) ReleaseEmailAccount(ctx context.Context, id, status, errMsg string) error {
	now := Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE email_pool SET status = ?, error_message = ?, updated_at = ? WHERE id = ?`,
		status, errMsg, now, id)
	return err
}

// CountEmailAccountsByStatus returns counts grouped by status for the summary bar.
func (s *Store) CountEmailAccountsByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT status, COUNT(*) FROM email_pool GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, rows.Err()
}
