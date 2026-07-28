package storage

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
)

func TestMigrationDigestIsOrderIndependentAndNormalizesIntegerWidths(t *testing.T) {
	var first, second migrationDigest
	for _, row := range [][]any{{int64(1), "one"}, {int64(2), "two"}} {
		if err := first.Add(row); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range [][]any{{int32(2), "two"}, {int32(1), "one"}} {
		if err := second.Add(row); err != nil {
			t.Fatal(err)
		}
	}
	if first != second || first.String() == "" {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func TestOrderMigrationTablesHonorsForeignKeys(t *testing.T) {
	ordered, err := orderMigrationTables([]migrationTable{
		{name: "grandchild", dependencies: []string{"child"}},
		{name: "parent"},
		{name: "child", dependencies: []string{"parent"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ordered[0].name != "parent" || ordered[1].name != "child" || ordered[2].name != "grandchild" {
		t.Fatalf("order=%+v", ordered)
	}
	if _, err = orderMigrationTables([]migrationTable{{name: "a", dependencies: []string{"b"}}, {name: "b", dependencies: []string{"a"}}}); err == nil {
		t.Fatal("cyclic dependency was accepted")
	}
}

func TestMigrationDigestChangesWithDuplicateRows(t *testing.T) {
	var once, twice migrationDigest
	row := []any{[]byte("payload"), float64(1.25)}
	if err := once.Add(row); err != nil {
		t.Fatal(err)
	}
	if err := twice.Add(row); err != nil {
		t.Fatal(err)
	}
	if err := twice.Add(row); err != nil {
		t.Fatal(err)
	}
	if once == twice || once.String() == twice.String() {
		t.Fatal("row multiplicity was not represented in the digest")
	}
	if len(once.String()) != sha256.Size*2 {
		t.Fatalf("digest length=%d", len(once.String()))
	}
}

func TestSQLitePostgresMigrationIntegration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	sourcePath := filepath.Join(t.TempDir(), "source.sqlite3")
	source, err := Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Init(ctx); err != nil {
		source.Close()
		t.Fatal(err)
	}
	account := Account{ID: "migration-account", Label: "Migration", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err = source.UpsertAccount(ctx, account, AccountToken{AccessToken: "migration-token"}); err != nil {
		source.Close()
		t.Fatal(err)
	}
	const preciseCost = 0.123456789123
	if _, err = source.DB().ExecContext(ctx, `INSERT INTO registration_records(id,job_id,account_id,email,phone,tier,cost_usd,duration_seconds,status,error,detail_json,created_at,sms_provider,sms_country,sms_cost)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "migration-registration", "job", account.ID, "migration@example.test", "", "free", preciseCost, 1, "success", "", "{}", Now(), "provider", "US", preciseCost); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := MigrateSQLiteToPostgres(ctx, SQLitePostgresMigrationOptions{SQLitePath: sourcePath, PostgresDSN: dsn, ReplaceTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows == 0 || len(result.Tables) == 0 {
		t.Fatalf("result=%+v", result)
	}
	cfg := config.Default()
	cfg.StorageDriver, cfg.PostgresDSN, cfg.RedisURL = "postgres", dsn, "redis://migration.invalid:6379/0"
	target, err := OpenWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err = target.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if migrated, getErr := target.GetAccount(ctx, account.ID); getErr != nil || migrated.Label != account.Label {
		t.Fatalf("account=%+v err=%v", migrated, getErr)
	}
	var cost, smsCost float64
	if err = target.DB().QueryRowContext(ctx, `SELECT cost_usd,sms_cost FROM registration_records WHERE id=?`, "migration-registration").Scan(&cost, &smsCost); err != nil {
		t.Fatal(err)
	}
	if cost != preciseCost || smsCost != preciseCost {
		t.Fatalf("numeric precision changed: cost=%.15f sms=%.15f want=%.15f", cost, smsCost, preciseCost)
	}
}
