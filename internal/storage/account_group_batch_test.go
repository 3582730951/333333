package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func TestSetAccountsGroupPreservesMembershipSemantics(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"batch-a", "batch-b"} {
		if err := store.UpsertAccount(ctx, Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, AccountToken{AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
		if err := store.SetAccountGroup(ctx, id, "old-primary"); err != nil {
			t.Fatal(err)
		}
		if err := store.AddAccountToGroup(ctx, id, "extra-membership"); err != nil {
			t.Fatal(err)
		}
	}

	updated, err := store.SetAccountsGroup(ctx, []string{" batch-a ", "missing", "batch-a", "batch-b", " "}, " new-primary ")
	if err != nil {
		t.Fatalf("SetAccountsGroup: %v", err)
	}
	updatedSet := make(map[string]bool, len(updated))
	for _, id := range updated {
		updatedSet[id] = true
	}
	if len(updatedSet) != 2 || !updatedSet["batch-a"] || !updatedSet["batch-b"] {
		t.Fatalf("updated IDs = %v, want batch-a and batch-b", updated)
	}
	if updatedAgain, err := store.SetAccountsGroup(ctx, []string{"batch-a", "batch-b"}, "new-primary"); err != nil || len(updatedAgain) != 2 {
		t.Fatalf("idempotent SetAccountsGroup updated=%v err=%v, want both accounts", updatedAgain, err)
	}

	for _, id := range []string{"batch-a", "batch-b"} {
		account, err := store.GetAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if account.GroupName != "new-primary" {
			t.Fatalf("%s group = %q, want new-primary", id, account.GroupName)
		}
		memberships := accountGroupMembershipState(t, store, id)
		want := map[string]int{"extra-membership": 0, "new-primary": 1, "old-primary": 0}
		if !reflect.DeepEqual(memberships, want) {
			t.Fatalf("%s memberships = %v, want %v", id, memberships, want)
		}
	}
	if _, err := store.GetAccount(ctx, "missing"); err != sql.ErrNoRows {
		t.Fatalf("missing account error = %v, want sql.ErrNoRows", err)
	}
}

func accountGroupMembershipState(t *testing.T, store *Store, accountID string) map[string]int {
	t.Helper()
	rows, err := store.DB().Query(`SELECT group_name, is_primary FROM account_group_memberships WHERE account_id = ? ORDER BY group_name`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make(map[string]int)
	for rows.Next() {
		var group string
		var primary int
		if err := rows.Scan(&group, &primary); err != nil {
			t.Fatal(err)
		}
		got[group] = primary
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSetAccountsGroupRejectsMoreThan500UniqueIDs(t *testing.T) {
	store := newTestStore(t)
	ids := make([]string, AccountGroupBatchSize+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("account-%03d", i)
	}
	if _, err := store.SetAccountsGroup(context.Background(), ids, "target"); err == nil || !strings.Contains(err.Error(), "maximum is 500") {
		t.Fatalf("oversized batch error = %v", err)
	}
}

func TestSetAccountsGroupRollsBackOnAffectedRowMismatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"mismatch-good", "mismatch-skipped"} {
		if err := store.UpsertAccount(ctx, Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, AccountToken{AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
		if err := store.SetAccountGroup(ctx, id, "old-primary"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `
CREATE TRIGGER skip_one_account_group_update
BEFORE UPDATE OF group_name ON accounts
WHEN OLD.id = 'mismatch-skipped' AND NEW.group_name = 'new-primary'
BEGIN
	SELECT RAISE(IGNORE);
END`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.SetAccountsGroup(ctx, []string{"mismatch-good", "mismatch-skipped"}, "new-primary"); err == nil || !strings.Contains(err.Error(), "updated 1 of 2") {
		t.Fatalf("affected-row mismatch error = %v", err)
	}
	for _, id := range []string{"mismatch-good", "mismatch-skipped"} {
		account, err := store.GetAccount(ctx, id)
		if err != nil || account.GroupName != "old-primary" {
			t.Fatalf("%s group = %q err=%v, want rolled-back old-primary", id, account.GroupName, err)
		}
		memberships := accountGroupMembershipState(t, store, id)
		_, hasNewPrimary := memberships["new-primary"]
		if memberships["old-primary"] != 1 || hasNewPrimary {
			t.Fatalf("%s memberships = %v, want old primary retained", id, memberships)
		}
	}
}

func TestSetAccountsGroupReducesTransactionsAndQueries(t *testing.T) {
	store, counts := newCountingAccountGroupStore(t)
	ctx := context.Background()
	ids := make([]string, AccountGroupBatchSize+1)
	seed, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		ids[i] = fmt.Sprintf("counted-%03d", i)
		if _, err := seed.ExecContext(ctx, `INSERT INTO accounts(id, label, created_at, updated_at) VALUES(?, ?, 1, 1)`, ids[i], ids[i]); err != nil {
			_ = seed.Rollback()
			t.Fatal(err)
		}
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	counts.reset()

	for start := 0; start < len(ids); start += AccountGroupBatchSize {
		end := start + AccountGroupBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		updated, err := store.SetAccountsGroup(ctx, ids[start:end], "target")
		if err != nil {
			t.Fatal(err)
		}
		if len(updated) != end-start {
			t.Fatalf("batch updated %d IDs, want %d", len(updated), end-start)
		}
	}

	if got := counts.begins.Load(); got != 2 {
		t.Fatalf("transactions begun = %d, want 2 batches instead of %d per-account transactions", got, len(ids))
	}
	if got := counts.commits.Load(); got != 2 {
		t.Fatalf("transactions committed = %d, want 2", got)
	}
	if got := counts.queries.Load(); got != 2 {
		t.Fatalf("existence queries = %d, want 2", got)
	}
	if got := counts.execs.Load(); got != 6 {
		t.Fatalf("write statements = %d, want 6 instead of %d", got, 3*len(ids))
	}
}

func TestAccountGroupBatchQueriesRebindForPostgres(t *testing.T) {
	queries := buildAccountGroupBatchQueries(2)
	tests := []struct {
		name, query, want string
	}{
		{"existing", queries.existing, "IN ($1,$2)"},
		{"accounts", queries.updateAccounts, "updated_at = $2 WHERE id IN ($3,$4)"},
		{"demote", queries.demoteMemberships, "IN ($1,$2)"},
		{"upsert", queries.upsertMemberships, "SELECT id, $1, 1, $2 FROM accounts WHERE id IN ($3,$4)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := rebindPostgres(test.query)
			if strings.Contains(got, "?") || !strings.Contains(got, test.want) {
				t.Fatalf("rebound query = %q, want fragment %q and no question marks", got, test.want)
			}
		})
	}
}

type accountGroupQueryCounts struct {
	begins  atomic.Int64
	commits atomic.Int64
	execs   atomic.Int64
	queries atomic.Int64
}

func (c *accountGroupQueryCounts) reset() {
	c.begins.Store(0)
	c.commits.Store(0)
	c.execs.Store(0)
	c.queries.Store(0)
}

type accountGroupCountingDriver struct {
	driver.Driver
	counts *accountGroupQueryCounts
}

func (d *accountGroupCountingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return &accountGroupCountingConn{Conn: conn, counts: d.counts}, nil
}

type accountGroupCountingConn struct {
	driver.Conn
	counts *accountGroupQueryCounts
}

func (c *accountGroupCountingConn) Begin() (driver.Tx, error) {
	c.counts.begins.Add(1)
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return &accountGroupCountingTx{Tx: tx, counts: c.counts}, nil
}

func (c *accountGroupCountingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.counts.begins.Add(1)
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &accountGroupCountingTx{Tx: tx, counts: c.counts}, nil
}

func (c *accountGroupCountingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.counts.execs.Add(1)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *accountGroupCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.counts.queries.Add(1)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

type accountGroupCountingTx struct {
	driver.Tx
	counts *accountGroupQueryCounts
}

func (tx *accountGroupCountingTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	tx.counts.commits.Add(1)
	return nil
}

var accountGroupCountingDriverID atomic.Uint64

func newCountingAccountGroupStore(t *testing.T) (*Store, *accountGroupQueryCounts) {
	t.Helper()
	counts := &accountGroupQueryCounts{}
	driverName := fmt.Sprintf("account_group_counting_sqlite_%d", accountGroupCountingDriverID.Add(1))
	sql.Register(driverName, &accountGroupCountingDriver{Driver: &sqlite3.SQLiteDriver{}, counts: counts})
	path := filepath.Join(t.TempDir(), "pool.sqlite3")
	db, err := sql.Open(driverName, path+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{path: path, driver: "sqlite", db: db, rdb: db}
	if err := store.Init(context.Background()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, counts
}
