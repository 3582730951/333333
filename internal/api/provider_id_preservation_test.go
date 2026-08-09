package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

// A restored backup keeps whatever provider id it carried: normalizeAccountBackupCustomProvider
// only trims, while the admin create path slugifies. Editing such a provider must reach the row
// every model_provider target already names, not insert a second one under the slug.
func TestEditingProviderWithNonSlugIDUpdatesThatRow(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})

	const restoredID = "AiBa Relay"
	seeded := storage.CustomProvider{
		ID:               restoredID,
		Name:             "aiba",
		BaseURL:          "https://aiba.example/v1",
		UpstreamProtocol: storage.CustomProviderProtocolResponses,
		TransportProfile: storage.CustomProviderTransportGeneric,
		Enabled:          true,
		Models:           []string{"gpt-5.6-sol"},
		CreatedAt:        storage.Now(),
		UpdatedAt:        storage.Now(),
	}
	if err := h.store.UpsertCustomProvider(t.Context(), seeded); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodPost, "/admin/providers", `{
		"id":"AiBa Relay",
		"name":"aiba",
		"base_url":"https://aiba.example/v2",
		"enabled":false
	}`)
	if code != http.StatusOK {
		t.Fatalf("POST /admin/providers = %d: %s", code, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode provider: %v (%s)", err, raw)
	}
	if got["id"] != restoredID {
		t.Fatalf("edit retargeted the provider: id=%v, want %q", got["id"], restoredID)
	}

	edited, ok, err := h.store.GetCustomProvider(t.Context(), restoredID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("provider %q disappeared after an edit", restoredID)
	}
	if edited.BaseURL != "https://aiba.example/v2" {
		t.Fatalf("edit did not land on the existing row: base_url=%q", edited.BaseURL)
	}
	if edited.Enabled {
		t.Fatal("edit did not disable the existing row")
	}

	// The slug must not exist as a second provider: routing targets name the restored id, so a
	// shadow row under "aiba-relay" would leave live traffic on the pre-edit configuration.
	if shadow, ok, err := h.store.GetCustomProvider(t.Context(), slugify(restoredID)); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("edit inserted a shadow provider %q (base_url=%q)", shadow.ID, shadow.BaseURL)
	}
}

// Creating a genuinely new provider still normalizes its id, so the preservation above cannot be
// used to introduce fresh non-slug ids through the admin surface.
func TestCreatingProviderStillSlugifiesItsID(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})

	code, raw := grpReq(t, h, http.MethodPost, "/admin/providers", `{
		"id":"Brand New Relay",
		"name":"Brand New",
		"base_url":"https://new.example/v1"
	}`)
	if code != http.StatusOK {
		t.Fatalf("POST /admin/providers = %d: %s", code, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode provider: %v (%s)", err, raw)
	}
	if got["id"] != "brand-new-relay" {
		t.Fatalf("new provider id not slugified: %v", got["id"])
	}
	if _, ok, err := h.store.GetCustomProvider(t.Context(), "Brand New Relay"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("new provider was persisted under its raw id")
	}
}

// A reserved id must stay rejected however it is cased, otherwise "Codex" would slugify into the
// built-in namespace and shadow the real codex scheduler path.
func TestReservedProviderIDsAreRejectedCaseInsensitively(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	for _, id := range []string{"Codex", "CLAUDE", "Kiro", "AntiGravity"} {
		code, raw := grpReq(t, h, http.MethodPost, "/admin/providers",
			`{"id":"`+id+`","base_url":"https://relay.example/v1"}`)
		if code != http.StatusBadRequest {
			t.Fatalf("reserved provider id %q status=%d body=%s", id, code, raw)
		}
	}

	// Verbatim-id preservation must not become a way past the reserved check: a restore can
	// seed "Codex", and editing it would otherwise upsert a custom provider into the built-in
	// namespace, where it shadows the real codex scheduler path.
	if err := h.store.UpsertCustomProvider(t.Context(), storage.CustomProvider{
		ID:               "Codex",
		Name:             "Codex",
		BaseURL:          "https://relay.example/v1",
		UpstreamProtocol: storage.CustomProviderProtocolResponses,
		TransportProfile: storage.CustomProviderTransportGeneric,
		CreatedAt:        storage.Now(),
		UpdatedAt:        storage.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	code, raw := grpReq(t, h, http.MethodPost, "/admin/providers",
		`{"id":"Codex","base_url":"https://relay.example/v2"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("editing a seeded reserved-id provider status=%d body=%s", code, raw)
	}
	if got, ok, err := h.store.GetCustomProvider(t.Context(), "Codex"); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("seeded provider disappeared")
	} else if got.BaseURL != "https://relay.example/v1" {
		t.Fatalf("rejected edit still landed: base_url=%q", got.BaseURL)
	}
}
