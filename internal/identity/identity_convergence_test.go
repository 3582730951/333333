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
