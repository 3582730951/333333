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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/cursorproxy"
	"codex-account-pool/internal/storage"
)

func TestCodexQuotaPollTargetsBatchTokenFiltering(t *testing.T) {
	now := int64(1_700_000_000)
	accounts := []storage.Account{
		{ID: "codex-explicit", Provider: "codex", Status: "active"},
		{ID: "legacy-codex", Status: "active"},
		{ID: "legacy-claude", Status: "active"},
		{ID: "kiro-explicit", Provider: "kiro", Status: "active"},
		{ID: "kiro-quarantined", Provider: "kiro", Status: "active", QuarantineUntil: now + 60},
		{ID: "custom", Provider: "deepseek", Status: "active"},
		{ID: "disabled", Provider: "codex", Status: "disabled"},
		{ID: "quarantined", Provider: "codex", Status: "active", QuarantineUntil: now + 60},
		{ID: "missing-token", Provider: "codex", Status: "active"},
	}

	ids := quotaPollCandidateAccountIDs(accounts, now)
	wantIDs := []string{"codex-explicit", "legacy-codex", "legacy-claude", "kiro-explicit", "missing-token"}
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

func TestKiroQuotaPollTargetsAreReachableAndStateFiltered(t *testing.T) {
	now := int64(1_700_000_000)
	accounts := []storage.Account{
		{ID: "kiro-active", Provider: "kiro", Status: "active"},
		{ID: "kiro-case", Provider: "KIRO", Status: "active"},
		{ID: "kiro-disabled", Provider: "kiro", Status: "disabled"},
		{ID: "kiro-quarantined", Provider: "kiro", Status: "active", QuarantineUntil: now + 60},
		{ID: "claude", Provider: "claude", Status: "active"},
	}
	targets := kiroQuotaPollTargets(accounts, map[string]storage.AccountToken{
		"kiro-active":      {AccountID: "kiro-active", AccessToken: "a"},
		"kiro-case":        {AccountID: "kiro-case", AccessToken: "b"},
		"kiro-disabled":    {AccountID: "kiro-disabled", AccessToken: "c"},
		"kiro-quarantined": {AccountID: "kiro-quarantined", AccessToken: "d"},
		"claude":           {AccountID: "claude", AccessToken: "e"},
	}, now)
	got := make([]string, 0, len(targets))
	for _, target := range targets {
		got = append(got, target.Account.ID)
	}
	if want := []string{"kiro-active", "kiro-case"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Kiro quota targets=%v, want %v", got, want)
	}
}

func TestCursorQuotaTargetsBrowserAccountsAndAggregatesLimitingPool(t *testing.T) {
	now := int64(1_700_000_000)
	accounts := []storage.Account{
		{ID: "browser", Provider: "cursor", Status: "active"},
		{ID: "api-key", Provider: "cursor", Status: "active"},
		{ID: "disabled", Provider: "cursor", Status: "disabled"},
	}
	targets := cursorQuotaPollTargets(accounts, map[string]storage.AccountToken{
		"browser":  {AccountID: "browser", CredentialMode: cursorproxy.CredentialBrowser, AccessToken: "bridge"},
		"api-key":  {AccountID: "api-key", AuthMethod: "api_key", OpenAIAPIKey: "key"},
		"disabled": {AccountID: "disabled", CredentialMode: cursorproxy.CredentialBrowser, AccessToken: "bridge"},
	}, now)
	if len(targets) != 1 || targets[0].Account.ID != "browser" {
		t.Fatalf("Cursor quota targets = %+v", targets)
	}
	requestLimit := int64(100)
	tokenLimit := int64(1000)
	snapshots := cursorUsageSnapshots("browser", cursorproxy.Usage{
		StartOfMonth: "2026-08-01T00:00:00Z",
		Models: map[string]cursorproxy.ModelUsage{
			"requests": {NumRequests: 75, MaxRequestUsage: &requestLimit},
			"tokens":   {NumTokens: 900, MaxTokenUsage: &tokenLimit},
		},
	}, now)
	if len(snapshots) != 3 {
		t.Fatalf("Cursor snapshots = %+v", snapshots)
	}
	aggregate := snapshots[0]
	if aggregate.LimiterType != "cursor_monthly" || aggregate.UsedPercent != 90 || aggregate.LimitTokens != 1000 || aggregate.RemainingTokens != 100 || aggregate.ResetAt != 1788220800 {
		t.Fatalf("Cursor aggregate = %+v", aggregate)
	}
	if snapshots[1].LimiterType != "cursor_model_monthly" || snapshots[2].LimiterType != "cursor_model_monthly" {
		t.Fatalf("Cursor model diagnostics = %+v", snapshots[1:])
	}
}

func TestParseKiroUsageLimitsSelectsAgenticAndClampsExhaustion(t *testing.T) {
	limits := map[string]interface{}{
		"nextDateReset": "2026-08-01T00:00:00Z",
		"usageBreakdownList": []interface{}{
			map[string]interface{}{"resourceType": "CODE_COMPLETION", "usageLimit": float64(999), "currentUsage": float64(1)},
			map[string]interface{}{
				"resourceType":              "AGENTIC_REQUEST",
				"usageLimitWithPrecision":   float64(10),
				"currentUsageWithPrecision": float64(14),
				"freeTrialInfo": map[string]interface{}{
					"freeTrialStatus": "INACTIVE", "usageLimit": float64(100), "currentUsage": float64(0),
				},
				"bonuses": []interface{}{
					map[string]interface{}{"status": "ACTIVE", "usageLimitWithPrecision": "2.5", "currentUsageWithPrecision": "0.5"},
					map[string]interface{}{"status": "EXPIRED", "usageLimit": float64(500), "currentUsage": float64(0)},
				},
			},
		},
	}
	got, err := parseKiroUsageLimits(limits)
	if err != nil {
		t.Fatal(err)
	}
	if got.Limit != 12.5 || got.Current != 14.5 || got.Remaining != 0 ||
		got.UsedPercent != 100 || got.Status != "exhausted" ||
		got.ResetAt != 1785542400 {
		t.Fatalf("parsed Kiro usage=%+v", got)
	}
}

func TestParseKiroUsageLimitsNormalizesMillisecondReset(t *testing.T) {
	got, err := parseKiroUsageLimits(map[string]interface{}{
		"usageBreakdownList": []interface{}{
			map[string]interface{}{
				"usageLimit": float64(100), "currentUsage": float64(25),
				"nextDateReset": float64(1_785_542_400_000),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Remaining != 75 || got.UsedPercent != 25 || got.Status != "allowed" || got.ResetAt != 1785542400 {
		t.Fatalf("parsed legacy Kiro usage=%+v", got)
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

func TestManualQuotaRefreshCoalescesConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		startedOnce.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"limit_reached":false,"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_after_seconds":60,"status":"allowed"}}}`))
	}))
	defer upstream.Close()
	oldURL := whamUsageURL
	whamUsageURL = upstream.URL
	t.Cleanup(func() { whamUsageURL = oldURL })

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := h.store.UpsertAccount(t.Context(), storage.Account{ID: "quota-flight", Provider: "codex", Status: "active"}, storage.AccountToken{
		AuthMethod: "oauth", AccessToken: "access-token",
	}); err != nil {
		t.Fatal(err)
	}
	request := func(result chan<- error) {
		response, err := http.Post(h.pool.URL+"/admin/quota/refresh", "application/json", strings.NewReader(`{}`))
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				err = errors.New(response.Status)
			}
		}
		result <- err
	}
	results := make(chan error, 2)
	go request(results)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first quota refresh did not reach upstream")
	}
	go request(results)
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	response, err := http.Post(h.pool.URL+"/admin/quota/refresh", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var reused quotaRefreshResult
	if decodeErr := json.NewDecoder(response.Body).Decode(&reused); decodeErr != nil {
		_ = response.Body.Close()
		t.Fatal(decodeErr)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !reused.Reused {
		t.Fatalf("sequential quota refresh status=%d result=%+v", response.StatusCode, reused)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream quota calls = %d, want one coalesced and recently reused request", got)
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
