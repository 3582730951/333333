package identity

import (
	"reflect"
	"testing"
)

func convergedDeviceProjection(id Identity) Identity {
	id.AccountID = ""
	id.SessionNamespaceSeed = ""
	id.SessionID = ""
	id.ClaudeSessionID = ""
	return id
}

func TestFullDeviceConvergenceKeepsLogicalSessionsIsolated(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	left := ForOSWithConvergence(secret, "account-left", "Mac OS", "full")
	right := ForOSWithConvergence(secret, "account-right", "Windows", "full")

	if !reflect.DeepEqual(convergedDeviceProjection(left), convergedDeviceProjection(right)) {
		t.Fatalf("full convergence did not converge device profile: left=%+v right=%+v", left, right)
	}
	if left.SessionID == right.SessionID || left.ClaudeSessionID == right.ClaudeSessionID {
		t.Fatalf("full convergence crossed the logical session boundary: left=%+v right=%+v", left, right)
	}
	if SessionSeed(left) == SessionSeed(right) {
		t.Fatal("conversation namespace seed converged across credentials")
	}

	offLeft := ForOSWithConvergence(secret, "account-left", "Mac OS", "off")
	offRight := ForOSWithConvergence(secret, "account-right", "Mac OS", "off")
	if offLeft.MachineID == offRight.MachineID {
		t.Fatal("default-off mode unexpectedly converged devices")
	}

	fullDeviceA := CodexDeviceWithConvergence(secret, "account-left", "egress-a", "Linux", "full")
	fullDeviceB := CodexDeviceWithConvergence(secret, "account-right", "egress-b", "Mac OS", "full")
	if !reflect.DeepEqual(convergedDeviceProjection(fullDeviceA), convergedDeviceProjection(fullDeviceB)) {
		t.Fatalf("Codex installation did not fully converge: left=%+v right=%+v", fullDeviceA, fullDeviceB)
	}
	if fullDeviceA.SessionID == fullDeviceB.SessionID || SessionSeed(fullDeviceA) == SessionSeed(fullDeviceB) {
		t.Fatal("Codex device convergence crossed the logical account/egress session boundary")
	}
	offDeviceA := CodexDeviceWithConvergence(secret, "account-left", "egress-a", "Linux", "off")
	offDeviceB := CodexDeviceWithConvergence(secret, "account-left", "egress-b", "Linux", "off")
	if offDeviceA.MachineID == offDeviceB.MachineID {
		t.Fatal("default-off Codex installations crossed egress boundaries")
	}
}

func TestAccountConvergenceGivesOneDevicePerAccountAcrossEgress(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")

	// The reported failure mode: an account that rotates exit IP must keep ONE
	// installation id. Under the legacy mode each exit minted a new virtual device,
	// so changing IP to escape a risk flag widened the account's device spread.
	first := CodexDeviceWithConvergence(secret, "account-a", "egress-a", "diverse", "account")
	second := CodexDeviceWithConvergence(secret, "account-a", "egress-b", "diverse", "account")
	if first.MachineID != second.MachineID {
		t.Fatalf("changing egress minted a new installation id: %q then %q", first.MachineID, second.MachineID)
	}
	// The whole device profile must travel together — an account whose OS or terminal
	// changed with its exit IP is just as inconsistent as a new installation id.
	if !reflect.DeepEqual(convergedDeviceProjection(first), convergedDeviceProjection(second)) {
		t.Fatalf("device profile drifted with egress: %+v then %+v", first, second)
	}

	// Distinct accounts must still look like distinct devices: converging them would
	// present one machine running many credentials, which is its own risk signal.
	other := CodexDeviceWithConvergence(secret, "account-b", "egress-a", "diverse", "account")
	if other.MachineID == first.MachineID {
		t.Fatal("two accounts collapsed onto one installation id")
	}
	if SessionSeed(first) == SessionSeed(other) {
		t.Fatal("conversation namespace seed collapsed across accounts")
	}

	// An unset mode must converge rather than fall back to the exit-scoped device.
	if CodexDeviceWithConvergence(secret, "account-a", "egress-c", "diverse", "").MachineID != first.MachineID {
		t.Fatal("unset mode did not converge to the account device")
	}
	// Explicit off is still available for deployments that want an exit-scoped device.
	if CodexDeviceWithConvergence(secret, "account-a", "egress-b", "diverse", "off").MachineID == first.MachineID {
		t.Fatal("explicit off mode unexpectedly converged")
	}
}
