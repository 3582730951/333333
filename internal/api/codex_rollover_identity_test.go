package api

import (
	"testing"

	"codex-account-pool/internal/storage"
)

func TestCodexRolloverFreshIdentityIsDeterministicAndBranchScoped(t *testing.T) {
	rootBinding := storage.CodexSessionBinding{
		ID: "binding-root", RootSessionID: "018f6d46-c018-7c00-8000-000000000001", ThreadID: "018f6d46-c018-7c00-8000-000000000001",
		PendingRolloverNonce: "nonce-root",
	}
	rootOne, threadOne := codexRolloverFreshIdentity(rootBinding)
	rootTwo, threadTwo := codexRolloverFreshIdentity(rootBinding)
	if rootOne == rootBinding.RootSessionID || rootOne != rootTwo || threadOne != rootOne || threadTwo != threadOne {
		t.Fatalf("root rollover identity was not deterministic and fresh: first=(%q,%q) second=(%q,%q)", rootOne, threadOne, rootTwo, threadTwo)
	}

	childBinding := rootBinding
	childBinding.ThreadID = "018f6d46-c019-7c00-8000-000000000002"
	childBinding.PendingRolloverNonce = "nonce-child"
	childRootOne, childThreadOne := codexRolloverFreshIdentity(childBinding)
	childRootTwo, childThreadTwo := codexRolloverFreshIdentity(childBinding)
	if childRootOne != childBinding.RootSessionID || childRootOne != childRootTwo || childThreadOne == childBinding.ThreadID || childThreadOne != childThreadTwo {
		t.Fatalf("child rollover did not preserve root and deterministically rotate branch: first=(%q,%q) second=(%q,%q)", childRootOne, childThreadOne, childRootTwo, childThreadTwo)
	}
}
