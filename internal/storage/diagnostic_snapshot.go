package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DiagnosticSnapshot pins one read-only SQLite WAL snapshot. Writers continue to
// commit while every query issued through Store() observes the same boundary.
type DiagnosticSnapshot struct {
	id     string
	conn   *sql.Conn
	tx     *sql.Tx
	driver string
	mu     sync.Mutex
}

func (s *Store) BeginDiagnosticSnapshot(ctx context.Context) (*DiagnosticSnapshot, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("diagnostic snapshot store is unavailable")
	}
	pool, ok := s.rdb.(*sql.DB)
	if !ok {
		return nil, errors.New("diagnostic snapshot cannot be nested")
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if s.driver == "postgres" {
		tx, beginErr := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if beginErr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("begin diagnostic PostgreSQL snapshot: %w", beginErr)
		}
		var established int
		if beginErr = tx.QueryRowContext(ctx, "SELECT 1").Scan(&established); beginErr != nil {
			_ = tx.Rollback()
			_ = conn.Close()
			return nil, fmt.Errorf("establish diagnostic PostgreSQL snapshot: %w", beginErr)
		}
		return &DiagnosticSnapshot{id: newDiagnosticSnapshotID(), conn: conn, tx: tx, driver: s.driver}, nil
	}
	if _, err = conn.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("make diagnostic SQLite connection read-only: %w", err)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_, _ = conn.ExecContext(context.Background(), "PRAGMA query_only=OFF")
		_ = conn.Close()
		return nil, fmt.Errorf("begin diagnostic SQLite snapshot: %w", err)
	}
	// A deferred SQLite transaction chooses its WAL boundary on the first read.
	// Force that read here so later writer commits cannot move the export boundary.
	var schemaRows int64
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master").Scan(&schemaRows); err != nil {
		_ = tx.Rollback()
		_, _ = conn.ExecContext(context.Background(), "PRAGMA query_only=OFF")
		_ = conn.Close()
		return nil, fmt.Errorf("establish diagnostic SQLite snapshot: %w", err)
	}
	return &DiagnosticSnapshot{id: newDiagnosticSnapshotID(), conn: conn, tx: tx, driver: s.driver}, nil
}

func (s *DiagnosticSnapshot) ID() string {
	if s == nil {
		return ""
	}
	return s.id
}

// Store returns a read-only view whose normal list methods are bound to the
// snapshot transaction. The view does not own the source store or transaction.
func (s *DiagnosticSnapshot) Store(source *Store) (*Store, error) {
	if s == nil || source == nil || s.tx == nil {
		return nil, errors.New("diagnostic snapshot is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx == nil {
		return nil, errors.New("diagnostic snapshot is closed")
	}
	return &Store{path: source.path, driver: source.driver, db: source.db, rdb: s.tx, tokenKey: source.tokenKey}, nil
}

func (s *DiagnosticSnapshot) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx == nil || s.conn == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	commitErr := s.tx.Commit()
	s.tx = nil
	var resetErr error
	if s.driver != "postgres" {
		_, resetErr = s.conn.ExecContext(ctx, "PRAGMA query_only=OFF")
	}
	closeErr := s.conn.Close()
	s.conn = nil
	return errors.Join(commitErr, resetErr, closeErr)
}

func newDiagnosticSnapshotID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "diag_" + hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("diag_%x", time.Now().UnixNano())
}
