package identity

import (
	"regexp"
	"strings"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

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
