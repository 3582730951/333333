package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/config"

	"github.com/jackc/pgx/v5/stdlib"
)

const postgresDriverName = "codex-pgx-rebind"

var registerPostgresDriver sync.Once

func ensurePostgresDriver() {
	registerPostgresDriver.Do(func() {
		sql.Register(postgresDriverName, rebindDriver{underlying: stdlib.GetDefaultDriver()})
	})
}

// OpenWithConfig keeps Open(path) as the SQLite-compatible API while making the
// production backend choice explicit and validated at startup.
func OpenWithConfig(cfg config.Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(cfg.StorageDriver), "postgres") {
		return openPostgres(cfg.PostgresDSN)
	}
	return Open(cfg.DatabasePath)
}

func openPostgres(dsn string) (*Store, error) {
	ensurePostgresDriver()
	db, err := sql.Open(postgresDriverName, strings.TrimSpace(dsn))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(64)
	db.SetMaxIdleConns(16)
	db.SetConnMaxIdleTime(90 * time.Second)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	return &Store{path: strings.TrimSpace(dsn), driver: "postgres", db: db, rdb: db}, nil
}

type rebindDriver struct{ underlying driver.Driver }

func (d rebindDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.underlying.Open(name)
	if err != nil {
		return nil, err
	}
	return rebindConn{Conn: conn}, nil
}

type rebindConn struct{ driver.Conn }

func (c rebindConn) Prepare(query string) (driver.Stmt, error) {
	return c.Conn.Prepare(rebindPostgres(query))
}

func (c rebindConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if prepared, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return prepared.PrepareContext(ctx, rebindPostgres(query))
	}
	return c.Prepare(query)
}

func (c rebindConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if execer, ok := c.Conn.(driver.ExecerContext); ok {
		return execer.ExecContext(ctx, rebindPostgres(query), args)
	}
	return nil, driver.ErrSkip
}

func (c rebindConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if queryer, ok := c.Conn.(driver.QueryerContext); ok {
		return queryer.QueryContext(ctx, rebindPostgres(query), args)
	}
	return nil, driver.ErrSkip
}

func (c rebindConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

func (c rebindConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c rebindConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

func (c rebindConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c rebindConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func rebindPostgres(query string) string {
	query = postgresCompatQuery(query)
	var out strings.Builder
	out.Grow(len(query) + 16)
	parameter := 1
	for index := 0; index < len(query); {
		switch query[index] {
		case '\'':
			next := copySQLQuoted(&out, query, index, '\'')
			index = next
		case '"':
			next := copySQLQuoted(&out, query, index, '"')
			index = next
		case '-':
			if index+1 < len(query) && query[index+1] == '-' {
				end := strings.IndexByte(query[index:], '\n')
				if end < 0 {
					out.WriteString(query[index:])
					return out.String()
				}
				end += index + 1
				out.WriteString(query[index:end])
				index = end
				continue
			}
			out.WriteByte(query[index])
			index++
		case '/':
			if index+1 < len(query) && query[index+1] == '*' {
				end := strings.Index(query[index+2:], "*/")
				if end < 0 {
					out.WriteString(query[index:])
					return out.String()
				}
				end += index + 4
				out.WriteString(query[index:end])
				index = end
				continue
			}
			out.WriteByte(query[index])
			index++
		case '$':
			tag, ok := postgresDollarTag(query[index:])
			if !ok {
				out.WriteByte(query[index])
				index++
				continue
			}
			end := strings.Index(query[index+len(tag):], tag)
			if end < 0 {
				out.WriteString(query[index:])
				return out.String()
			}
			end += index + 2*len(tag)
			out.WriteString(query[index:end])
			index = end
		case '?':
			out.WriteByte('$')
			out.WriteString(fmt.Sprint(parameter))
			parameter++
			index++
		default:
			out.WriteByte(query[index])
			index++
		}
	}
	return out.String()
}

// postgresCompatQuery handles the small SQLite expression surface used by the
// Store facade. Placeholder rebinding stays separate so callers keep one SQL
// shape and one argument list across both drivers.
func postgresCompatQuery(query string) string {
	query = postgresSchema(query)
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "ALTER TABLE ") && !strings.Contains(strings.ToUpper(query), " ADD COLUMN IF NOT EXISTS ") {
		query = strings.Replace(query, " ADD COLUMN ", " ADD COLUMN IF NOT EXISTS ", 1)
	}
	replacements := []string{
		"email = ? COLLATE NOCASE", "LOWER(email) = LOWER(?)",
		" COLLATE NOCASE", "",
		"MAX(0,storage_bytes+?)", "GREATEST(0,storage_bytes+?)",
		"MIN(codex_upstream_attempt_daily.first_created_at,excluded.first_created_at)", "LEAST(codex_upstream_attempt_daily.first_created_at,excluded.first_created_at)",
		"MAX(codex_upstream_attempt_daily.last_created_at,excluded.last_created_at)", "GREATEST(codex_upstream_attempt_daily.last_created_at,excluded.last_created_at)",
		"MAX(codex_upstream_attempt_daily.expires_at,excluded.expires_at)", "GREATEST(codex_upstream_attempt_daily.expires_at,excluded.expires_at)",
		"MAX(usage_events.updated_at, excluded.updated_at)", "GREATEST(usage_events.updated_at, excluded.updated_at)",
		"MAX(usage_events.updated_at,excluded.updated_at)", "GREATEST(usage_events.updated_at,excluded.updated_at)",
		"MAX(updated_at,?)", "GREATEST(updated_at,?)",
		"instr(account_model_capabilities.source, excluded.source)", "strpos(account_model_capabilities.source, excluded.source)",
		"strftime('%s','now')", "EXTRACT(EPOCH FROM NOW())::BIGINT",
		"CAST(json_extract(raw_usage_json,'$.kiro_credits') AS REAL)", "CAST(raw_usage_json::jsonb->>'kiro_credits' AS REAL)",
		"printf('%020d', cache_breakpoint_count)", "LPAD(CAST(cache_breakpoint_count AS TEXT),20,'0')",
	}
	query = strings.NewReplacer(replacements...).Replace(query)
	query = strings.ReplaceAll(query, "rowid", "ctid")
	return rewriteSQLiteScalarFunctions(query)
}

func rewriteSQLiteScalarFunctions(query string) string {
	var out strings.Builder
	out.Grow(len(query) + 16)
	for index := 0; index < len(query); {
		switch query[index] {
		case '\'', '"':
			index = copySQLQuoted(&out, query, index, query[index])
			continue
		case '-':
			if index+1 < len(query) && query[index+1] == '-' {
				end := strings.IndexByte(query[index:], '\n')
				if end < 0 {
					out.WriteString(query[index:])
					return out.String()
				}
				end += index + 1
				out.WriteString(query[index:end])
				index = end
				continue
			}
		case '/':
			if index+1 < len(query) && query[index+1] == '*' {
				end := strings.Index(query[index+2:], "*/")
				if end < 0 {
					out.WriteString(query[index:])
					return out.String()
				}
				end += index + 4
				out.WriteString(query[index:end])
				index = end
				continue
			}
		}
		if scalar, replacement := sqliteScalarFunctionAt(query, index); scalar {
			out.WriteString(replacement)
			index += 3
			continue
		}
		out.WriteByte(query[index])
		index++
	}
	return out.String()
}

func sqliteScalarFunctionAt(query string, index int) (bool, string) {
	if index > 0 && (query[index-1] == '_' || query[index-1] >= '0' && query[index-1] <= '9' || query[index-1] >= 'A' && query[index-1] <= 'Z' || query[index-1] >= 'a' && query[index-1] <= 'z') {
		return false, ""
	}
	if index+4 > len(query) || query[index+3] != '(' {
		return false, ""
	}
	name := strings.ToUpper(query[index : index+3])
	if name != "MAX" && name != "MIN" {
		return false, ""
	}
	depth := 1
	for cursor := index + 4; cursor < len(query); cursor++ {
		switch query[cursor] {
		case '\'', '"':
			quote := query[cursor]
			for cursor++; cursor < len(query); cursor++ {
				if query[cursor] != quote {
					continue
				}
				if cursor+1 < len(query) && query[cursor+1] == quote {
					cursor++
					continue
				}
				break
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return false, ""
			}
		case ',':
			if depth == 1 {
				if name == "MAX" {
					return true, "GREATEST"
				}
				return true, "LEAST"
			}
		}
	}
	return false, ""
}

func copySQLQuoted(out *strings.Builder, query string, start int, quote byte) int {
	out.WriteByte(quote)
	for index := start + 1; index < len(query); index++ {
		out.WriteByte(query[index])
		if query[index] != quote {
			continue
		}
		if index+1 < len(query) && query[index+1] == quote {
			out.WriteByte(query[index+1])
			index++
			continue
		}
		return index + 1
	}
	return len(query)
}

func postgresDollarTag(query string) (string, bool) {
	if len(query) < 2 || query[0] != '$' {
		return "", false
	}
	for index := 1; index < len(query); index++ {
		switch query[index] {
		case '$':
			return query[:index+1], true
		default:
			if query[index] != '_' && (query[index] < 'a' || query[index] > 'z') && (query[index] < 'A' || query[index] > 'Z') && (query[index] < '0' || query[index] > '9') {
				return "", false
			}
		}
	}
	return "", false
}

func postgresSchema(source string) string {
	source = strings.ReplaceAll(source, "INTEGER PRIMARY KEY AUTOINCREMENT", "BIGSERIAL PRIMARY KEY")
	source = strings.ReplaceAll(source, " BLOB ", " BYTEA ")
	return source
}

func splitSQLStatements(source string) []string {
	var statements []string
	start := 0
	var single, double, lineComment, blockComment bool
	for index := 0; index < len(source); index++ {
		switch {
		case lineComment:
			lineComment = source[index] != '\n'
		case blockComment:
			if source[index] == '*' && index+1 < len(source) && source[index+1] == '/' {
				blockComment = false
				index++
			}
		case single:
			if source[index] == '\'' {
				if index+1 < len(source) && source[index+1] == '\'' {
					index++
				} else {
					single = false
				}
			}
		case double:
			if source[index] == '"' {
				if index+1 < len(source) && source[index+1] == '"' {
					index++
				} else {
					double = false
				}
			}
		case source[index] == '-' && index+1 < len(source) && source[index+1] == '-':
			lineComment = true
			index++
		case source[index] == '/' && index+1 < len(source) && source[index+1] == '*':
			blockComment = true
			index++
		case source[index] == '\'':
			single = true
		case source[index] == '"':
			double = true
		case source[index] == ';':
			if statement := strings.TrimSpace(source[start:index]); statement != "" {
				statements = append(statements, statement)
			}
			start = index + 1
		}
	}
	if statement := strings.TrimSpace(source[start:]); statement != "" {
		statements = append(statements, statement)
	}
	return statements
}

func (s *Store) initPostgres(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext('codex-account-pool-schema'))`); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtext('codex-account-pool-schema'))`)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(
driver TEXT NOT NULL, version TEXT NOT NULL, checksum TEXT NOT NULL, applied_at BIGINT NOT NULL, PRIMARY KEY(driver,version))`); err != nil {
		return err
	}
	const version = "20260727_base_v1"
	combined := postgresSchema(schemaSQL + goalContinuitySchemaSQL + codexSessionMappingSchemaSQL + lifecycleSchemaSQL)
	checksumRaw := sha256.Sum256([]byte(combined))
	checksum := hex.EncodeToString(checksumRaw[:])
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE driver='postgres' AND version=?`, version).Scan(&existing)
	if err == nil && existing != checksum {
		return fmt.Errorf("PostgreSQL migration checksum mismatch for %s", version)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, statement := range splitSQLStatements(combined) {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply PostgreSQL schema: %w", err)
		}
	}
	now := Now()
	if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(driver,version,checksum,applied_at) VALUES('postgres',?,?,?)
ON CONFLICT(driver,version) DO NOTHING`, version, checksum, now); err != nil {
		return err
	}
	// Keep the immutable base checksum stable for existing PostgreSQL installs.
	// Team lifecycle is an additive migration with its own checksum/version.
	const teamLifecycleVersion = "20260731_team_lifecycle_v1"
	teamLifecycleSchema := postgresSchema(teamManagementSchemaSQL)
	teamChecksumRaw := sha256.Sum256([]byte(teamLifecycleSchema))
	teamChecksum := hex.EncodeToString(teamChecksumRaw[:])
	existing = ""
	err = tx.QueryRowContext(ctx, `
SELECT checksum FROM schema_migrations WHERE driver='postgres' AND version=?`,
		teamLifecycleVersion).Scan(&existing)
	if err == nil && existing != teamChecksum {
		return fmt.Errorf("PostgreSQL migration checksum mismatch for %s", teamLifecycleVersion)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, statement := range splitSQLStatements(teamLifecycleSchema) {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply PostgreSQL team lifecycle schema: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, `
INSERT INTO schema_migrations(driver,version,checksum,applied_at)
VALUES('postgres',?,?,?) ON CONFLICT(driver,version) DO NOTHING`,
		teamLifecycleVersion, teamChecksum, now); err != nil {
		return err
	}
	// This table is independent from the immutable base and team lifecycle v1
	// migrations. The checked migration below owns the team mailbox columns.
	for _, statement := range splitSQLStatements(postgresSchema(mailboxProfileSchemaSQL)) {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply PostgreSQL mailbox health schema: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES('usage_accuracy_cutover_at',?,?)
ON CONFLICT(key) DO NOTHING`, fmt.Sprint(now), now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO groups(name,system_prompt,prompt_mode,system_prompt_apply_to_compaction,virtual_2m_enabled,created_at,updated_at)
VALUES ('cyber','','prepend',1,0,?,?),('kiro','','prepend',0,0,?,?),('antigravity','','prepend',0,0,?,?)
ON CONFLICT(name) DO NOTHING`, now, now, now, now, now, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO egress_profiles(id,name,type,endpoint,region,stream_capable,health,latency_millis,cf_score,last_cf_ray,cooldown_until,max_concurrency,created_at,updated_at)
VALUES(?,'direct','direct','','',1,'healthy',0,0,'',0,0,?,?) ON CONFLICT(id) DO NOTHING`, DefaultDirectEgressID, now, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE egress_profiles SET max_concurrency=0,updated_at=? WHERE id=? AND type='direct' AND max_concurrency=128`, now, DefaultDirectEgressID); err != nil {
		return err
	}
	for _, provider := range []struct{ id, name, baseURL, models string }{
		{"deepseek", "DeepSeek", "https://api.deepseek.com/v1", `["deepseek-chat","deepseek-reasoner"]`},
		{"siliconflow", "SiliconFlow 硅基流动", "https://api.siliconflow.cn/v1", `[]`},
	} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO custom_providers(id,name,base_url,upstream_protocol,enabled,auto_discover_models,models_json,created_at,updated_at)
VALUES(?,?,?,'chat_completions',1,1,?,?,?) ON CONFLICT(id) DO NOTHING`, provider.id, provider.name, provider.baseURL, provider.models, now, now); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = s.migrate(ctx); err != nil {
		return fmt.Errorf("apply PostgreSQL additive migrations: %w", err)
	}
	if err = s.applyCheckedPostgresMigrations(ctx); err != nil {
		return err
	}
	_, err = s.RepairMissingAccountEgressBindings(ctx)
	return err
}

type checkedPostgresMigration struct {
	version    string
	statements []string
}

var checkedPostgresMigrations = []checkedPostgresMigration{
	{
		version: "20260731_team_mailbox_policy_v1",
		statements: []string{
			`ALTER TABLE team_workspaces ADD COLUMN IF NOT EXISTS mailbox_provider_key TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE team_workspaces ADD COLUMN IF NOT EXISTS required_email_domain TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE team_workspaces ADD COLUMN IF NOT EXISTS same_domain_required INTEGER NOT NULL DEFAULT 1`,
			`ALTER TABLE team_lifecycle_workflows ADD COLUMN IF NOT EXISTS mailbox_provider_key TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE team_lifecycle_workflows ADD COLUMN IF NOT EXISTS required_email_domain TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_team_workspaces_mailbox_provider ON team_workspaces(mailbox_provider_key)`,
		},
	},
	{
		version: "20260727_float64_v1",
		statements: []string{
			`ALTER TABLE paypal_accounts ALTER COLUMN balance_usd TYPE DOUBLE PRECISION USING balance_usd::double precision`,
			`ALTER TABLE kiro_runtime_capabilities ALTER COLUMN cache_reuse_credit_reduction_percent TYPE DOUBLE PRECISION USING cache_reuse_credit_reduction_percent::double precision`,
			`ALTER TABLE usage_records ALTER COLUMN kiro_credits TYPE DOUBLE PRECISION USING kiro_credits::double precision`,
			`ALTER TABLE account_rate_limits ALTER COLUMN used_percent TYPE DOUBLE PRECISION USING used_percent::double precision`,
			`ALTER TABLE registration_records ALTER COLUMN cost_usd TYPE DOUBLE PRECISION USING cost_usd::double precision`,
			`ALTER TABLE registration_records ALTER COLUMN sms_cost TYPE DOUBLE PRECISION USING sms_cost::double precision`,
			`ALTER TABLE registration_stats_daily ALTER COLUMN cost_usd TYPE DOUBLE PRECISION USING cost_usd::double precision`,
		},
	},
	{
		version: "20260727_lifecycle_columns_v1",
		statements: []string{
			`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS registration_method TEXT NOT NULL DEFAULT 'manual'`,
			`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS phone TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS subscription_status TEXT NOT NULL DEFAULT 'unknown'`,
			`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS subscription_expires_at INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS last_validity_check_at INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS registration_task_id TEXT NOT NULL DEFAULT ''`,
		},
	},
}

func (s *Store) applyCheckedPostgresMigrations(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, migration := range checkedPostgresMigrations {
		source := strings.Join(migration.statements, ";\n")
		checksumRaw := sha256.Sum256([]byte(source))
		checksum := hex.EncodeToString(checksumRaw[:])
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE driver='postgres' AND version=?`, migration.version).Scan(&existing)
		if err == nil {
			if existing != checksum {
				return fmt.Errorf("PostgreSQL migration checksum mismatch for %s", migration.version)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, statement := range migration.statements {
			if _, err = tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply PostgreSQL migration %s: %w", migration.version, err)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(driver,version,checksum,applied_at) VALUES('postgres',?,?,?)`, migration.version, checksum, Now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
