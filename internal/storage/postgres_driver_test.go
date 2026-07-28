package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"github.com/jackc/pgx/v5"
)

func TestRebindPostgresSkipsQuotedAndCommentQuestionMarks(t *testing.T) {
	query := "SELECT '?', \"?\", value FROM t WHERE a=? AND note='it''s ?' -- ?\nAND b=? /* ? */"
	got := rebindPostgres(query)
	want := "SELECT '?', \"?\", value FROM t WHERE a=$1 AND note='it''s ?' -- ?\nAND b=$2 /* ? */"
	if got != want {
		t.Fatalf("rebind:\n got %s\nwant %s", got, want)
	}
}

func TestPostgresSchemaTranslationAndSplit(t *testing.T) {
	translated := postgresSchema("CREATE TABLE a(id INTEGER PRIMARY KEY AUTOINCREMENT, raw BLOB NOT NULL); CREATE INDEX i ON a(id);")
	if strings.Contains(translated, "AUTOINCREMENT") || !strings.Contains(translated, "BIGSERIAL PRIMARY KEY") || !strings.Contains(translated, "BYTEA") {
		t.Fatalf("translated schema=%s", translated)
	}
	if statements := splitSQLStatements(translated); len(statements) != 2 {
		t.Fatalf("statements=%v", statements)
	}
}

func TestPostgresIntegrationInitAndCoreRoundTrip(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	cfg := config.Default()
	cfg.StorageDriver = "postgres"
	cfg.PostgresDSN = dsn
	cfg.RedisURL = "redis://integration-required.invalid:6379/0"
	store, err := OpenWithConfig(cfg)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err = store.Init(ctx); err != nil {
		t.Fatalf("init PostgreSQL: %v", err)
	}
	if _, err = store.GetGroup(ctx, "cyber"); err != nil {
		t.Fatalf("get seeded group: %v", err)
	}
	if _, err = store.GetEgressProfile(ctx, DefaultDirectEgressID); err != nil {
		t.Fatalf("get seeded egress: %v", err)
	}
	account := Account{ID: "postgres-integration-account", Label: "PostgreSQL integration", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err = store.UpsertAccount(ctx, account, AccountToken{AccessToken: "integration-token"}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	roundTrip, err := store.GetAccount(ctx, account.ID)
	if err != nil || roundTrip.Provider != account.Provider {
		t.Fatalf("round-trip account=%+v err=%v", roundTrip, err)
	}
	if _, err = store.UpsertAffinityBindingResult(ctx, AffinityBinding{
		RouteKeyHash: "postgres-integration-route", RouteKey: "route", Source: "session",
		AccountID: account.ID, Provider: "codex", Model: "gpt-5", EgressID: DefaultDirectEgressID,
	}); err != nil {
		t.Fatalf("upsert affinity: %v", err)
	}
	if _, err = store.GetAffinityBinding(ctx, "postgres-integration-route"); err != nil {
		t.Fatalf("get affinity: %v", err)
	}
	holdID, err := store.CreateBillingHold(ctx, "postgres-integration-route", account.ID, 30)
	if err != nil {
		t.Fatalf("create billing hold: %v", err)
	}
	if err = store.InsertUsageRecordWithDiagnostics(ctx, account.ID, "postgres-integration-route", "", "", "gpt-5", 20, 10, 30, 5, 5, 0,
		json.RawMessage(`{"input_tokens":20,"output_tokens":10}`), UsageDiagnostics{
			UsageEventID: "usage_" + holdID, UsageProvider: "codex", UsageSource: "upstream", BillingHoldID: holdID, RouteEpoch: 1,
		}); err != nil {
		t.Fatalf("insert real usage: %v", err)
	}
	if err = store.SettleBillingHold(ctx, holdID, "settled"); err != nil {
		t.Fatalf("settle billing hold: %v", err)
	}
	if _, err = store.CacheUsageMetricsWindow(ctx, Now()-60, Now()+60); err != nil {
		t.Fatalf("cache usage aggregation: %v", err)
	}
	goal, err := store.CommitGoalTurn(ctx, GoalTurn{
		Protocol: "codex", ResponseID: "postgres-response-1", Aliases: []GoalAlias{{Type: "response_id", Value: "postgres-response-1"}},
		CheckpointPayload: `{"model":"gpt-5","input":[]}`, SegmentPayload: `{"input":"hello","output":"world"}`,
		ExpiresAt: Now() + 3600, StorageMaxBytes: 64 << 20, CompressionStages: 2,
	})
	if err != nil {
		t.Fatalf("commit goal turn: %v", err)
	}
	if replay, _, replayErr := store.BuildGoalReplay(ctx, goal.ID); replayErr != nil || len(replay) == 0 {
		t.Fatalf("build goal replay bytes=%d err=%v", len(replay), replayErr)
	}
	if err = store.PutContextJournal(ctx, ContextJournal{
		ResponseID: "postgres-context", AffinityHash: "affinity", AccountID: account.ID,
		Payload: `{"input":"context"}`, ExpiresAt: Now() + 3600,
	}); err != nil {
		t.Fatalf("put context journal: %v", err)
	}
	if _, err = store.ClearContextJournal(ctx); err != nil {
		t.Fatalf("clear context journal: %v", err)
	}
	if inserted, bulkErr := store.BulkInsertEmailAccounts(ctx, []EmailAccount{{ID: "postgres-email", Email: "postgres@example.test"}}); bulkErr != nil || inserted > 1 {
		t.Fatalf("bulk email inserted=%d err=%v", inserted, bulkErr)
	}
	if err = store.SetEgressCooldown(ctx, DefaultDirectEgressID, Now()+60, "integration-ray"); err != nil {
		t.Fatalf("set egress cooldown: %v", err)
	}
	if err = store.InsertCodexUpstreamAttempt(ctx, CodexUpstreamAttempt{
		TreeID: "postgres-tree", AccountID: account.ID, EgressID: DefaultDirectEgressID, Epoch: 1,
		State: "usage_limit", StatusCode: 429, CreatedAt: Now() - 8*24*3600, ExpiresAt: Now() - 1,
	}); err != nil {
		t.Fatalf("insert upstream attempt: %v", err)
	}
	if _, err = store.CleanupCodexSessionMappings(ctx); err != nil {
		t.Fatalf("aggregate upstream attempts: %v", err)
	}
	if daily, err := store.ListCodexUpstreamAttemptDailyDiagnostics(ctx); err != nil || len(daily) == 0 {
		t.Fatalf("daily upstream attempts=%v err=%v", daily, err)
	}
	if err = store.CheckpointLogStorage(ctx); err != nil {
		t.Fatalf("PostgreSQL maintenance: %v", err)
	}
	snapshot, err := store.BeginDiagnosticSnapshot(ctx)
	if err != nil {
		t.Fatalf("begin diagnostic snapshot: %v", err)
	}
	if err = snapshot.Close(); err != nil {
		t.Fatalf("close diagnostic snapshot: %v", err)
	}
}

func TestPostgresIntegrationRecoversAfterDurableRestart(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	container := strings.TrimSpace(os.Getenv("TEST_POSTGRES_RESTART_CONTAINER"))
	if dsn == "" || container == "" {
		t.Skip("TEST_POSTGRES_DSN and TEST_POSTGRES_RESTART_CONTAINER are required")
	}
	cfg := config.Default()
	cfg.StorageDriver, cfg.PostgresDSN, cfg.RedisURL = "postgres", dsn, "redis://integration-required.invalid:6379/0"
	store, err := OpenWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "postgres-restart-account", Label: "Restart", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err = store.UpsertAccount(ctx, account, AccountToken{AccessToken: "restart-token"}); err != nil {
		t.Fatal(err)
	}
	if output, restartErr := exec.Command("docker", "restart", container).CombinedOutput(); restartErr != nil {
		t.Fatalf("restart PostgreSQL: %v output=%s", restartErr, output)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		pingCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		pingErr := store.DB().PingContext(pingCtx)
		cancel()
		if pingErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("PostgreSQL did not recover: %v", pingErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	got, err := store.GetAccount(ctx, account.ID)
	if err != nil || got.Label != account.Label {
		t.Fatalf("account after restart=%+v err=%v", got, err)
	}
}

func TestPostgresIntegrationActiveActiveUsageIdempotency(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	cfg := config.Default()
	cfg.StorageDriver, cfg.PostgresDSN, cfg.RedisURL = "postgres", dsn, "redis://integration-required.invalid:6379/0"
	first, err := OpenWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx := context.Background()
	if err = first.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err = second.Init(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	holdID, eventID := "pg-ha-hold-"+suffix, "pg-ha-event-"+suffix
	if err = first.BatchWriteTelemetry(ctx, nil, nil, []BillingHoldWrite{{ID: holdID, EventID: eventID, AccountID: "pg-ha-account", EstimatedTokens: 120, RouteEpoch: 17, Create: true}}, nil); err != nil {
		t.Fatal(err)
	}
	write := UsageRecordWrite{AccountID: "pg-ha-account", Model: "gpt-5.6", Prompt: 90, Completion: 10, Total: 100,
		Raw: json.RawMessage(`{"total_tokens":100}`), Diagnostics: UsageDiagnostics{UsageEventID: eventID, BillingHoldID: holdID, UsageSource: "upstream", RouteEpoch: 17}}
	const workers = 32
	var wait sync.WaitGroup
	errs := make(chan error, workers)
	for index := 0; index < workers; index++ {
		wait.Add(1)
		store := first
		if index%2 == 1 {
			store = second
		}
		go func() {
			defer wait.Done()
			errs <- store.BatchInsertUsageRecords(ctx, []UsageRecordWrite{write})
		}()
	}
	wait.Wait()
	close(errs)
	for writeErr := range errs {
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	var rows, total int64
	if err = first.DB().QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(total_tokens),0) FROM usage_records WHERE usage_event_id=?`, eventID).Scan(&rows, &total); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || total != 100 {
		t.Fatalf("active-active duplicate event rows=%d total=%d", rows, total)
	}
}

func TestPostgresIntegrationPromotesPhysicalStandby(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_FAILOVER_DSN"))
	standbyDSN := strings.TrimSpace(os.Getenv("TEST_POSTGRES_STANDBY_DSN"))
	primaryContainer := strings.TrimSpace(os.Getenv("TEST_POSTGRES_PRIMARY_CONTAINER"))
	standbyContainer := strings.TrimSpace(os.Getenv("TEST_POSTGRES_STANDBY_CONTAINER"))
	if dsn == "" || standbyDSN == "" || primaryContainer == "" || standbyContainer == "" {
		t.Skip("PostgreSQL physical failover environment is not configured")
	}
	cfg := config.Default()
	cfg.StorageDriver, cfg.PostgresDSN, cfg.RedisURL = "postgres", dsn, "redis://integration-required.invalid:6379/0"
	store, err := OpenWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	before := Account{ID: "pg-failover-before-" + suffix, Label: "replicated-before-promotion", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err = store.UpsertAccount(ctx, before, AccountToken{AccessToken: "before-token"}); err != nil {
		t.Fatal(err)
	}
	replica, err := pgx.Connect(ctx, standbyDSN)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var count int
		replicaErr := replica.QueryRow(ctx, `SELECT COUNT(*) FROM accounts WHERE id=$1`, before.ID).Scan(&count)
		if replicaErr == nil && count == 1 {
			break
		}
		if time.Now().After(deadline) {
			replica.Close(ctx)
			t.Fatalf("standby did not catch up before promotion: %v", replicaErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	replica.Close(ctx)
	if output, stopErr := exec.Command("docker", "stop", primaryContainer).CombinedOutput(); stopErr != nil {
		t.Fatalf("stop PostgreSQL primary: %v output=%s", stopErr, output)
	}
	if output, promoteErr := exec.Command("docker", "exec", "-u", "postgres", standbyContainer, "pg_ctl", "-D", "/var/lib/postgresql/data", "promote", "-w").CombinedOutput(); promoteErr != nil {
		t.Fatalf("promote PostgreSQL standby: %v output=%s", promoteErr, output)
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		pingCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		pingErr := store.DB().PingContext(pingCtx)
		cancel()
		if pingErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool did not reconnect to promoted standby: %v", pingErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if replicated, getErr := store.GetAccount(ctx, before.ID); getErr != nil || replicated.Label != before.Label {
		t.Fatalf("replicated account after promotion=%+v err=%v", replicated, getErr)
	}
	after := Account{ID: "pg-failover-after-" + suffix, Label: "written-after-promotion", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err = store.UpsertAccount(ctx, after, AccountToken{AccessToken: "after-token"}); err != nil {
		t.Fatalf("write after promotion: %v", err)
	}
	if persisted, getErr := store.GetAccount(ctx, after.ID); getErr != nil || persisted.Label != after.Label {
		t.Fatalf("post-promotion account=%+v err=%v", persisted, getErr)
	}
}
