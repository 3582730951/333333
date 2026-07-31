package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

func teamLifecycleRequest(t *testing.T, client *http.Client, method, url string, body interface{}, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, responseBody
}

func TestTeamLifecycleWorkspaceAutoFillsNativeConnectorAndWorkspaceReference(t *testing.T) {
	harness := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	if err := harness.store.UpsertAccount(context.Background(), storage.Account{
		ID: "parent-account", Label: "Parent", Email: "parent@example.com",
		UpstreamAccountID: "team-upstream-ref", Provider: "codex", Status: "active",
	}, storage.AccountToken{AccountID: "parent-account", AccessToken: "fixture-access"}); err != nil {
		t.Fatal(err)
	}
	response, body := teamLifecycleRequest(t, harness.pool.Client(), http.MethodPost,
		harness.pool.URL+"/admin/team-lifecycle/workspaces",
		map[string]interface{}{
			"name": "Auto room", "parent_account_id": "parent-account", "max_members": 6,
		}, nil)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	var workspace storage.TeamWorkspace
	if err := json.Unmarshal(body, &workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.WorkspaceRef != "team-upstream-ref" || workspace.ConnectorKind != "native" {
		t.Fatalf("workspace=%+v", workspace)
	}
}

func TestTeamLifecycleAdminAPIIdempotencyAndNoSecretFields(t *testing.T) {
	harness := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	client := harness.pool.Client()

	response, body := teamLifecycleRequest(t, client, http.MethodPost,
		harness.pool.URL+"/admin/team-lifecycle/workspaces",
		map[string]interface{}{
			"id": "workspace-fixture", "name": "Fixture room",
			"parent_account_id": "parent-ref", "workspace_ref": "remote-ref",
			"connector_kind": "fixture", "max_members": 8,
		}, nil)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("workspace status=%d body=%s", response.StatusCode, body)
	}

	payload := map[string]interface{}{
		"workspace_id": "workspace-fixture", "parent_account_id": "parent-ref",
		"child_account_id": "child-ref", "rotate_threshold_percent": 1,
		"shadow_mode": true,
	}
	headers := map[string]string{"Idempotency-Key": "fixture-cycle-api"}
	response, body = teamLifecycleRequest(t, client, http.MethodPost,
		harness.pool.URL+"/admin/team-lifecycle/workflows", payload, headers)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("workflow status=%d body=%s", response.StatusCode, body)
	}
	var first struct {
		Created  bool `json:"created"`
		Workflow struct {
			ID         string `json:"id"`
			ShadowMode bool   `json:"shadow_mode"`
		} `json:"workflow"`
	}
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	if !first.Created || !first.Workflow.ShadowMode || first.Workflow.ID == "" {
		t.Fatalf("first response=%s", body)
	}
	for _, forbidden := range [][]byte{
		[]byte("access_token"), []byte("refresh_token"), []byte("phone_number"), []byte("password"),
	} {
		if bytes.Contains(body, forbidden) {
			t.Fatalf("response exposed forbidden field %q: %s", forbidden, body)
		}
	}

	response, body = teamLifecycleRequest(t, client, http.MethodPost,
		harness.pool.URL+"/admin/team-lifecycle/workflows", payload, headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("repeat status=%d body=%s", response.StatusCode, body)
	}
	var second struct {
		Created  bool `json:"created"`
		Workflow struct {
			ID string `json:"id"`
		} `json:"workflow"`
	}
	if err := json.Unmarshal(body, &second); err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Workflow.ID != first.Workflow.ID {
		t.Fatalf("idempotent response=%s first=%+v", body, first)
	}

	response, body = teamLifecycleRequest(t, client, http.MethodGet,
		harness.pool.URL+"/admin/team-lifecycle/workflows/"+first.Workflow.ID+"/events",
		nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"event_type":"created"`)) {
		t.Fatalf("events status=%d body=%s", response.StatusCode, body)
	}

	response, body = teamLifecycleRequest(t, client, http.MethodGet,
		harness.pool.URL+"/admin/team-lifecycle/stats", nil, nil)
	if response.StatusCode != http.StatusOK ||
		!bytes.Contains(body, []byte(`"credential_persistence":"encrypted_account_reference"`)) ||
		!bytes.Contains(body, []byte(`"lease_heartbeat":true`)) {
		t.Fatalf("stats status=%d body=%s", response.StatusCode, body)
	}
}

func TestTeamLifecycleAPIRequiresIdempotencyKey(t *testing.T) {
	harness := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	response, body := teamLifecycleRequest(t, harness.pool.Client(), http.MethodPost,
		harness.pool.URL+"/admin/team-lifecycle/workflows",
		map[string]interface{}{
			"workspace_id": "workspace-fixture", "parent_account_id": "parent-ref",
			"child_account_id": "child-ref",
		}, nil)
	if response.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"invalid_request"`)) {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
}
