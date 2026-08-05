package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/storage"
	sqlite3 "github.com/mattn/go-sqlite3"
)

func TestAdminAccountsAssignGroupBatchesAndPreservesCompatibility(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id":"resp"}`)) })
	createAccountGroupForBatchTest(t, h, "batch-target")
	for _, id := range []string{"batch-api-a", "batch-api-b"} {
		seedAccountForBatchTest(t, h, id)
		if err := h.store.SetAccountGroup(context.Background(), id, "cyber"); err != nil {
			t.Fatal(err)
		}
		if err := h.store.AddAccountToGroup(context.Background(), id, "extra-membership"); err != nil {
			t.Fatal(err)
		}
	}
	commits, stopCounting := countSQLiteCommits(t, h.store)

	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/assign-group", `{"ids":[" "," batch-api-a ","missing","batch-api-a","batch-api-b"],"group":" batch-target "}`)
	stopCounting()
	if code != http.StatusOK {
		t.Fatalf("assign group = %d: %s", code, raw)
	}
	var response struct {
		Group           string `json:"group"`
		AccountsUpdated int    `json:"accounts_updated"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.Group != "batch-target" || response.AccountsUpdated != 3 {
		t.Fatalf("response = %+v, want group=batch-target accounts_updated=3", response)
	}
	if got := commits.Load(); got != 1 {
		t.Fatalf("commits = %d, want one batch transaction instead of three per-input transactions", got)
	}
	for _, id := range []string{"batch-api-a", "batch-api-b"} {
		account, err := h.store.GetAccount(context.Background(), id)
		if err != nil || account.GroupName != "batch-target" {
			t.Fatalf("%s account = %+v err=%v", id, account, err)
		}
		groups, err := h.store.GetAccountGroups(context.Background(), id)
		if err != nil || !accountGroupBatchContains(groups, "extra-membership") || !accountGroupBatchContains(groups, "batch-target") {
			t.Fatalf("%s memberships = %v err=%v", id, groups, err)
		}
	}
}

func TestAdminAccountsAssignGroupFallsBackPerItemOnBatchError(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id":"resp"}`)) })
	createAccountGroupForBatchTest(t, h, "fallback-target")
	for _, id := range []string{"fallback-good-a", "fallback-poison", "fallback-good-b"} {
		seedAccountForBatchTest(t, h, id)
		if err := h.store.SetAccountGroup(context.Background(), id, "cyber"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.DB().ExecContext(context.Background(), `
CREATE TRIGGER fail_fallback_poison_group
BEFORE UPDATE OF group_name ON accounts
WHEN OLD.id = 'fallback-poison' AND NEW.group_name = 'fallback-target'
BEGIN
	SELECT RAISE(ABORT, 'synthetic account group failure');
END`); err != nil {
		t.Fatal(err)
	}
	commits, stopCounting := countSQLiteCommits(t, h.store)

	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/assign-group", `{"ids":["fallback-good-a","fallback-poison","missing","fallback-good-a","fallback-good-b"],"group":"fallback-target"}`)
	stopCounting()
	if code != http.StatusOK {
		t.Fatalf("assign group = %d: %s", code, raw)
	}
	var response struct {
		AccountsUpdated int `json:"accounts_updated"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.AccountsUpdated != 3 {
		t.Fatalf("accounts_updated = %d, want duplicate good account counted twice plus one other success", response.AccountsUpdated)
	}
	if got := commits.Load(); got != 3 {
		t.Fatalf("commits = %d, want three successful per-item fallback commits", got)
	}
	for id, wantGroup := range map[string]string{
		"fallback-good-a": "fallback-target",
		"fallback-poison": "cyber",
		"fallback-good-b": "fallback-target",
	} {
		account, err := h.store.GetAccount(context.Background(), id)
		if err != nil || account.GroupName != wantGroup {
			t.Fatalf("%s group = %q err=%v, want %q", id, account.GroupName, err, wantGroup)
		}
	}
}

func TestAccountGroupAssignmentBatchesPreserveOccurrencesAndLimit500(t *testing.T) {
	ids := make([]string, storage.AccountGroupBatchSize*2+2)
	for i := range ids {
		ids[i] = fmt.Sprintf(" account-%04d ", i)
	}
	ids[1] = " "
	ids[storage.AccountGroupBatchSize] = "account-0000"
	batches := accountGroupAssignmentBatches(ids)
	if len(batches) != 3 || len(batches[0]) != 500 || len(batches[1]) != 500 || len(batches[2]) != 1 {
		t.Fatalf("batch sizes = %v, want [500 500 1]", accountGroupBatchLengths(batches))
	}
	if batches[0][0] != "account-0000" || batches[0][storage.AccountGroupBatchSize-1] != "account-0000" {
		t.Fatalf("trimmed duplicate occurrences were not preserved at the batch boundary: first=%q last=%q", batches[0][0], batches[0][storage.AccountGroupBatchSize-1])
	}
}

func createAccountGroupForBatchTest(t *testing.T, h *testHarness, name string) {
	t.Helper()
	code, raw := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"`+name+`"}`)
	if code != http.StatusOK {
		t.Fatalf("create group %q = %d: %s", name, code, raw)
	}
}

func seedAccountForBatchTest(t *testing.T, h *testHarness, id string) {
	t.Helper()
	if err := h.store.UpsertAccount(context.Background(), storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "token-" + id}); err != nil {
		t.Fatal(err)
	}
}

func countSQLiteCommits(t *testing.T, store *storage.Store) (*atomic.Int64, func()) {
	t.Helper()
	var commits atomic.Int64
	setHook := func(callback func() int) {
		conn, err := store.DB().Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.Raw(func(raw interface{}) error {
			sqliteConn, ok := raw.(*sqlite3.SQLiteConn)
			if !ok {
				return fmt.Errorf("driver connection is %T, want *sqlite3.SQLiteConn", raw)
			}
			sqliteConn.RegisterCommitHook(callback)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setHook(func() int {
		commits.Add(1)
		return 0
	})
	stopped := atomic.Bool{}
	stop := func() {
		if stopped.CompareAndSwap(false, true) {
			setHook(nil)
		}
	}
	t.Cleanup(stop)
	return &commits, stop
}

func accountGroupBatchContains(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func accountGroupBatchLengths(batches [][]string) []int {
	lengths := make([]int, len(batches))
	for i := range batches {
		lengths[i] = len(batches[i])
	}
	return lengths
}
