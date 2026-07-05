package api

import (
	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func upsertTestEgressProfile(t *testing.T, h *testHarness, id string) {
	t.Helper()
	if err := h.store.UpsertEgressProfile(context.Background(), storage.EgressProfile{
		ID:             id,
		Name:           id,
		Type:           "curl_cffi_sidecar",
		Endpoint:       "http://127.0.0.1:8790",
		StreamCapable:  true,
		Health:         "healthy",
		MaxConcurrency: 16,
	}); err != nil {
		t.Fatalf("upsert egress profile %s: %v", id, err)
	}
}

func TestAdminEgressBindingValidatesReferencedProfiles(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	accountID := h.importAccount(t, "egress-validation", "up-egress-validation", "tok-egress-validation")

	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+accountID+"/egress-binding", `{"primary_egress_id":"missing"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("missing primary egress status = %d, want 400: %s", code, raw)
	}

	upsertTestEgressProfile(t, h, "egress_alt")
	code, raw = grpReq(t, h, http.MethodPost, "/admin/accounts/"+accountID+"/egress-binding", `{
		"primary_egress_id":"egress_alt",
		"standby_egress_ids":["missing"]
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("missing standby egress status = %d, want 400: %s", code, raw)
	}
}

func TestAdminEgressBindingNormalizesStandbyAndCookieJar(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	accountID := h.importAccount(t, "egress-normalize", "up-egress-normalize", "tok-egress-normalize")
	upsertTestEgressProfile(t, h, "egress_alt")
	upsertTestEgressProfile(t, h, "egress_backup")

	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+accountID+"/egress-binding", `{
		"primary_egress_id":" egress_alt ",
		"standby_egress_ids":["", "egress_alt", "egress_backup", "egress_backup", "egress_direct"],
		"cookie_jar_key":"stale:key"
	}`)
	if code != http.StatusOK {
		t.Fatalf("save binding = %d: %s", code, raw)
	}
	var binding storage.AccountEgressBinding
	if err := json.Unmarshal(raw, &binding); err != nil {
		t.Fatalf("decode binding: %v (%s)", err, raw)
	}
	if binding.PrimaryEgressID != "egress_alt" {
		t.Fatalf("primary = %q, want egress_alt", binding.PrimaryEgressID)
	}
	if binding.StandbyEgressIDs != "egress_backup,egress_direct" {
		t.Fatalf("standby = %q, want normalized backup,direct", binding.StandbyEgressIDs)
	}
	if binding.CookieJarKey != accountID+":egress_alt" {
		t.Fatalf("cookie_jar_key = %q, want account:primary", binding.CookieJarKey)
	}
}

func TestSaveImportedAccountIgnoresGroupDefaultEgress(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	upsertTestEgressProfile(t, h, "egress_group_default")
	group, err := h.store.GetGroup(context.Background(), config.DefaultGroupName)
	if err != nil {
		t.Fatal(err)
	}
	group.DefaultEgressID = "egress_group_default"
	if err := h.store.UpdateGroup(context.Background(), group); err != nil {
		t.Fatal(err)
	}

	account, err := h.app.saveImportedAccount(context.Background(), authparse.ParsedAuth{
		AccountID:         "acc-imported-direct",
		UpstreamAccountID: "up-imported-direct",
		AccessToken:       "tok-imported-direct",
		RefreshToken:      "refresh-imported-direct",
	}, "imported-direct", "", "", "codex", "")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.PrimaryEgressID != storage.DefaultDirectEgressID {
		t.Fatalf("primary egress = %q, want direct despite group default %q", binding.PrimaryEgressID, group.DefaultEgressID)
	}
}

func TestAccountGroupMovePreservesPrimaryEgress(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	accountID := h.importAccount(t, "move-egress", "up-move-egress", "tok-move-egress")
	upsertTestEgressProfile(t, h, "egress_alt")
	if err := h.store.UpsertEgressBinding(context.Background(), storage.AccountEgressBinding{
		AccountID:       accountID,
		PrimaryEgressID: "egress_alt",
	}); err != nil {
		t.Fatal(err)
	}

	if code, raw := grpReq(t, h, http.MethodPost, "/admin/groups", `{"name":"team-egress","default_egress_id":"egress_direct"}`); code != http.StatusOK {
		t.Fatalf("create group = %d: %s", code, raw)
	}
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+accountID+"/group", `{"group":"team-egress"}`); code != http.StatusOK {
		t.Fatalf("move account = %d: %s", code, raw)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.PrimaryEgressID != "egress_alt" {
		t.Fatalf("primary egress after move = %q, want unchanged egress_alt", binding.PrimaryEgressID)
	}
	if !strings.Contains(binding.CookieJarKey, "egress_alt") {
		t.Fatalf("cookie jar key after move = %q, want egress_alt", binding.CookieJarKey)
	}
}
