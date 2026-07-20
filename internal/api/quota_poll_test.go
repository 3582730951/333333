package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestCodexQuotaPollTargetsBatchTokenFiltering(t *testing.T) {
	now := int64(1_700_000_000)
	accounts := []storage.Account{
		{ID: "codex-explicit", Provider: "codex", Status: "active"},
		{ID: "legacy-codex", Status: "active"},
		{ID: "legacy-claude", Status: "active"},
		{ID: "custom", Provider: "deepseek", Status: "active"},
		{ID: "disabled", Provider: "codex", Status: "disabled"},
		{ID: "quarantined", Provider: "codex", Status: "active", QuarantineUntil: now + 60},
		{ID: "missing-token", Provider: "codex", Status: "active"},
	}

	ids := quotaPollCandidateAccountIDs(accounts, now)
	wantIDs := []string{"codex-explicit", "legacy-codex", "legacy-claude", "missing-token"}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("candidate ids = %#v, want %#v", ids, wantIDs)
	}

	targets, missing := codexQuotaPollTargets(accounts, map[string]storage.AccountToken{
		"codex-explicit": {AccountID: "codex-explicit", AccessToken: "codex-at"},
		"legacy-codex":   {AccountID: "legacy-codex", AccessToken: "codex-legacy-at"},
		"legacy-claude":  {AccountID: "legacy-claude", AccessToken: "sk-ant-oat-test"},
	}, now)
	if missing != 1 {
		t.Fatalf("missing token count = %d, want 1", missing)
	}
	gotIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		gotIDs = append(gotIDs, target.Account.ID)
	}
	wantTargets := []string{"codex-explicit", "legacy-codex"}
	if !reflect.DeepEqual(gotIDs, wantTargets) {
		t.Fatalf("target ids = %#v, want %#v", gotIDs, wantTargets)
	}
}

func TestQuotaPollEgressForAccount(t *testing.T) {
	bindings := map[string]storage.AccountEgressBinding{
		"proxied":         {AccountID: "proxied", PrimaryEgressID: "egress-proxy"},
		"sidecar":         {AccountID: "sidecar", PrimaryEgressID: "egress-proxy", SidecarEgressID: "sidecar-transport"},
		"missing":         {AccountID: "missing", PrimaryEgressID: "egress-missing"},
		"missing-sidecar": {AccountID: "missing-sidecar", PrimaryEgressID: "egress-proxy", SidecarEgressID: "deleted-sidecar"},
	}
	profiles := quotaPollEgressProfilesByID([]storage.EgressProfile{
		{ID: storage.DefaultDirectEgressID, Type: "direct"},
		{ID: "egress-proxy", Type: "http_proxy", Endpoint: "http://127.0.0.1:8080"},
		{ID: "sidecar-transport", Type: storage.CurlCFFISidecarEgressType, Endpoint: "http://127.0.0.1:8790"},
	})

	if got := quotaPollEgressForAccount("proxied", bindings, profiles); got.ID != "egress-proxy" || got.Type != "http_proxy" {
		t.Fatalf("proxied egress = %#v, want egress-proxy http_proxy", got)
	}
	if got := quotaPollEgressForAccount("direct", bindings, profiles); got.ID != storage.DefaultDirectEgressID || got.Type != "direct" {
		t.Fatalf("direct egress = %#v, want default direct", got)
	}
	if got := quotaPollEgressForAccount("missing", bindings, profiles); got.Type != "direct" {
		t.Fatalf("missing profile fallback = %#v, want direct", got)
	}
	if got := quotaPollEgressForAccount("sidecar", bindings, profiles); got.ID != "egress-proxy" || got.Type != storage.CurlCFFISidecarEgressType || got.ChainProxy != "http://127.0.0.1:8080" || got.Endpoint != "http://127.0.0.1:8790" {
		t.Fatalf("sidecar-wrapped quota egress = %#v", got)
	}
	if got := quotaPollEgressForAccount("missing-sidecar", bindings, profiles); got.Type != "invalid_sidecar_binding" {
		t.Fatalf("missing sidecar leaked to base proxy: %#v", got)
	}
}

func TestCodexQuotaPollUsesBoundSidecarProtocol(t *testing.T) {
	type sidecarCapture struct{ target, proxy string }
	captures := make(chan sidecarCapture, 1)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metaRaw, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-Sidecar-Meta"))
		var meta struct {
			URL   string `json:"url"`
			Proxy string `json:"proxy"`
		}
		_ = json.Unmarshal(metaRaw, &meta)
		captures <- sidecarCapture{target: meta.URL, proxy: meta.Proxy}
		_, _ = io.Copy(io.Discard, r.Body)
		responseHeaders, _ := json.Marshal(http.Header{"Content-Type": []string{"application/json"}})
		w.Header().Set("x-sidecar-upstream-status", "200")
		w.Header().Set("x-sidecar-upstream-headers-b64", base64.StdEncoding.EncodeToString(responseHeaders))
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"limit_reached":false,"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_after_seconds":60,"status":"allowed"}}}`))
	}))
	defer sidecar.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	account := storage.Account{ID: "codex-sidecar-quota", Provider: "codex", GroupName: "cyber", Status: "active"}
	token := storage.AccountToken{AccountID: account.ID, AccessToken: "access-token"}
	if err := h.store.UpsertAccount(ctx, account, token); err != nil {
		t.Fatal(err)
	}
	wrapped, err := storage.WrapEgressWithSidecar(
		storage.EgressProfile{ID: "proxy-exit", Type: "http_proxy", Endpoint: "http://proxy.example:8080"},
		storage.EgressProfile{ID: "sidecar", Type: storage.CurlCFFISidecarEgressType, Endpoint: sidecar.URL},
	)
	if err != nil {
		t.Fatal(err)
	}
	oldURL := whamUsageURL
	whamUsageURL = "https://chatgpt.com/backend-api/wham/usage"
	t.Cleanup(func() { whamUsageURL = oldURL })
	if err := h.app.pollOneCodexQuota(ctx, account, token, wrapped); err != nil {
		t.Fatal(err)
	}
	got := <-captures
	if got.target != whamUsageURL || got.proxy != "http://proxy.example:8080" {
		t.Fatalf("quota sidecar target=%q proxy=%q", got.target, got.proxy)
	}
}

func TestQuotaPollerUsesBatchTokenLookup(t *testing.T) {
	source := readAPISource(t, "quota_poll.go")
	pollAll := functionBody(t, source, "pollAllCodexQuotas")
	if !strings.Contains(pollAll, ".ListTokensByAccountIDs(") {
		t.Fatal("pollAllCodexQuotas must batch-load account tokens")
	}
	if strings.Contains(pollAll, ".GetToken(") {
		t.Fatal("pollAllCodexQuotas must not use per-account GetToken")
	}
	if !strings.Contains(pollAll, ".attachQuotaPollEgresses(") {
		t.Fatal("pollAllCodexQuotas must preload egress data before launching workers")
	}
	attach := functionBody(t, source, "attachQuotaPollEgresses")
	if !strings.Contains(attach, ".ListEgressBindingsByAccountIDs(") || !strings.Contains(attach, ".ListEgressProfiles(") {
		t.Fatal("attachQuotaPollEgresses must batch-load bindings and profiles")
	}
	pollOne := functionBody(t, source, "pollOneCodexQuota")
	if strings.Contains(pollOne, ".GetToken(") {
		t.Fatal("pollOneCodexQuota must receive a preloaded token instead of querying per account")
	}
	for _, forbidden := range []string{".GetEgressBinding(", ".GetEgressProfile("} {
		if strings.Contains(pollOne, forbidden) {
			t.Fatalf("pollOneCodexQuota must receive preloaded egress data instead of calling %s", forbidden)
		}
	}
}

func TestClaudeOAuthUsageLimiterIgnoresConcreteModelNamesForQuotaSelection(t *testing.T) {
	tests := []struct {
		name          string
		windowSeconds int64
		wantLimiter   string
		wantModel     string
	}{
		{name: "claude-opus-4-8", windowSeconds: 5 * 3600, wantLimiter: ""},
		{name: "claude-sonnet-4-6", windowSeconds: 7 * 24 * 3600, wantLimiter: ""},
		{name: "claude-haiku-future-2027", windowSeconds: 0, wantLimiter: "", wantModel: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLimiter, gotModel := normalizeClaudeUsageLimiter(tt.name, tt.windowSeconds)
			if gotLimiter != tt.wantLimiter || gotModel != tt.wantModel {
				t.Fatalf("normalizeClaudeUsageLimiter(%q,%d) = (%q,%q), want (%q,%q)", tt.name, tt.windowSeconds, gotLimiter, gotModel, tt.wantLimiter, tt.wantModel)
			}
		})
	}
}

func TestRecordQuotaPollErrorWritesMarkerForQuotaSummary(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	acc := storage.Account{ID: "quota-error", Provider: "codex", Status: "active", GroupName: "cyber"}
	if err := h.store.UpsertAccount(ctx, acc, storage.AccountToken{AccountID: acc.ID, AccessToken: "access"}); err != nil {
		t.Fatal(err)
	}
	err := quotaPollError{reason: "http_error", statusCode: 429, body: "quota exhausted", err: errors.New("http 429")}
	h.app.recordQuotaPollError(ctx, acc, "codex", err)
	marker, ok, getErr := h.store.GetAccountRateLimitFor(ctx, acc.ID, "codex", "", "quota_poll_error")
	if getErr != nil || !ok {
		t.Fatalf("quota_poll_error marker missing ok=%v err=%v", ok, getErr)
	}
	if marker.Status != "error/http_error" || marker.Source != "quota_poll_error" {
		t.Fatalf("marker status/source = %#v", marker)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(marker.Raw), &raw); err != nil {
		t.Fatalf("marker raw is not JSON: %v (%s)", err, marker.Raw)
	}
	if raw["sync_reason"] != "error/http_error" || raw["http_status"].(float64) != 429 || !strings.Contains(raw["body_snippet"].(string), "quota exhausted") {
		t.Fatalf("marker raw = %#v", raw)
	}
	summary := BuildQuotaSummary(acc, &storage.AccountToken{AccountID: acc.ID, AccessToken: "access"}, []storage.AccountRateLimit{marker}, storage.Now())
	if summary.SyncReason != "error/http_error" {
		t.Fatalf("summary sync_reason = %q, want marker reason", summary.SyncReason)
	}
}
