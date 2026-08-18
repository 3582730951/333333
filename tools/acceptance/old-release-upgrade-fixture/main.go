package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

const (
	fixtureEpoch       = int64(1786766400)
	defaultFixtureRows = 50000
)

type fixtureSnapshot struct {
	FormatVersion       int    `json:"format_version"`
	Integrity           string `json:"integrity"`
	Accounts            int64  `json:"accounts"`
	ModelCapabilities   int64  `json:"model_capabilities"`
	AffinityBindings    int64  `json:"affinity_bindings"`
	UsageRows           int64  `json:"usage_rows"`
	CodexUsageRows      int64  `json:"codex_usage_rows"`
	KiroUsageRows       int64  `json:"kiro_usage_rows"`
	PromptTokens        int64  `json:"prompt_tokens"`
	CompletionTokens    int64  `json:"completion_tokens"`
	TotalTokens         int64  `json:"total_tokens"`
	CachedTokens        int64  `json:"cached_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	GoalSessions        int64  `json:"goal_sessions"`
	GoalAliases         int64  `json:"goal_aliases"`
	GoalCheckpoints     int64  `json:"goal_checkpoints"`
	ContextJournal      int64  `json:"context_journal"`
	CodexBindings       int64  `json:"codex_bindings"`
	CodexSessionAliases int64  `json:"codex_session_aliases"`
	GoalPayload         string `json:"goal_payload"`
	ContextPayload      string `json:"context_payload"`
	SessionIdentity     string `json:"session_identity"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "old-release-upgrade-fixture: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: old-release-upgrade-fixture MODE --database PATH [--rows N] [--snapshot PATH]")
	}
	mode := args[0]
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	databasePath := flags.String("database", "", "SQLite database path")
	rows := flags.Int("rows", defaultFixtureRows, "number of historical usage rows")
	snapshotPath := flags.String("snapshot", "", "snapshot path for snapshot/verify")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*databasePath) == "" {
		return errors.New("--database is required")
	}
	if *rows <= 0 {
		return errors.New("--rows must be positive")
	}
	db, err := openFixtureDB(*databasePath)
	if err != nil {
		return err
	}
	defer db.Close()

	switch mode {
	case "seed":
		if err := seedFixture(db, *rows); err != nil {
			return err
		}
		snapshot, err := readFixtureSnapshot(db)
		if err != nil {
			return err
		}
		fmt.Printf("SEEDED accounts=%d usage=%d codex=%d kiro=%d goals=%d contexts=%d sessions=%d\n",
			snapshot.Accounts, snapshot.UsageRows, snapshot.CodexUsageRows, snapshot.KiroUsageRows,
			snapshot.GoalSessions, snapshot.ContextJournal, snapshot.CodexBindings)
		return nil
	case "snapshot":
		if strings.TrimSpace(*snapshotPath) == "" {
			return errors.New("snapshot mode requires --snapshot")
		}
		snapshot, err := readFixtureSnapshot(db)
		if err != nil {
			return err
		}
		if err := writeSnapshotAtomic(*snapshotPath, snapshot); err != nil {
			return err
		}
		fmt.Printf("SNAPSHOT %s usage=%d integrity=%s\n", *snapshotPath, snapshot.UsageRows, snapshot.Integrity)
		return nil
	case "verify":
		if strings.TrimSpace(*snapshotPath) == "" {
			return errors.New("verify mode requires --snapshot")
		}
		if err := verifyFixtureSnapshot(db, *snapshotPath); err != nil {
			return err
		}
		fmt.Printf("VERIFIED snapshot=%s\n", *snapshotPath)
		return nil
	default:
		return fmt.Errorf("unsupported mode %q (want seed, snapshot, or verify)", mode)
	}
}

func openFixtureDB(path string) (*sql.DB, error) {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	db, err := sql.Open("sqlite3", path+separator+"_busy_timeout=30000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func seedFixture(db *sql.DB, usageRows int) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin fixture seed: %w", err)
	}
	defer tx.Rollback()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT OR REPLACE INTO accounts(id,label,group_name,provider,status,created_at,updated_at) VALUES('old-acc-codex','old codex','cyber','codex','active',?,?)`, []any{fixtureEpoch, fixtureEpoch}},
		{`INSERT OR REPLACE INTO accounts(id,label,group_name,provider,status,created_at,updated_at) VALUES('old-acc-kiro','old kiro','kiro','kiro','active',?,?)`, []any{fixtureEpoch, fixtureEpoch}},
		{`INSERT OR REPLACE INTO account_model_capabilities(account_id,model_slug,availability_state,context_1m_state,context_1m_source,source,last_probe_at) VALUES('old-acc-codex','gpt-5.4','verified','supported','old-fixture','probe',?)`, []any{fixtureEpoch}},
		{`INSERT OR REPLACE INTO account_model_capabilities(account_id,model_slug,availability_state,context_1m_state,context_1m_source,source,last_probe_at) VALUES('old-acc-kiro','claude-opus-4.8','verified','unknown','','probe',?)`, []any{fixtureEpoch}},
		{`INSERT OR REPLACE INTO affinity_bindings(route_key_hash,route_key,source,account_id,provider,model,egress_id,epoch,created_at,updated_at,expires_at) VALUES('old-route-hash','old-route','prompt_cache_key','old-acc-codex','codex','gpt-5.4','egress_direct',7,?,?,?)`, []any{fixtureEpoch, fixtureEpoch, fixtureEpoch + 86400}},
		{`INSERT OR REPLACE INTO goal_session(id,protocol,downstream_key_hash,workspace_hash,initial_goal_hash,last_response_hash,state,current_checkpoint_id,encrypted_working_state,storage_bytes,expires_at,created_at,updated_at) VALUES('old-goal','responses','downstream-old','workspace-old','initial-old','response-old','ready','old-checkpoint','old-working-state',17,?,?,?)`, []any{fixtureEpoch + 86400, fixtureEpoch, fixtureEpoch}},
		{`INSERT OR REPLACE INTO goal_alias(alias_hash,alias_type,goal_id,created_at) VALUES('old-goal-alias','response_id','old-goal',?)`, []any{fixtureEpoch}},
		{`INSERT OR REPLACE INTO goal_checkpoint(id,goal_id,sequence,through_segment_sequence,payload_hash,payload_bytes,encrypted_payload,format_version,created_at) VALUES('old-checkpoint','old-goal',1,0,'old-payload-hash',11,'old-payload',2,?)`, []any{fixtureEpoch}},
		{`INSERT OR REPLACE INTO context_journal(response_id,affinity_hash,account_id,encrypted_payload,created_at,expires_at) VALUES('old-response','old-route-hash','old-acc-codex','old-context-payload',?,?)`, []any{fixtureEpoch, fixtureEpoch + 86400}},
		{`INSERT OR REPLACE INTO codex_session_binding(id,tree_id,namespace_hash,account_id,egress_id,epoch,state,encrypted_identity,created_at,updated_at,expires_at) VALUES('old-binding','old-tree','old-namespace','old-acc-codex','egress_direct',7,'active','old-encrypted-identity',?,?,?)`, []any{fixtureEpoch, fixtureEpoch, fixtureEpoch + 86400}},
		{`INSERT OR REPLACE INTO codex_session_alias(alias_hash,alias_type,binding_id,created_at,updated_at,expires_at) VALUES('old-session-alias','response_id','old-binding',?,?,?)`, []any{fixtureEpoch, fixtureEpoch, fixtureEpoch + 86400}},
	}
	for index, statement := range statements {
		if _, err := tx.Exec(statement.query, statement.args...); err != nil {
			return fmt.Errorf("seed fixture statement %d: %w", index+1, err)
		}
	}
	_, err = tx.Exec(`WITH RECURSIVE n(x) AS (
SELECT 1 WHERE ?>0 UNION ALL SELECT x+1 FROM n WHERE x<?
) INSERT INTO usage_records(
usage_event_id,account_id,route_key_hash,model,prompt_tokens,completion_tokens,total_tokens,
cached_tokens,cache_read_tokens,usage_provider,usage_source,cache_read_present,
cache_total_input_tokens,raw_usage_json,created_at
) SELECT 'old-event-'||x,
CASE WHEN x%5=0 THEN 'old-acc-kiro' ELSE 'old-acc-codex' END,
'old-route-hash',CASE WHEN x%5=0 THEN 'claude-opus-4.8' ELSE 'gpt-5.4' END,
128000+x%1000,256,128256+x%1000,64000,64000,
CASE WHEN x%5=0 THEN 'kiro' ELSE 'codex' END,'upstream',1,128000+x%1000,
'{"fixture":"old-release"}',?-x%86400 FROM n`, usageRows, usageRows, fixtureEpoch)
	if err != nil {
		return fmt.Errorf("seed historical usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit fixture seed: %w", err)
	}
	return nil
}

func readFixtureSnapshot(db *sql.DB) (fixtureSnapshot, error) {
	out := fixtureSnapshot{FormatVersion: 1}
	var integrityRows []string
	rows, err := db.Query(`PRAGMA quick_check`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			return out, err
		}
		integrityRows = append(integrityRows, value)
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	out.Integrity = strings.Join(integrityRows, ";")
	queries := []struct {
		query string
		dest  []any
	}{
		{`SELECT COUNT(*) FROM accounts WHERE id IN ('old-acc-codex','old-acc-kiro')`, []any{&out.Accounts}},
		{`SELECT COUNT(*) FROM account_model_capabilities WHERE account_id IN ('old-acc-codex','old-acc-kiro')`, []any{&out.ModelCapabilities}},
		{`SELECT COUNT(*) FROM affinity_bindings WHERE route_key_hash='old-route-hash'`, []any{&out.AffinityBindings}},
		{`SELECT COUNT(*),
SUM(CASE WHEN usage_provider='codex' THEN 1 ELSE 0 END),
SUM(CASE WHEN usage_provider='kiro' THEN 1 ELSE 0 END),
COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0),COALESCE(SUM(total_tokens),0),
COALESCE(SUM(cached_tokens),0),COALESCE(SUM(cache_read_tokens),0)
FROM usage_records WHERE usage_event_id LIKE 'old-event-%'`, []any{&out.UsageRows, &out.CodexUsageRows, &out.KiroUsageRows, &out.PromptTokens, &out.CompletionTokens, &out.TotalTokens, &out.CachedTokens, &out.CacheReadTokens}},
		{`SELECT COUNT(*) FROM goal_session WHERE id='old-goal'`, []any{&out.GoalSessions}},
		{`SELECT COUNT(*) FROM goal_alias WHERE goal_id='old-goal'`, []any{&out.GoalAliases}},
		{`SELECT COUNT(*) FROM goal_checkpoint WHERE goal_id='old-goal'`, []any{&out.GoalCheckpoints}},
		{`SELECT COUNT(*) FROM context_journal WHERE response_id='old-response'`, []any{&out.ContextJournal}},
		{`SELECT COUNT(*) FROM codex_session_binding WHERE id='old-binding'`, []any{&out.CodexBindings}},
		{`SELECT COUNT(*) FROM codex_session_alias WHERE binding_id='old-binding'`, []any{&out.CodexSessionAliases}},
		{`SELECT encrypted_working_state FROM goal_session WHERE id='old-goal'`, []any{&out.GoalPayload}},
		{`SELECT encrypted_payload FROM context_journal WHERE response_id='old-response'`, []any{&out.ContextPayload}},
		{`SELECT encrypted_identity FROM codex_session_binding WHERE id='old-binding'`, []any{&out.SessionIdentity}},
	}
	for index, query := range queries {
		if err := db.QueryRow(query.query).Scan(query.dest...); err != nil {
			return out, fmt.Errorf("snapshot query %d: %w", index+1, err)
		}
	}
	if out.Integrity != "ok" {
		return out, fmt.Errorf("SQLite quick_check=%q", out.Integrity)
	}
	return out, nil
}

func writeSnapshotAtomic(path string, snapshot fixtureSnapshot) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(parent, ".old-release-snapshot-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(snapshot); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func verifyFixtureSnapshot(db *sql.DB, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var expected fixtureSnapshot
	if err := json.Unmarshal(data, &expected); err != nil {
		return err
	}
	actual, err := readFixtureSnapshot(db)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		wantJSON, _ := json.Marshal(expected)
		gotJSON, _ := json.Marshal(actual)
		return fmt.Errorf("old-release data changed across upgrade\nwant=%s\n got=%s", wantJSON, gotJSON)
	}
	return nil
}
