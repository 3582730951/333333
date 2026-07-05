package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeTestJSON(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectJSONFilesHonorsRecursiveOption(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, "root.json"), `{"access_token":"root"}`)
	writeTestJSON(t, filepath.Join(dir, "nested", "child.json"), `{"access_token":"child"}`)
	if err := os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	nonRecursive, err := collectJSONFiles(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := baseNames(nonRecursive), []string{"root.json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("non-recursive files = %v, want %v", got, want)
	}

	recursive, err := collectJSONFiles(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := baseNames(recursive), []string{"child.json", "root.json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recursive files = %v, want %v", got, want)
	}
}

func TestRunImportPostsJSONFilesWithEgressAndFilenameLabel(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, "worker-01.json"), `{"access_token":"secret-token","account_id":"up-worker-01"}`)

	var captured map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/account-pool/import" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer poolimp_secret" {
			t.Fatalf("authorization = %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &captured); err != nil {
			t.Fatalf("decode request: %v (%s)", err, raw)
		}
		_, _ = w.Write([]byte(`{"import_status":"imported","id":"acc-worker-01"}`))
	}))
	defer srv.Close()

	summary, err := runImport(context.Background(), importConfig{
		PoolURL:   srv.URL,
		APIKey:    "poolimp_secret",
		JSONDir:   dir,
		GroupName: "cyber",
		EgressID:  "egress_alt",
	}, srv.Client(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Imported != 1 || summary.Duplicate != 0 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if captured["label"] != "worker-01" || captured["group_name"] != "cyber" || captured["egress_id"] != "egress_alt" {
		t.Fatalf("request metadata = %v", captured)
	}
	if captured["auth_json_text"] != `{"access_token":"secret-token","account_id":"up-worker-01"}` {
		t.Fatalf("auth_json_text changed: %v", captured["auth_json_text"])
	}
}

func TestRunImportContinuesAfterDuplicateAndHTTPFailure(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, "duplicate.json"), `{"access_token":"dup"}`)
	writeTestJSON(t, filepath.Join(dir, "failed.json"), `{"access_token":"fail"}`)
	writeTestJSON(t, filepath.Join(dir, "imported.json"), `{"access_token":"ok"}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)
		switch req["label"] {
		case "duplicate":
			_, _ = w.Write([]byte(`{"duplicate":true,"import_status":"duplicate"}`))
		case "failed":
			http.Error(w, `{"error":"boom"}`, http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte(`{"import_status":"imported"}`))
		}
	}))
	defer srv.Close()

	var out strings.Builder
	summary, err := runImport(context.Background(), importConfig{
		PoolURL: srv.URL,
		APIKey:  "poolimp_secret",
		JSONDir: dir,
	}, srv.Client(), &out)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Imported != 1 || summary.Duplicate != 1 || summary.Failed != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if !strings.Contains(out.String(), "failed.json") || !strings.Contains(out.String(), "502") {
		t.Fatalf("failure output should include file and status, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "poolimp_secret") || strings.Contains(out.String(), "access_token") {
		t.Fatalf("output leaked sensitive content:\n%s", out.String())
	}
}

func TestRunImportReportsScannedFilesAndNonRecursiveHint(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, filepath.Join(dir, "root.json"), `{"access_token":"root"}`)
	writeTestJSON(t, filepath.Join(dir, "nested", "child.json"), `{"access_token":"child"}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"import_status":"imported"}`))
	}))
	defer srv.Close()

	var out strings.Builder
	summary, err := runImport(context.Background(), importConfig{
		PoolURL:   srv.URL,
		APIKey:    "poolimp_secret",
		JSONDir:   dir,
		Recursive: false,
	}, srv.Client(), &out)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Imported != 1 || summary.Duplicate != 0 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	got := out.String()
	for _, want := range []string{"扫描到 1 个 JSON 文件", "子目录里还有 1 个 JSON 文件", "递归", "root.json"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "child.json") {
		t.Fatalf("non-recursive import should not process child.json, got:\n%s", got)
	}
}

func baseNames(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	return out
}
