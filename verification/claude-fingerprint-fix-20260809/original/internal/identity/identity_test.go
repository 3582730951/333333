package identity

import (
	"regexp"
	"strings"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var uuidV7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestDeterministic(t *testing.T) {
	a := For(nil, "acc_123")
	b := For(nil, "acc_123")
	if a != b {
		t.Fatal("identity not deterministic for the same account")
	}
}

func TestDistinctPerAccount(t *testing.T) {
	a := For(nil, "acc_123")
	b := For(nil, "acc_456")
	if a.SessionID == b.SessionID || a.UserID == b.UserID || a.MachineID == b.MachineID {
		t.Fatal("identities collide across accounts")
	}
}

func TestSecretChangesIdentity(t *testing.T) {
	a := For([]byte("secret-one"), "acc_123")
	b := For([]byte("secret-two"), "acc_123")
	if a.SessionID == b.SessionID {
		t.Fatal("identity did not change with the secret")
	}
}

func TestResolveSecret(t *testing.T) {
	// A configured secret is used verbatim.
	if got := ResolveSecret([]byte("explicit")); string(got) != "explicit" {
		t.Fatalf("configured secret not honored: %q", got)
	}
	// Empty config resolves to a stable, non-empty secret (host-derived when a host
	// id exists, else the package default) and is deterministic across calls.
	a := ResolveSecret(nil)
	b := ResolveSecret([]byte(""))
	if len(a) == 0 || string(a) != string(b) {
		t.Fatalf("empty-config secret unstable: %q vs %q", a, b)
	}
	// When a host seed is present, the derived deployment secret must differ from the
	// shared constant default (the whole point: identities not predictable across
	// installs). When no host id is available it equals DefaultSecret — both are
	// acceptable; assert only that it is deterministic and usable.
	if string(a) == "" {
		t.Fatal("resolved secret must never be empty")
	}
}

func TestUUIDShape(t *testing.T) {
	id := For(nil, "acc_xyz")
	for _, u := range []string{id.SessionID, id.ClaudeSessionID, id.MachineID} {
		if !uuidRe.MatchString(u) {
			t.Fatalf("not a v4 uuid: %q", u)
		}
	}
}

func TestUserIDShape(t *testing.T) {
	id := For(nil, "acc_xyz")
	if len(id.UserID) != 64 {
		t.Fatalf("user id len = %d, want 64", len(id.UserID))
	}
}

func TestUserAgents(t *testing.T) {
	id := For(nil, "acc_ua")
	cua := id.CodexUserAgent()
	if !strings.HasPrefix(cua, "codex_cli_rs/"+CodexCLIVersion+" (") {
		t.Fatalf("unexpected codex UA: %q", cua)
	}
	if !strings.Contains(cua, id.OSName) || !strings.Contains(cua, id.Arch) || !strings.Contains(cua, id.Terminal) {
		t.Fatalf("codex UA missing profile fields: %q", cua)
	}
	execUA := id.CodexUserAgentExecVersion(CodexCLIVersion)
	if !strings.HasPrefix(execUA, "codex_exec/"+CodexCLIVersion+" (") || !strings.Contains(execUA, id.Terminal) {
		t.Fatalf("unexpected codex exec UA: %q", execUA)
	}
	clua := id.ClaudeUserAgent()
	if clua != "claude-cli/"+ClaudeCLIVersion+" (external, cli)" {
		t.Fatalf("unexpected claude UA: %q", clua)
	}
}

func TestEnvFieldsConsistent(t *testing.T) {
	id := For(nil, "acc_env")
	if id.Username == "" || id.Hostname == "" || id.HomeDir == "" {
		t.Fatal("env fields not populated")
	}
	if !strings.Contains(id.Hostname, id.Username) {
		t.Fatalf("hostname %q should contain username %q", id.Hostname, id.Username)
	}
	if !strings.HasSuffix(id.HomeDir, id.Username) {
		t.Fatalf("home dir %q should end with username %q", id.HomeDir, id.Username)
	}
	// macOS profiles use /Users, Linux uses /home — must be consistent with OS.
	if id.OSName == "Linux" && !strings.HasPrefix(id.HomeDir, "/home/") {
		t.Fatalf("linux home dir should be under /home: %q", id.HomeDir)
	}
	if id.OSName == "Mac OS" && !strings.HasPrefix(id.HomeDir, "/Users/") {
		t.Fatalf("macOS home dir should be under /Users: %q", id.HomeDir)
	}
}

func TestForOSAndDerived(t *testing.T) {
	if ForOS(nil, "acc1", "darwin").OSName != "Mac OS" {
		t.Fatal("ForOS darwin should present Mac OS")
	}
	if ForOS(nil, "acc1", "linux").OSName != "Linux" {
		t.Fatal("ForOS linux should present Linux")
	}
	if ForOS(nil, "acc1", "plan9").OSName == "" {
		t.Fatal("unknown OS should fall back to the detected host")
	}
	a1 := DerivedUUID("seedA", "x")
	a2 := DerivedUUID("seedA", "x")
	b := DerivedUUID("seedB", "x")
	if a1 != a2 {
		t.Fatal("DerivedUUID must be deterministic")
	}
	if a1 == b {
		t.Fatal("DerivedUUID must differ per seed (per account)")
	}
	if !uuidRe.MatchString(a1) {
		t.Fatalf("DerivedUUID not v4-shaped: %s", a1)
	}
}

func TestDerivedUUIDv7PreservesTimestampAndNamespacesRandomBits(t *testing.T) {
	const raw = "019f4a5e-8611-79a2-83ef-4df5ab75b715"
	a1 := DerivedUUIDv7("account-a", raw)
	a2 := DerivedUUIDv7("account-a", raw)
	b := DerivedUUIDv7("account-b", raw)
	if a1 != a2 {
		t.Fatal("DerivedUUIDv7 must be deterministic")
	}
	if a1 == b {
		t.Fatal("DerivedUUIDv7 must differ per account seed")
	}
	if !uuidV7Re.MatchString(a1) || !uuidV7Re.MatchString(b) {
		t.Fatalf("derived values are not UUIDv7: %q %q", a1, b)
	}
	wantMillis, _ := uuidV7TimestampMillis(raw)
	gotMillis, ok := uuidV7TimestampMillis(a1)
	if !ok || gotMillis != wantMillis {
		t.Fatalf("UUIDv7 timestamp changed: got %d want %d", gotMillis, wantMillis)
	}
	if a1[13:] == raw[13:] {
		t.Fatalf("UUIDv7 identifying bits were not replaced: %q", a1)
	}
}

func TestDerivedUUIDv7AtUsesExplicitTimestampForOpaqueInput(t *testing.T) {
	const wantMillis = int64(1783659136531)
	got := DerivedUUIDv7At("account-a", "opaque-turn-id", wantMillis)
	if !uuidV7Re.MatchString(got) {
		t.Fatalf("DerivedUUIDv7At is not UUIDv7: %q", got)
	}
	gotMillis, ok := uuidV7TimestampMillis(got)
	if !ok || gotMillis != wantMillis {
		t.Fatalf("UUIDv7 timestamp = %d, want %d", gotMillis, wantMillis)
	}
	if repeat := DerivedUUIDv7At("account-a", "opaque-turn-id", wantMillis); repeat != got {
		t.Fatalf("DerivedUUIDv7At is not deterministic: %q vs %q", got, repeat)
	}
}
