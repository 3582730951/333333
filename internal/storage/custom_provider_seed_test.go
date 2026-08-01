package storage

import (
	"context"
	"testing"
)

func insertLegacyProviderSeed(t *testing.T, store *Store, id, name, baseURL, models string, now int64) {
	t.Helper()
	if _, err := store.DB().ExecContext(context.Background(), `
INSERT INTO custom_providers(
  id,name,base_url,upstream_protocol,transport_profile,egress_ids,
  enabled,auto_discover_models,models_json,created_at,updated_at
) VALUES(?,?,?,'chat_completions','generic','[]',1,1,?,?,?)`,
		id, name, baseURL, models, now, now); err != nil {
		t.Fatalf("insert legacy provider: %v", err)
	}
}

func TestFreshStoreDoesNotSeedExampleCustomProviders(t *testing.T) {
	store := newTestStore(t)
	providers, err := store.ListCustomProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 {
		t.Fatalf("fresh custom providers = %#v, want none", providers)
	}
}

func TestLegacyProviderSeedCleanupIsConservative(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := Now()
	insertLegacyProviderSeed(t, store, "deepseek", "DeepSeek", "https://api.deepseek.com/v1", `["deepseek-chat","deepseek-reasoner"]`, now)
	insertLegacyProviderSeed(t, store, "siliconflow", "SiliconFlow 硅基流动", "https://api.siliconflow.cn/v1", `[]`, now)

	// A user edit makes the former example an operator-owned definition.
	if _, err := store.DB().ExecContext(ctx, `UPDATE custom_providers SET name='My DeepSeek',updated_at=? WHERE id='deepseek'`, now+1); err != nil {
		t.Fatal(err)
	}
	// A referenced exact seed is active configuration and must also survive.
	if err := store.UpsertAccount(ctx, Account{
		ID: "uses-siliconflow", GroupName: "cyber", Provider: "siliconflow", Status: "active",
	}, AccountToken{OpenAIAPIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	if err := removeUnusedLegacySeedProviders(ctx, store.DB()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"deepseek", "siliconflow"} {
		if _, found, err := store.GetCustomProvider(ctx, id); err != nil || !found {
			t.Fatalf("provider %s should be preserved: found=%v err=%v", id, found, err)
		}
	}

	if _, err := store.DB().ExecContext(ctx, `DELETE FROM accounts WHERE id='uses-siliconflow'`); err != nil {
		t.Fatal(err)
	}
	if err := removeUnusedLegacySeedProviders(ctx, store.DB()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetCustomProvider(ctx, "siliconflow"); err != nil || found {
		t.Fatalf("untouched unreferenced seed should be removed: found=%v err=%v", found, err)
	}
	if _, found, err := store.GetCustomProvider(ctx, "deepseek"); err != nil || !found {
		t.Fatalf("edited provider should remain: found=%v err=%v", found, err)
	}
}
