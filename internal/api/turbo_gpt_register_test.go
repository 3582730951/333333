package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminTurboGPTRegisterJobAndConfigCRUD(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/turbo-gpt-register/jobs", `{
		"full_name":"Example User","birth_date":"1998-01-01","auto_import":true
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create turbo job = %d: %s", code, raw)
	}
	var job storage.TurboGPTRegisterJob
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || job.Phase != "phase1" || job.Status != "pending" {
		t.Fatalf("unexpected created job: %+v", job)
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/turbo-gpt-register/jobs", "")
	if code != http.StatusOK {
		t.Fatalf("list turbo jobs = %d: %s", code, raw)
	}
	var list struct {
		Jobs []storage.TurboGPTRegisterJob `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || len(list.Jobs) != 1 {
		t.Fatalf("unexpected list: err=%v body=%s", err, raw)
	}

	code, raw = grpReq(t, h, http.MethodPut, "/admin/turbo-gpt-register/config", `{"mail_domain":"example.com"}`)
	if code != http.StatusOK {
		t.Fatalf("put turbo config = %d: %s", code, raw)
	}
	code, raw = grpReq(t, h, http.MethodGet, "/admin/turbo-gpt-register/config", "")
	if code != http.StatusOK || !json.Valid(raw) {
		t.Fatalf("get turbo config = %d: %s", code, raw)
	}

	code, raw = grpReq(t, h, http.MethodDelete, "/admin/turbo-gpt-register/jobs/"+job.ID, "")
	if code != http.StatusOK {
		t.Fatalf("delete turbo job = %d: %s", code, raw)
	}
}
