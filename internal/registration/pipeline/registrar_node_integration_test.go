package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/storage"
)

// TestLiveNodeRegistration drives ONE real registration through the full orchestrator
// (merged operator config → isolated job → node engine → auth.json → UpsertAccount).
// Gated behind POOL_LIVE_REG=1 because it spends the operator's hero-sms balance and
// creates a real OpenAI account. Run:
//
//	POOL_LIVE_REG=1 CODEX_REG_NODE_DIR=/abs/other_new_gpt_register DISPLAY=:0 \
//	  go test -buildvcs=false -run TestLiveNodeRegistration -timeout 20m ./internal/registration/pipeline/
func TestLiveNodeRegistration(t *testing.T) {
	if os.Getenv("POOL_LIVE_REG") != "1" {
		t.Skip("live registration (spends SMS balance, creates a real account) — set POOL_LIVE_REG=1")
	}
	// POOL_LIVE_DB lets the run import into the REAL pool DB (so the account actually
	// joins the pool); default is a throwaway temp DB (chain-only proof).
	dbPath := os.Getenv("POOL_LIVE_DB")
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), "live.sqlite3")
	}
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := NewPipeline(store, nil, nil, nil)
	acct, err := p.nodeRegisterOne(context.Background(), RegisterRequest{Method: "node", GroupName: "cyber"})
	if err != nil {
		t.Fatalf("live registration failed: %v", err)
	}
	t.Logf("✓ registered: id=%s email=%s upstream=%s", acct.ID, acct.Email, acct.UpstreamAccountID)
	tok, err := store.GetToken(context.Background(), acct.ID)
	if err != nil || tok.AccessToken == "" {
		t.Fatalf("token not imported into pool: %v", err)
	}
	t.Logf("✓ auth imported into pool (access_token len=%d, refresh len=%d)", len(tok.AccessToken), len(tok.RefreshToken))
}

// TestNodeRegisterOneOrchestration validates the full Go orchestration of the Node
// registrar WITHOUT a real registration (no SMS spend, no OpenAI signup, no browser):
// a fake "node" binary stands in for the registrar, reading the per-job CONFIG_FILE the
// orchestrator wrote and emitting a token file exactly where the orchestrator expects
// it. This exercises config-building, the subprocess invocation + env, token parsing,
// and account upsert — the migration's wiring end-to-end.
func TestNodeRegisterOneOrchestration(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Fake registrar: validate the per-job isolation fields from $CONFIG_FILE, then
	// write BOTH artifacts — a fallback codex-*-free.json into tokenOutputDir AND the
	// preferred auth.json into cwd (which the orchestrator sets to the isolated job
	// dir). The orchestrator must import auth.json (auth@…), not the token file.
	fake := `#!/usr/bin/env bash
set -e
DIR=$(python3 -c "import json,os;c=json.load(open(os.environ['CONFIG_FILE']));print(c['tokenOutputDir'])")
PROF=$(python3 -c "import json,os;c=json.load(open(os.environ['CONFIG_FILE']));print(c['browserUserDataDir'])")
SEED=$(python3 -c "import json,os;c=json.load(open(os.environ['CONFIG_FILE']));print(c['fingerprintSeed'])")
test -n "$DIR" && test -d "$PROF" && test -n "$SEED" || { echo "missing isolation fields" >&2; exit 3; }
cat > "$DIR/codex-token@example.com-free.json" <<JSON
{"access_token":"tok-access","refresh_token":"tok-refresh","id_token":"tok-id","account_id":"acc-tok","email":"token@example.com","type":"codex"}
JSON
cat > auth.json <<JSON
{"access_token":"auth-access","refresh_token":"auth-refresh","id_token":"auth-id","account_id":"acc-auth","email":"auth@example.com","type":"codex"}
JSON
echo "fake registrar ok seed=$SEED"
`
	regDir := t.TempDir()
	scriptPath := filepath.Join(regDir, "fake-registrar.sh")
	if err := os.WriteFile(scriptPath, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	// Drive nodeRegisterOne at the fake: node binary = bash, entry = the fake script.
	t.Setenv("CODEX_REG_NODE", "bash")
	t.Setenv("CODEX_REG_NODE_ENTRY", scriptPath)
	t.Setenv("CODEX_REG_NODE_DIR", regDir)

	p := NewPipeline(store, nil, nil, nil)
	acct, err := p.nodeRegisterOne(context.Background(), RegisterRequest{Method: "node", GroupName: "cyber"})
	if err != nil {
		t.Fatalf("nodeRegisterOne: %v", err)
	}
	// auth.json must win over the token file.
	if acct.Email != "auth@example.com" || acct.Provider != "codex" {
		t.Fatalf("account = %+v, want auth@example.com/codex (auth.json should be preferred)", acct)
	}
	// Token must be persisted (the just-registered account lands in the pool).
	tok, err := store.GetToken(context.Background(), acct.ID)
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if tok.AccessToken != "auth-access" || tok.RefreshToken != "auth-refresh" {
		t.Fatalf("token = %+v, want auth-access/auth-refresh (from auth.json)", tok)
	}
}
