package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

// upstreamErrorRuleRows emitted 24 values under a 22-name header, so an exported package
// carried description=false and created_at=true while the real timestamps fell off the end
// of the row. The header was also duplicated verbatim at two call sites. This pins the
// widths against the one shared list so a field added to the builder cannot drift again.
func TestUpstreamErrorRuleRowWidthMatchesItsHeader(t *testing.T) {
	rows := upstreamErrorRuleRows([]storage.UpstreamErrorRule{{ID: "r1", Name: "r1"}})
	if len(rows) != 1 {
		t.Fatalf("row count = %d, want 1", len(rows))
	}
	if len(rows[0]) != len(upstreamErrorRuleColumns) {
		t.Fatalf("row emits %d values under %d column names; the two lists have drifted",
			len(rows[0]), len(upstreamErrorRuleColumns))
	}
	// The names that were missing. Their absence is what shifted every later column, so a
	// silent reorder that drops them again has to fail here too.
	for _, name := range []string{"filter_account_action", "keyword_case_sensitive"} {
		found := false
		for _, column := range upstreamErrorRuleColumns {
			if column == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("column %q missing from the header", name)
		}
	}
}

// csvString writes each row at whatever width it was handed, and encoding/csv's Writer --
// unlike its Reader -- does not enforce a field count. So a builder that drifts from its
// header produces a malformed table with no error anywhere. This walks a real exported
// package and checks every table.
//
// The test states its own coverage on purpose: a header-only table trivially satisfies a
// width check, so counting a table as verified when it has no rows would make this pass
// for a defect it never looked at. Tables with no rows are listed, and the populated count
// is pinned so losing coverage fails instead of quietly narrowing the check.
func TestExportedCSVRowWidthsMatchTheirHeaders(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-chat"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dsChatResp))
	})
	setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	// The table whose schema actually broke. Without a row here the invariant would be
	// checked against a header-only file and the original defect would pass.
	if err := h.store.UpsertUpstreamErrorRule(context.Background(), storage.UpstreamErrorRule{
		ID: "rule-width-1", Name: "rule-width-1", Enabled: true, Priority: 100,
		Providers: []string{"chatgpt"}, Entrypoints: []string{"responses"},
		MatchMode: "any", AccountAction: "none", DownstreamAction: "hide_safety_buffering",
		ResponseStatus: http.StatusServiceUnavailable, CooldownSeconds: 1800,
		PreferRetryAfter: true, IdleSeconds: 60, IdlePingSeconds: 15,
		FilterAccountAction: true, KeywordCaseSensitive: true, Description: "width fixture",
	}); err != nil {
		t.Fatal(err)
	}

	// usage_records.csv is the widest table in the package at 54 columns, which makes it the
	// likeliest to drift and the most valuable to cover. Inserted synchronously rather than
	// waited for: the request path writes it asynchronously, so relying on traffic would
	// make coverage depend on whether the writer happened to flush before the snapshot.
	if err := h.store.InsertUsageRecordWithDiagnostics(context.Background(),
		"width-fixture-account", "route-key-hash", "api-key-hash", "user-1", "deepseek-chat",
		32248, 512, 32760, 28327, 28327, 0, nil, storage.UsageDiagnostics{},
	); err != nil {
		t.Fatal(err)
	}

	// Drive traffic so the request-scoped tables carry rows too.
	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// The same frozen snapshot the production job uses. Exporting straight from the live
	// store races the asynchronous usage writer, and the export's own row-count guard
	// rejects that -- correctly, which is why the snapshot exists.
	snapshot, err := h.store.BeginDiagnosticSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	snapshotStore, err := snapshot.Store(h.store)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := h.app.writeDiagnosticsExport(context.Background(), &buf, snapshot.ID(), snapshotStore); err != nil {
		t.Fatalf("export: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("open exported zip: %v", err)
	}

	var empty, populated []string
	for _, file := range reader.File {
		if !strings.HasSuffix(file.Name, ".csv") {
			continue
		}
		opened, err := file.Open()
		if err != nil {
			t.Fatalf("%s: open: %v", file.Name, err)
		}
		content, err := io.ReadAll(opened)
		_ = opened.Close()
		if err != nil {
			t.Fatalf("%s: read: %v", file.Name, err)
		}
		// FieldsPerRecord -1 disables the reader's own width check so this test reports the
		// mismatch itself rather than surfacing it as an opaque parse error.
		cr := csv.NewReader(bytes.NewReader(content))
		cr.FieldsPerRecord = -1
		records, err := cr.ReadAll()
		if err != nil {
			t.Fatalf("%s: parse: %v", file.Name, err)
		}
		if len(records) == 0 {
			t.Errorf("%s: no header row", file.Name)
			continue
		}
		width := len(records[0])
		for index, record := range records[1:] {
			if len(record) != width {
				t.Errorf("%s: row %d has %d values under %d column names: %q",
					file.Name, index+1, len(record), width, record)
			}
		}
		if len(records) == 1 {
			empty = append(empty, file.Name)
		} else {
			populated = append(populated, file.Name)
		}
	}

	sort.Strings(empty)
	sort.Strings(populated)
	t.Logf("width-verified with rows (%d): %s", len(populated), strings.Join(populated, " "))
	t.Logf("header only, NOT width-verified (%d): %s", len(empty), strings.Join(empty, " "))

	if len(populated) == 0 {
		t.Fatal("no table carried a data row, so the width invariant verified nothing")
	}
	// Coverage floor set to what this fixture actually reaches. Raise it when a table gains
	// fixture rows; a drop means the export stopped emitting rows and the check silently
	// narrowed to fewer tables than it used to verify.
	const minPopulated = 12
	if len(populated) < minPopulated {
		t.Errorf("only %d of %d tables carried rows (floor %d): %s",
			len(populated), len(populated)+len(empty), minPopulated, strings.Join(populated, " "))
	}
	for _, name := range populated {
		if name == "upstream_error_rules.csv" {
			return
		}
	}
	t.Error("upstream_error_rules.csv had no rows, so the defect this test exists for was not checked")
}
