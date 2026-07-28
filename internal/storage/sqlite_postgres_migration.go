package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/config"

	"github.com/jackc/pgx/v5"
)

type SQLitePostgresMigrationOptions struct {
	SQLitePath    string
	PostgresDSN   string
	ReplaceTarget bool
	Progress      func(table string, rows int64)
}

type SQLitePostgresMigrationTable struct {
	Name   string
	Rows   int64
	Digest string
}

type SQLitePostgresMigrationResult struct {
	Tables   []SQLitePostgresMigrationTable
	Rows     int64
	Duration time.Duration
}

type migrationTable struct {
	name         string
	columns      []string
	dependencies []string
}

// MigrateSQLiteToPostgres freezes SQLite writes, streams every row through
// PostgreSQL CopyFrom, and commits only after per-table row counts and digests match.
func MigrateSQLiteToPostgres(ctx context.Context, options SQLitePostgresMigrationOptions) (result SQLitePostgresMigrationResult, err error) {
	started := time.Now()
	sqlitePath := strings.TrimSpace(options.SQLitePath)
	dsn := strings.TrimSpace(options.PostgresDSN)
	if sqlitePath == "" || dsn == "" {
		return result, errors.New("SQLite path and PostgreSQL DSN are required")
	}
	absolutePath, err := filepath.Abs(sqlitePath)
	if err != nil {
		return result, fmt.Errorf("resolve SQLite path: %w", err)
	}
	info, err := os.Stat(absolutePath)
	if err != nil {
		return result, fmt.Errorf("open SQLite source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("SQLite source %s is not a regular file", absolutePath)
	}

	targetConfig := config.Default()
	targetConfig.StorageDriver = "postgres"
	targetConfig.PostgresDSN = dsn
	targetConfig.RedisURL = "redis://migration-validation.invalid:6379/0"
	target, err := OpenWithConfig(targetConfig)
	if err != nil {
		return result, fmt.Errorf("open PostgreSQL target: %w", err)
	}
	if err = target.Init(ctx); err != nil {
		_ = target.Close()
		return result, fmt.Errorf("initialize PostgreSQL target: %w", err)
	}
	if err = target.Close(); err != nil {
		return result, fmt.Errorf("close PostgreSQL initializer: %w", err)
	}

	sourceDSN := absolutePath + "?_busy_timeout=30000&_foreign_keys=on"
	source, err := sql.Open("sqlite3", sourceDSN)
	if err != nil {
		return result, fmt.Errorf("open SQLite source: %w", err)
	}
	defer source.Close()
	source.SetMaxOpenConns(1)
	sourceConn, err := source.Conn(ctx)
	if err != nil {
		return result, fmt.Errorf("connect SQLite source: %w", err)
	}
	defer sourceConn.Close()
	if _, err = sourceConn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return result, fmt.Errorf("freeze SQLite writes (stop the running pool server first): %w", err)
	}
	sourceOpen := true
	defer func() {
		if sourceOpen {
			_, _ = sourceConn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if err = validateSQLiteSource(ctx, sourceConn); err != nil {
		return result, err
	}
	tables, err := sqliteMigrationTables(ctx, sourceConn)
	if err != nil {
		return result, err
	}

	targetConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return result, fmt.Errorf("connect PostgreSQL target: %w", err)
	}
	defer targetConn.Close(context.Background())
	tx, err := targetConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return result, fmt.Errorf("begin PostgreSQL migration: %w", err)
	}
	targetOpen := true
	defer func() {
		if targetOpen {
			_ = tx.Rollback(context.Background())
		}
	}()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('codex-account-pool-sqlite-migration'))`); err != nil {
		return result, fmt.Errorf("lock PostgreSQL migration: %w", err)
	}
	if err = validatePostgresMigrationSchema(ctx, tx, tables); err != nil {
		return result, err
	}
	if !options.ReplaceTarget {
		occupied, checkErr := postgresOccupiedMigrationTables(ctx, tx, tables)
		if checkErr != nil {
			return result, checkErr
		}
		if len(occupied) > 0 {
			return result, fmt.Errorf("PostgreSQL target contains rows in %s; rerun with explicit replace-target authorization", strings.Join(occupied, ", "))
		}
	} else if err = truncatePostgresMigrationTarget(ctx, tx); err != nil {
		return result, err
	}

	result.Tables = make([]SQLitePostgresMigrationTable, 0, len(tables))
	for _, table := range tables {
		sourceDigest, copied, copyErr := copySQLiteTable(ctx, sourceConn, tx, table)
		if copyErr != nil {
			return result, copyErr
		}
		targetDigest, verifyErr := digestPostgresTable(ctx, tx, table)
		if verifyErr != nil {
			return result, verifyErr
		}
		if copied != targetDigest.count || sourceDigest != targetDigest {
			return result, fmt.Errorf("verify table %s: source rows=%d digest=%s target rows=%d digest=%s", table.name, copied, sourceDigest.String(), targetDigest.count, targetDigest.String())
		}
		result.Tables = append(result.Tables, SQLitePostgresMigrationTable{Name: table.name, Rows: copied, Digest: sourceDigest.String()})
		result.Rows += copied
		if options.Progress != nil {
			options.Progress(table.name, copied)
		}
	}
	if err = resetPostgresSequences(ctx, tx); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit PostgreSQL migration: %w", err)
	}
	targetOpen = false
	if _, err = sourceConn.ExecContext(ctx, `COMMIT`); err != nil {
		return result, fmt.Errorf("release SQLite maintenance snapshot: %w", err)
	}
	sourceOpen = false
	result.Duration = time.Since(started)
	return result, nil
}

func validateSQLiteSource(ctx context.Context, conn *sql.Conn) error {
	var quickCheck string
	if err := conn.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&quickCheck); err != nil {
		return fmt.Errorf("SQLite quick_check: %w", err)
	}
	if quickCheck != "ok" {
		return fmt.Errorf("SQLite quick_check failed: %s", quickCheck)
	}
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("SQLite foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("SQLite foreign_key_check found an invalid relationship")
	}
	return rows.Err()
}

func sqliteMigrationTables(ctx context.Context, conn *sql.Conn) ([]migrationTable, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name<>'schema_migrations' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list SQLite tables: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(names))
	for _, name := range names {
		known[name] = true
	}
	tables := make([]migrationTable, 0, len(names))
	for _, name := range names {
		columns, columnErr := sqliteTableColumns(ctx, conn, name)
		if columnErr != nil {
			return nil, columnErr
		}
		dependencies, dependencyErr := sqliteTableDependencies(ctx, conn, name, known)
		if dependencyErr != nil {
			return nil, dependencyErr
		}
		tables = append(tables, migrationTable{name: name, columns: columns, dependencies: dependencies})
	}
	return orderMigrationTables(tables)
}

func sqliteTableColumns(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(`+sqliteIdentifier(table)+`)`)
	if err != nil {
		return nil, fmt.Errorf("inspect SQLite table %s: %w", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var position, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err = rows.Scan(&position, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("SQLite table %s has no columns", table)
	}
	return columns, nil
}

func sqliteTableDependencies(ctx context.Context, conn *sql.Conn, table string, known map[string]bool) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_list(`+sqliteIdentifier(table)+`)`)
	if err != nil {
		return nil, fmt.Errorf("inspect SQLite foreign keys for %s: %w", table, err)
	}
	defer rows.Close()
	set := make(map[string]bool)
	for rows.Next() {
		var id, sequence int
		var parent, from, to, onUpdate, onDelete, match string
		if err = rows.Scan(&id, &sequence, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return nil, err
		}
		if parent != table && known[parent] {
			set[parent] = true
		}
	}
	dependencies := make([]string, 0, len(set))
	for dependency := range set {
		dependencies = append(dependencies, dependency)
	}
	sort.Strings(dependencies)
	return dependencies, rows.Err()
}

func orderMigrationTables(tables []migrationTable) ([]migrationTable, error) {
	remaining := make(map[string]migrationTable, len(tables))
	for _, table := range tables {
		remaining[table.name] = table
	}
	ordered := make([]migrationTable, 0, len(tables))
	for len(remaining) > 0 {
		ready := make([]string, 0)
		for name, table := range remaining {
			blocked := false
			for _, dependency := range table.dependencies {
				if _, exists := remaining[dependency]; exists {
					blocked = true
					break
				}
			}
			if !blocked {
				ready = append(ready, name)
			}
		}
		if len(ready) == 0 {
			names := make([]string, 0, len(remaining))
			for name := range remaining {
				names = append(names, name)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("cyclic SQLite foreign keys prevent ordered migration: %s", strings.Join(names, ", "))
		}
		sort.Strings(ready)
		for _, name := range ready {
			ordered = append(ordered, remaining[name])
			delete(remaining, name)
		}
	}
	return ordered, nil
}

func validatePostgresMigrationSchema(ctx context.Context, tx pgx.Tx, tables []migrationTable) error {
	for _, table := range tables {
		rows, err := tx.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema='public' AND table_name=$1`, table.name)
		if err != nil {
			return fmt.Errorf("inspect PostgreSQL table %s: %w", table.name, err)
		}
		targetColumns := make(map[string]bool)
		for rows.Next() {
			var column string
			if err = rows.Scan(&column); err != nil {
				rows.Close()
				return err
			}
			targetColumns[column] = true
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(targetColumns) == 0 {
			return fmt.Errorf("PostgreSQL target is missing table %s", table.name)
		}
		for _, column := range table.columns {
			if !targetColumns[column] {
				return fmt.Errorf("PostgreSQL table %s is missing source column %s", table.name, column)
			}
		}
	}
	return nil
}

func postgresOccupiedMigrationTables(ctx context.Context, tx pgx.Tx, tables []migrationTable) ([]string, error) {
	var occupied []string
	for _, table := range tables {
		var exists bool
		query := `SELECT EXISTS(SELECT 1 FROM ` + pgx.Identifier{table.name}.Sanitize() + ` LIMIT 1)`
		if err := tx.QueryRow(ctx, query).Scan(&exists); err != nil {
			return nil, fmt.Errorf("inspect PostgreSQL table %s: %w", table.name, err)
		}
		if exists {
			occupied = append(occupied, table.name)
		}
	}
	return occupied, nil
}

func truncatePostgresMigrationTarget(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT tablename FROM pg_catalog.pg_tables WHERE schemaname='public' AND tablename<>'schema_migrations' ORDER BY tablename`)
	if err != nil {
		return fmt.Errorf("list PostgreSQL target tables: %w", err)
	}
	var identifiers []string
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			rows.Close()
			return err
		}
		identifiers = append(identifiers, pgx.Identifier{table}.Sanitize())
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(identifiers) == 0 {
		return errors.New("PostgreSQL target contains no migratable tables")
	}
	if _, err = tx.Exec(ctx, `TRUNCATE TABLE `+strings.Join(identifiers, ",")+` RESTART IDENTITY CASCADE`); err != nil {
		return fmt.Errorf("clear PostgreSQL migration target: %w", err)
	}
	return nil
}

type sqliteCopySource struct {
	rows   *sql.Rows
	values []any
	scan   []any
	digest migrationDigest
	err    error
}

func newSQLiteCopySource(rows *sql.Rows, columns int) *sqliteCopySource {
	source := &sqliteCopySource{rows: rows, values: make([]any, columns), scan: make([]any, columns)}
	for index := range source.values {
		source.scan[index] = &source.values[index]
	}
	return source
}

func (s *sqliteCopySource) Next() bool {
	if s.err != nil || !s.rows.Next() {
		return false
	}
	for index := range s.values {
		s.values[index] = nil
	}
	if s.err = s.rows.Scan(s.scan...); s.err != nil {
		return false
	}
	s.err = s.digest.Add(s.values)
	return s.err == nil
}

func (s *sqliteCopySource) Values() ([]any, error) { return s.values, s.err }

func (s *sqliteCopySource) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.rows.Err()
}

func copySQLiteTable(ctx context.Context, source *sql.Conn, target pgx.Tx, table migrationTable) (migrationDigest, int64, error) {
	columnList := make([]string, len(table.columns))
	for index, column := range table.columns {
		columnList[index] = sqliteIdentifier(column)
	}
	rows, err := source.QueryContext(ctx, `SELECT `+strings.Join(columnList, ",")+` FROM `+sqliteIdentifier(table.name))
	if err != nil {
		return migrationDigest{}, 0, fmt.Errorf("read SQLite table %s: %w", table.name, err)
	}
	copySource := newSQLiteCopySource(rows, len(table.columns))
	copied, err := target.CopyFrom(ctx, pgx.Identifier{table.name}, table.columns, copySource)
	closeErr := rows.Close()
	if err == nil {
		err = copySource.Err()
	}
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return migrationDigest{}, copied, fmt.Errorf("copy SQLite table %s: %w", table.name, err)
	}
	if copied != copySource.digest.count {
		return migrationDigest{}, copied, fmt.Errorf("copy SQLite table %s counted %d rows but CopyFrom accepted %d", table.name, copySource.digest.count, copied)
	}
	return copySource.digest, copied, nil
}

func digestPostgresTable(ctx context.Context, tx pgx.Tx, table migrationTable) (migrationDigest, error) {
	columns := make([]string, len(table.columns))
	for index, column := range table.columns {
		columns[index] = pgx.Identifier{column}.Sanitize()
	}
	rows, err := tx.Query(ctx, `SELECT `+strings.Join(columns, ",")+` FROM `+pgx.Identifier{table.name}.Sanitize())
	if err != nil {
		return migrationDigest{}, fmt.Errorf("verify PostgreSQL table %s: %w", table.name, err)
	}
	defer rows.Close()
	var digest migrationDigest
	for rows.Next() {
		values, valuesErr := rows.Values()
		if valuesErr != nil {
			return migrationDigest{}, valuesErr
		}
		if valuesErr = digest.Add(values); valuesErr != nil {
			return migrationDigest{}, fmt.Errorf("digest PostgreSQL table %s: %w", table.name, valuesErr)
		}
	}
	return digest, rows.Err()
}

func resetPostgresSequences(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT table_name,column_name FROM information_schema.columns WHERE table_schema='public' AND column_default LIKE 'nextval(%' ORDER BY table_name,column_name`)
	if err != nil {
		return fmt.Errorf("list PostgreSQL sequences: %w", err)
	}
	type serialColumn struct{ table, column string }
	var columns []serialColumn
	for rows.Next() {
		var column serialColumn
		if err = rows.Scan(&column.table, &column.column); err != nil {
			rows.Close()
			return err
		}
		columns = append(columns, column)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, column := range columns {
		var sequence *string
		if err = tx.QueryRow(ctx, `SELECT pg_get_serial_sequence($1,$2)`, "public."+column.table, column.column).Scan(&sequence); err != nil {
			return fmt.Errorf("resolve PostgreSQL sequence for %s.%s: %w", column.table, column.column, err)
		}
		if sequence == nil || *sequence == "" {
			continue
		}
		query := `SELECT setval($1::regclass,COALESCE(MAX(` + pgx.Identifier{column.column}.Sanitize() + `),1),COUNT(*)>0) FROM ` + pgx.Identifier{column.table}.Sanitize()
		if _, err = tx.Exec(ctx, query, *sequence); err != nil {
			return fmt.Errorf("reset PostgreSQL sequence for %s.%s: %w", column.table, column.column, err)
		}
	}
	return nil
}

type migrationDigest struct {
	count int64
	sum   [32]byte
	sum2  [32]byte
}

func (d *migrationDigest) Add(values []any) error {
	hash := sha256.New()
	for _, value := range values {
		if err := writeCanonicalMigrationValue(hash, value); err != nil {
			return err
		}
	}
	var row [32]byte
	copy(row[:], hash.Sum(nil))
	second := sha256.Sum256(append([]byte{0xa5}, row[:]...))
	addMigrationDigest(&d.sum, row)
	addMigrationDigest(&d.sum2, second)
	d.count++
	return nil
}

func (d migrationDigest) String() string {
	raw := make([]byte, 0, len(d.sum)+len(d.sum2))
	raw = append(raw, d.sum[:]...)
	raw = append(raw, d.sum2[:]...)
	final := sha256.Sum256(raw)
	return hex.EncodeToString(final[:])
}

func writeCanonicalMigrationValue(target interface{ Write([]byte) (int, error) }, value any) error {
	var scratch [9]byte
	write := func(tag byte, payload []byte) error {
		scratch[0] = tag
		binary.BigEndian.PutUint64(scratch[1:], uint64(len(payload)))
		if _, err := target.Write(scratch[:]); err != nil {
			return err
		}
		_, err := target.Write(payload)
		return err
	}
	switch typed := value.(type) {
	case nil:
		return write(0, nil)
	case int:
		return writeCanonicalMigrationValue(target, int64(typed))
	case int8:
		return writeCanonicalMigrationValue(target, int64(typed))
	case int16:
		return writeCanonicalMigrationValue(target, int64(typed))
	case int32:
		return writeCanonicalMigrationValue(target, int64(typed))
	case int64:
		var payload [8]byte
		binary.BigEndian.PutUint64(payload[:], uint64(typed))
		return write(1, payload[:])
	case uint:
		return writeCanonicalMigrationValue(target, uint64(typed))
	case uint8:
		return writeCanonicalMigrationValue(target, uint64(typed))
	case uint16:
		return writeCanonicalMigrationValue(target, uint64(typed))
	case uint32:
		return writeCanonicalMigrationValue(target, uint64(typed))
	case uint64:
		var payload [8]byte
		binary.BigEndian.PutUint64(payload[:], typed)
		return write(2, payload[:])
	case float32:
		return writeCanonicalMigrationValue(target, float64(typed))
	case float64:
		var payload [8]byte
		binary.BigEndian.PutUint64(payload[:], math.Float64bits(typed))
		return write(3, payload[:])
	case string:
		return write(4, []byte(typed))
	case []byte:
		return write(5, typed)
	case bool:
		if typed {
			return write(6, []byte{1})
		}
		return write(6, []byte{0})
	case time.Time:
		return write(7, []byte(typed.UTC().Format(time.RFC3339Nano)))
	default:
		return fmt.Errorf("unsupported migration value %T (%s)", value, strconv.Quote(fmt.Sprint(value)))
	}
}

func addMigrationDigest(target *[32]byte, value [32]byte) {
	carry := uint16(0)
	for index := len(target) - 1; index >= 0; index-- {
		sum := uint16(target[index]) + uint16(value[index]) + carry
		target[index] = byte(sum)
		carry = sum >> 8
	}
}

func sqliteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
