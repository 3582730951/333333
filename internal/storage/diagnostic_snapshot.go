package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-sqlite3"
)

// DiagnosticSnapshot pins one read-only SQLite WAL snapshot. Writers continue to
// commit while every query issued through Store() observes the same boundary.
type DiagnosticSnapshot struct {
	id         string
	conn       *sql.Conn
	tx         *sql.Tx
	driver     string
	mu         sync.Mutex
	backupDB   *sql.DB
	backupPath string
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
	_ = conn.Close()
	return s.beginSQLiteDiagnosticBackup(ctx, pool)
}

func (s *Store) beginSQLiteDiagnosticBackup(ctx context.Context, pool *sql.DB) (*DiagnosticSnapshot, error) {
	tempDir := os.TempDir()
	databasePath := strings.SplitN(s.path, "?", 2)[0]
	if databasePath != "" && databasePath != ":memory:" && !strings.Contains(databasePath, "mode=memory") {
		if candidate := filepath.Dir(databasePath); candidate != "" {
			tempDir = candidate
		}
	}
	temp, err := os.CreateTemp(tempDir, ".diagnostic-snapshot-*.sqlite3")
	if err != nil {
		return nil, fmt.Errorf("create diagnostic SQLite backup: %w", err)
	}
	tempPath := temp.Name()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return nil, err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return nil, err
	}
	destination, err := sql.Open("sqlite3", tempPath+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		_ = os.Remove(tempPath)
		return nil, err
	}
	destination.SetMaxOpenConns(1)
	sourceConn, err := pool.Conn(ctx)
	if err != nil {
		_ = destination.Close()
		_ = os.Remove(tempPath)
		return nil, err
	}
	defer sourceConn.Close()
	destinationConn, err := destination.Conn(ctx)
	if err != nil {
		_ = destination.Close()
		_ = os.Remove(tempPath)
		return nil, err
	}
	backupErr := sourceConn.Raw(func(sourceDriver any) error {
		sourceSQLite, ok := sourceDriver.(*sqlite3.SQLiteConn)
		if !ok {
			return errors.New("unexpected SQLite source driver")
		}
		return destinationConn.Raw(func(destinationDriver any) error {
			destinationSQLite, ok := destinationDriver.(*sqlite3.SQLiteConn)
			if !ok {
				return errors.New("unexpected SQLite destination driver")
			}
			backup, err := destinationSQLite.Backup("main", sourceSQLite, "main")
			if err != nil {
				return err
			}
			defer backup.Finish()
			for {
				done, stepErr := backup.Step(256)
				if stepErr != nil {
					return stepErr
				}
				if done {
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Millisecond):
				}
			}
		})
	})
	_ = destinationConn.Close()
	if backupErr != nil {
		_ = destination.Close()
		_ = os.Remove(tempPath)
		return nil, fmt.Errorf("backup diagnostic SQLite snapshot: %w", backupErr)
	}
	if _, err := destination.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		_ = destination.Close()
		_ = os.Remove(tempPath)
		return nil, err
	}
	return &DiagnosticSnapshot{
		id: newDiagnosticSnapshotID(), driver: s.driver,
		backupDB: destination, backupPath: tempPath,
	}, nil
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
	if s == nil || source == nil {
		return nil, errors.New("diagnostic snapshot is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backupDB != nil {
		return &Store{
			path: s.backupPath, driver: source.driver, db: s.backupDB, rdb: s.backupDB,
			tokenKey: source.tokenKey, tokenKeys: source.tokenKeys, cryptoStrict: source.cryptoStrict,
		}, nil
	}
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
	if s.backupDB != nil {
		closeErr := s.backupDB.Close()
		s.backupDB = nil
		removeErr := os.Remove(s.backupPath)
		s.backupPath = ""
		return errors.Join(closeErr, removeErr)
	}
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
