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
)

// DiagnosticSnapshot pins one read-only database snapshot. SQLite readers use a
// WAL transaction on a dedicated read-pool connection, so writers continue to
// commit while every query issued through Store() observes the same boundary.
//
// Do not materialize the whole SQLite database into a temporary backup here. A
// busy source database causes sqlite3_backup_step to restart after concurrent
// writes; on multi-gigabyte production databases that turned a small diagnostics
// export into an unbounded, CPU-heavy copy and left jobs in "snapshotting".
type DiagnosticSnapshot struct {
	id     string
	conn   *sql.Conn
	tx     *sql.Tx
	driver string
	mu     sync.Mutex
}

// DiagnosticWALPath returns the local SQLite WAL watched while a logical
// diagnostic snapshot is open. PostgreSQL and in-memory stores have no local WAL.
func (s *Store) DiagnosticWALPath() string {
	if s == nil || s.driver != "sqlite" || s.InMemory() {
		return ""
	}
	path := strings.SplitN(strings.TrimSpace(s.path), "?", 2)[0]
	if path == "" {
		return ""
	}
	return path + "-wal"
}

// TryTruncateDiagnosticWAL makes one non-destructive attempt to checkpoint and
// truncate the local SQLite WAL. A live reader can legitimately keep frames
// pinned; that condition is returned as completed=false rather than an error so
// the diagnostics maintenance loop can retry after the reader releases its
// snapshot. The caller owns the timeout policy.
func (s *Store) TryTruncateDiagnosticWAL(ctx context.Context) (completed bool, err error) {
	if s == nil || s.db == nil {
		return false, errors.New("diagnostic WAL store is unavailable")
	}
	if s.DiagnosticWALPath() == "" {
		return true, nil
	}
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(
		&busy, &logFrames, &checkpointed,
	); err != nil {
		return false, err
	}
	return busy == 0, nil
}

// CleanupLegacyDiagnosticSnapshots removes physical SQLite copies left by the
// pre-v3 snapshot implementation after a crash or worker handoff. Only regular
// files with the exact private prefix in the database directory are eligible;
// symlinks and directories are left untouched.
func (s *Store) CleanupLegacyDiagnosticSnapshots() (int, error) {
	if s == nil || s.driver != "sqlite" || s.InMemory() {
		return 0, nil
	}
	databasePath := strings.SplitN(strings.TrimSpace(s.path), "?", 2)[0]
	if databasePath == "" {
		return 0, nil
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(databasePath), ".diagnostic-snapshot-*"))
	if err != nil {
		return 0, err
	}
	type legacyFile struct {
		path string
		info os.FileInfo
	}
	type legacyFamily struct {
		files  []legacyFile
		unsafe bool
	}
	families := make(map[string]*legacyFamily)
	var cleanupErr error
	for _, match := range matches {
		stem, valid := legacyDiagnosticSnapshotStem(filepath.Base(match))
		if !valid {
			continue
		}
		family := families[stem]
		if family == nil {
			family = &legacyFamily{}
			families[stem] = family
		}
		info, statErr := os.Lstat(match)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			continue
		case statErr != nil:
			family.unsafe = true
			cleanupErr = errors.Join(cleanupErr, statErr)
		case !info.Mode().IsRegular():
			// A symlink, directory, or device using a snapshot-family name makes
			// the entire family ineligible.
			family.unsafe = true
		default:
			family.files = append(family.files, legacyFile{path: match, info: info})
		}
	}
	removed := 0
	for _, family := range families {
		if family.unsafe || len(family.files) == 0 {
			continue
		}
		for _, file := range family.files {
			open, openErr := diagnosticFileOpenBySameUID(file.path)
			if openErr != nil {
				cleanupErr = errors.Join(cleanupErr, openErr)
				family.unsafe = true
				break
			}
			if open {
				family.unsafe = true
				break
			}
		}
		if family.unsafe {
			continue
		}
		// Recheck the complete family after scanning /proc. If any member was
		// replaced, keep every member; partially deleting a live SQLite family can
		// corrupt the draining worker's snapshot.
		for _, file := range family.files {
			current, currentErr := os.Lstat(file.path)
			if currentErr != nil {
				if !errors.Is(currentErr, os.ErrNotExist) {
					cleanupErr = errors.Join(cleanupErr, currentErr)
				}
				family.unsafe = true
				break
			}
			if !current.Mode().IsRegular() || !os.SameFile(file.info, current) {
				family.unsafe = true
				break
			}
		}
		if family.unsafe {
			continue
		}
		for _, file := range family.files {
			if removeErr := os.Remove(file.path); removeErr != nil {
				if !errors.Is(removeErr, os.ErrNotExist) {
					cleanupErr = errors.Join(cleanupErr, removeErr)
				}
				continue
			}
			removed++
		}
	}
	return removed, cleanupErr
}

func legacyDiagnosticSnapshotStem(name string) (string, bool) {
	if !strings.HasPrefix(name, ".diagnostic-snapshot-") {
		return "", false
	}
	for _, suffix := range []string{".sqlite3-journal", ".sqlite3-wal", ".sqlite3-shm", ".sqlite3"} {
		stem := strings.TrimSuffix(name, suffix)
		if stem != name && len(stem) > len(".diagnostic-snapshot-") {
			return stem, true
		}
	}
	return "", false
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
	if _, err = conn.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("enable diagnostic SQLite query-only mode: %w", err)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		resetSQLiteDiagnosticConnection(conn)
		return nil, fmt.Errorf("begin diagnostic SQLite snapshot: %w", err)
	}
	// BEGIN is deferred in SQLite. Execute a read now so the snapshot boundary is
	// fixed before request-time writers can commit additional rows.
	var established int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master`).Scan(&established); err != nil {
		_ = tx.Rollback()
		resetSQLiteDiagnosticConnection(conn)
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
	if s == nil || source == nil {
		return nil, errors.New("diagnostic snapshot is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx == nil {
		return nil, errors.New("diagnostic snapshot is closed")
	}
	return &Store{
		path: source.path, driver: source.driver, db: source.db, rdb: s.tx,
		tokenKey: source.tokenKey, tokenKeys: source.tokenKeys, cryptoStrict: source.cryptoStrict,
	}, nil
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
	if errors.Is(commitErr, sql.ErrTxDone) ||
		errors.Is(commitErr, context.Canceled) ||
		errors.Is(commitErr, context.DeadlineExceeded) {
		// database/sql rolls a context-bound transaction back asynchronously when
		// its context is cancelled. Closing the snapshot remains idempotent.
		commitErr = nil
	}
	s.tx = nil
	var resetErr error
	if s.driver != "postgres" {
		_, resetErr = s.conn.ExecContext(ctx, "PRAGMA query_only=OFF")
		if errors.Is(resetErr, sql.ErrConnDone) {
			resetErr = nil
		}
	}
	closeErr := s.conn.Close()
	if errors.Is(closeErr, sql.ErrConnDone) {
		// A transaction bound to a cancelled context may already have returned
		// its dedicated connection. The reader and WAL pin are released in that state.
		closeErr = nil
	}
	s.conn = nil
	return errors.Join(commitErr, resetErr, closeErr)
}

func resetSQLiteDiagnosticConnection(conn *sql.Conn) {
	if conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = conn.ExecContext(ctx, `PRAGMA query_only=OFF`)
	cancel()
	_ = conn.Close()
}

func newDiagnosticSnapshotID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "diag_" + hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("diag_%x", time.Now().UnixNano())
}
