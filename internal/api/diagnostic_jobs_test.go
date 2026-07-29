package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

func TestDiagnosticJobV3StableAliasesDLPAndRangeDownload(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	accountID := h.importAccount(t, "diagnostic@example.test", "upstream-sensitive-id", "secret-diagnostic-token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.app.startDiagnosticJobLoop(ctx)

	create := func() storage.DiagnosticJob {
		request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/admin/diagnostics/jobs", strings.NewReader(`{}`))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusAccepted {
			t.Fatalf("create status=%d body=%s", response.StatusCode, raw)
		}
		var envelope struct {
			Job storage.DiagnosticJob `json:"job"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			job, getErr := h.store.GetDiagnosticJob(context.Background(), envelope.Job.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			switch job.Status {
			case storage.DiagnosticJobReady:
				return job
			case storage.DiagnosticJobFailed, storage.DiagnosticJobCancelled:
				t.Fatalf("job=%+v", job)
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("diagnostic job did not finish")
		return storage.DiagnosticJob{}
	}

	first := create()
	archive, err := zip.OpenReader(first.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	var firstAlias string
	names := map[string]bool{}
	for _, entry := range archive.File {
		names[entry.Name] = true
		if entry.Name != "accounts_snapshot.csv" {
			continue
		}
		stream, openErr := entry.Open()
		if openErr != nil {
			t.Fatal(openErr)
		}
		rows, readErr := csv.NewReader(stream).ReadAll()
		stream.Close()
		if readErr != nil || len(rows) < 2 {
			t.Fatalf("account snapshot rows=%v err=%v", rows, readErr)
		}
		firstAlias = rows[1][0]
	}
	archive.Close()
	if names["account_map.csv"] || !strings.HasPrefix(firstAlias, "ACC-") {
		t.Fatalf("v3 archive names=%v alias=%q", names, firstAlias)
	}
	rawArchive, err := os.ReadFile(first.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{accountID, "diagnostic@example.test", "upstream-sensitive-id", "secret-diagnostic-token"} {
		if bytes.Contains(rawArchive, []byte(forbidden)) {
			t.Fatalf("artifact contained forbidden value %q", forbidden)
		}
	}

	second := create()
	secondArchive, err := zip.OpenReader(second.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	var secondAlias string
	for _, entry := range secondArchive.File {
		if entry.Name != "accounts_snapshot.csv" {
			continue
		}
		stream, _ := entry.Open()
		rows, _ := csv.NewReader(stream).ReadAll()
		stream.Close()
		if len(rows) > 1 {
			secondAlias = rows[1][0]
		}
	}
	secondArchive.Close()
	if secondAlias != firstAlias {
		t.Fatalf("aliases changed across bundles: %q != %q", firstAlias, secondAlias)
	}

	rangeRequest, _ := http.NewRequest(http.MethodGet, h.pool.URL+"/admin/diagnostics/jobs/"+first.ID+"/download", nil)
	rangeRequest.Header.Set("Range", "bytes=0-9")
	rangeResponse, err := http.DefaultClient.Do(rangeRequest)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody, _ := io.ReadAll(rangeResponse.Body)
	rangeResponse.Body.Close()
	if rangeResponse.StatusCode != http.StatusPartialContent || len(rangeBody) != 10 ||
		rangeResponse.Header.Get("ETag") == "" || rangeResponse.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("range status=%d len=%d headers=%v", rangeResponse.StatusCode, len(rangeBody), rangeResponse.Header)
	}
}
