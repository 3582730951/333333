package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestCodexFiveVersionsDriveHeadersAndBodyProfile(t *testing.T) {
	client, _ := fixedSecretClient(t, config.Default())
	const (
		rootSession = "019f1000-0000-7000-8000-000000000001"
		childThread = "019f1000-0000-7000-8000-000000000002"
		turnID      = "019f1000-0000-7000-8000-000000000003"
		downstream  = "019f1000-0000-7000-8000-000000000004"
		parentRaw   = "downstream-parent-turn"
	)
	tests := []struct {
		version      string
		codeMode     bool
		parentTurnID bool
		cacheKey     string
	}{
		{version: "0.145.0", cacheKey: rootSession},
		{version: "0.146.0", codeMode: true, cacheKey: rootSession},
		{version: "0.146.1", codeMode: true, cacheKey: rootSession},
		{version: "0.147.0", codeMode: true, parentTurnID: true, cacheKey: rootSession},
		{version: "0.148.0", codeMode: true, parentTurnID: true, cacheKey: rootSession},
	}
	for _, tc := range tests {
		t.Run(tc.version, func(t *testing.T) {
			turnMetadata, err := json.Marshal(map[string]interface{}{
				"thread_source":        "user",
				"code_mode_tool_names": map[string]string{"shell": "shell"},
				"parent_turn_id":       parentRaw,
			})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]interface{}{
				"model":            "gpt-5.6-sol",
				"instructions":     "keep",
				"input":            "hello",
				"prompt_cache_key": downstream,
				"client_metadata": map[string]interface{}{
					"parent_turn_id":        parentRaw,
					"x-codex-turn-metadata": string(turnMetadata),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			headers := http.Header{}
			headers.Set("version", tc.version)
			headers.Set("User-Agent", "codex_cli_rs/"+tc.version+" (Linux 6.8.0; x86_64) unknown")
			spec := Request{
				DownstreamPath: "/v1/responses",
				Headers:        headers,
				Body:           testBody(raw),
				Account:        storage.Account{ID: "acc-profile-" + tc.version},
				Token:          storage.AccountToken{AccessToken: "oauth-access", RefreshToken: "oauth-refresh"},
				CodexIdentity: &CodexIdentitySnapshot{
					InstallationID:   "installation-fixed",
					SessionID:        rootSession,
					ThreadID:         childThread,
					TurnID:           turnID,
					WindowGeneration: 2,
				},
			}
			metadata := client.newCodexRequestMetadata(spec)
			if metadata.profile.version != tc.version || metadata.profile.fingerprintVersion != tc.version ||
				metadata.profile.requiredBetaFeatures != codexBetaFeaturesHeader {
				t.Fatalf("profile = %+v, want exact %q fingerprint", metadata.profile, tc.version)
			}
			spec.codexMetadata = &metadata
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			body := normalizeCodexPromptCacheKeyForProfileWithFields(raw, fields, metadata)
			body = applyCodexClientMetadataWithFields(body, fields, metadata, false)
			var root map[string]interface{}
			if err := json.Unmarshal(body, &root); err != nil {
				t.Fatal(err)
			}
			if got := root["prompt_cache_key"]; got != tc.cacheKey {
				t.Fatalf("prompt_cache_key = %v, want %q", got, tc.cacheKey)
			}
			clientMetadata := root["client_metadata"].(map[string]interface{})
			gotParent, parentPresent := clientMetadata["parent_turn_id"].(string)
			if parentPresent != tc.parentTurnID {
				t.Fatalf("parent_turn_id present = %v, want %v: %+v", parentPresent, tc.parentTurnID, clientMetadata)
			}
			if parentPresent && (gotParent == parentRaw || !looksLikeCodexGeneratedUUID(gotParent)) {
				t.Fatalf("parent_turn_id was not virtualized: %q", gotParent)
			}
			var nested map[string]interface{}
			if err := json.Unmarshal([]byte(clientMetadata["x-codex-turn-metadata"].(string)), &nested); err != nil {
				t.Fatal(err)
			}
			_, codeModePresent := nested["code_mode_tool_names"]
			if codeModePresent != tc.codeMode {
				t.Fatalf("code_mode_tool_names present = %v, want %v: %+v", codeModePresent, tc.codeMode, nested)
			}
			_, nestedParentPresent := nested["parent_turn_id"]
			if nestedParentPresent != tc.parentTurnID {
				t.Fatalf("nested parent_turn_id present = %v, want %v: %+v", nestedParentPresent, tc.parentTurnID, nested)
			}

			upstreamHeaders := http.Header{}
			if err := client.applyCodexHeaders(upstreamHeaders, spec); err != nil {
				t.Fatal(err)
			}
			if got := getHeaderFold(upstreamHeaders, "version"); got != tc.version {
				t.Fatalf("version = %q, want %q", got, tc.version)
			}
			if ua := upstreamHeaders.Get("User-Agent"); !strings.HasPrefix(ua, "codex_cli_rs/"+tc.version+" ") {
				t.Fatalf("User-Agent = %q, want downstream version", ua)
			}
			if beta := getHeaderFold(upstreamHeaders, "x-codex-beta-features"); beta != metadata.profile.requiredBetaFeatures {
				t.Fatalf("beta features = %q, want profile %q", beta, metadata.profile.requiredBetaFeatures)
			}
			if strings.Contains(upstreamHeaders.Get("x-codex-turn-metadata"), "code_mode_tool_names") {
				t.Fatalf("bounded compatibility header leaked code-mode map: %s", upstreamHeaders.Get("x-codex-turn-metadata"))
			}
		})
	}
}

func TestCodexResolvedVersionSnapshotSurvivesHotConfigUpdate(t *testing.T) {
	cfg := config.Default()
	client, _ := fixedSecretClient(t, cfg)
	headers := http.Header{}
	headers.Set("version", "0.145.0")
	headers.Set("User-Agent", "codex_cli_rs/0.145.0 (Linux 6.8.0; x86_64) unknown")
	spec := Request{
		DownstreamPath: "/v1/responses",
		Headers:        headers,
		Account:        storage.Account{ID: "acc-version-snapshot"},
		Token:          storage.AccountToken{AccessToken: "oauth-access", RefreshToken: "oauth-refresh"},
	}
	spec.codexResolvedClientVersion = client.resolveCodexClientVersion(spec)
	updated := cfg
	updated.CodexCLIVersionOverride = "0.148.0"
	client.UpdateConfig(updated)
	metadata := client.newCodexRequestMetadata(spec)
	upstreamHeaders := http.Header{}
	if err := client.applyCodexHeaders(upstreamHeaders, spec); err != nil {
		t.Fatal(err)
	}
	if metadata.profile.version != "0.145.0" || getHeaderFold(upstreamHeaders, "version") != "0.145.0" {
		t.Fatalf("request mixed versions after hot update: profile=%q headers=%q", metadata.profile.version, getHeaderFold(upstreamHeaders, "version"))
	}
}

func TestCodexCustomCacheKeySurvivesVersionNormalization(t *testing.T) {
	raw := []byte(`{"prompt_cache_key":"operator-cache-key"}`)
	fields := map[string]json.RawMessage{"prompt_cache_key": json.RawMessage(`"operator-cache-key"`)}
	metadata := codexRequestMetadata{
		sessionID: "session-replacement",
		threadID:  "thread-replacement",
		profile:   codexProtocolProfileForVersion("0.148.0"),
	}
	if got := normalizeCodexPromptCacheKeyForProfileWithFields(raw, fields, metadata); string(got) != string(raw) {
		t.Fatalf("custom cache key changed: %s", got)
	}
}

func TestCodexSourcePathUsesFrozenVersionProfile(t *testing.T) {
	client, _ := fixedSecretClient(t, config.Default())
	for _, tc := range []struct {
		version      string
		cacheKey     string
		wantCodeMode bool
		wantParent   bool
	}{
		{version: "0.145.0", cacheKey: "019f2000-0000-7000-8000-000000000001"},
		{version: "0.148.0", cacheKey: "019f2000-0000-7000-8000-000000000001", wantCodeMode: true, wantParent: true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			raw := []byte(`{"model":"gpt-5.6-sol","instructions":"keep","input":"hello","prompt_cache_key":"019f2000-0000-7000-8000-000000000004","client_metadata":{"parent_turn_id":"raw-parent","x-codex-turn-metadata":"{\"code_mode_tool_names\":{\"shell\":\"shell\"},\"parent_turn_id\":\"raw-parent\"}"}}`)
			source := bodysource.Bytes(raw)
			meta, err := bodysource.ScanJSON(context.Background(), source, nil)
			if err != nil {
				t.Fatal(err)
			}
			headers := http.Header{}
			headers.Set("version", tc.version)
			headers.Set("User-Agent", "codex_cli_rs/"+tc.version+" (Linux 6.8.0; x86_64) unknown")
			spec := Request{
				DownstreamPath: "/v1/responses",
				Headers:        headers,
				Body:           source,
				BodyMeta:       &meta,
				Account:        storage.Account{ID: "source-version-" + tc.version},
				Token:          storage.AccountToken{AccessToken: "oauth-access", RefreshToken: "oauth-refresh"},
				CodexIdentity: &CodexIdentitySnapshot{
					InstallationID: "installation-fixed",
					SessionID:      "019f2000-0000-7000-8000-000000000001",
					ThreadID:       "019f2000-0000-7000-8000-000000000002",
					TurnID:         "019f2000-0000-7000-8000-000000000003",
				},
			}
			spec.codexResolvedClientVersion = client.resolveCodexClientVersion(spec)
			normalized, err := normalizeCodexSource(client, &spec, whamBaseURL, false)
			if err != nil || !normalized {
				t.Fatalf("normalize=%v err=%v", normalized, err)
			}
			got, err := bodysource.ReadAll(spec.Body)
			if err != nil {
				t.Fatal(err)
			}
			var root map[string]interface{}
			if err := json.Unmarshal(got, &root); err != nil {
				t.Fatal(err)
			}
			if root["prompt_cache_key"] != tc.cacheKey {
				t.Fatalf("source prompt_cache_key=%v want=%q body=%s", root["prompt_cache_key"], tc.cacheKey, got)
			}
			clientMetadata := root["client_metadata"].(map[string]interface{})
			_, parentPresent := clientMetadata["parent_turn_id"]
			if parentPresent != tc.wantParent {
				t.Fatalf("source parent present=%v want=%v metadata=%+v", parentPresent, tc.wantParent, clientMetadata)
			}
			var turn map[string]interface{}
			if err := json.Unmarshal([]byte(clientMetadata["x-codex-turn-metadata"].(string)), &turn); err != nil {
				t.Fatal(err)
			}
			_, codeModePresent := turn["code_mode_tool_names"]
			if codeModePresent != tc.wantCodeMode {
				t.Fatalf("source code-mode present=%v want=%v metadata=%+v", codeModePresent, tc.wantCodeMode, turn)
			}
		})
	}
}

func TestAnalyzeCodexDownstreamVersionSelectsExactFingerprint(t *testing.T) {
	client, _ := fixedSecretClient(t, config.Default())
	tests := []struct {
		name       string
		path       string
		headers    http.Header
		body       string
		want       string
		wantSource string
	}{
		{
			name: "consensus", path: "/v1/responses?client_version=0.145.0",
			headers: http.Header{"version": {"0.145.0"}, "User-Agent": {"codex_exec/0.145.0 (Linux; x86_64) terminal"}},
			want:    "0.145.0", wantSource: "header+user_agent+query",
		},
		{
			name: "codex header", path: "/v1/responses",
			headers: http.Header{"X-Codex-Client-Version": {"0.146.1"}},
			want:    "0.146.1", wantSource: "codex_header",
		},
		{
			name: "tracked body", path: "/v1/responses", headers: http.Header{},
			body: `{"client_version":"0.147.0","input":"hi"}`,
			want: "0.147.0", wantSource: "body",
		},
		{
			name: "latest", path: "/v1/responses", headers: http.Header{"version": {"v0.148.0"}},
			want: "0.148.0", wantSource: "header",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := Request{DownstreamPath: tc.path, Headers: tc.headers}
			if tc.body != "" {
				spec.Body = bodysource.Bytes([]byte(tc.body))
				meta, err := bodysource.ScanJSON(context.Background(), spec.Body, nil)
				if err != nil {
					t.Fatal(err)
				}
				spec.BodyMeta = &meta
			}
			analysis := analyzeCodexDownstreamVersion(spec)
			if analysis.version != tc.want || analysis.fingerprintVersion != tc.want || analysis.source != tc.wantSource || analysis.conflict || analysis.unsupported {
				t.Fatalf("analysis=%+v want version=%q source=%q", analysis, tc.want, tc.wantSource)
			}
			profile := codexProtocolProfileForVersion(client.resolveCodexClientVersion(spec))
			if profile.version != tc.want || profile.fingerprintVersion != tc.want {
				t.Fatalf("profile=%+v", profile)
			}
		})
	}
}

func TestAnalyzeCodexDownstreamVersionRejectsConflictUnknownAndThirdParty(t *testing.T) {
	cfg := config.Default()
	cfg.CodexCLIVersionOverride = "0.146.1"
	client, _ := fixedSecretClient(t, cfg)
	tests := []struct {
		name            string
		spec            Request
		wantConflict    bool
		wantUnsupported bool
	}{
		{
			name: "header ua conflict",
			spec: Request{Headers: http.Header{
				"version": {"0.148.0"}, "User-Agent": {"codex_cli_rs/0.147.0 (Linux; x86_64) terminal"},
			}},
			wantConflict: true,
		},
		{
			name:         "query conflict",
			spec:         Request{DownstreamPath: "/v1/responses?client_version=0.147.0", Headers: http.Header{"version": {"0.148.0"}}},
			wantConflict: true,
		},
		{
			name: "/future version", spec: Request{DownstreamPath: "/v1/responses?client_version=0.149.0"},
			wantUnsupported: true,
		},
		{
			name: "third party ua", spec: Request{Headers: http.Header{"User-Agent": {"third-party/0.148.0"}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			analysis := analyzeCodexDownstreamVersion(tc.spec)
			if analysis.version != "" || analysis.conflict != tc.wantConflict || analysis.unsupported != tc.wantUnsupported {
				t.Fatalf("analysis=%+v", analysis)
			}
			if got := client.resolveCodexClientVersion(tc.spec); got != "0.146.1" {
				t.Fatalf("fallback version=%q", got)
			}
		})
	}

	forced := Request{
		Headers: http.Header{"version": {"0.145.0"}}, CodexClientVersion: config.DefaultClientVersion,
	}
	if got := client.resolveCodexClientVersion(forced); got != config.DefaultClientVersion {
		t.Fatalf("model/probe override lost to downstream detection: %q", got)
	}
}

func TestStripCodexResponsesClientVersionQueryPreservesOtherQueryBytes(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"/v1/responses?client_version=0.148.0", "/v1/responses"},
		{"/v1/responses?z=2&client_version=0.148.0&a=1", "/v1/responses?z=2&a=1"},
		{"/v1/responses/compact?client%5Fversion=0.147.0&keep=%2F", "/v1/responses/compact?keep=%2F"},
		{"/v1/models?client_version=0.148.0", "/v1/models?client_version=0.148.0"},
	} {
		if got := stripCodexResponsesClientVersionQuery(tc.input); got != tc.want {
			t.Errorf("stripCodexResponsesClientVersionQuery(%q)=%q want=%q", tc.input, got, tc.want)
		}
	}
}

func TestCodexTrackedBodyVersionSelectsAndStripsFingerprintHint(t *testing.T) {
	client, _ := fixedSecretClient(t, config.Default())
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":"keep","input":"hi","client_version":"0.147.0"}`)
	source := bodysource.Bytes(raw)
	meta, err := bodysource.ScanJSON(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(meta.Scalars["client_version"]) != `"0.147.0"` {
		t.Fatalf("client_version scan=%s", meta.Scalars["client_version"])
	}
	spec := Request{
		DownstreamPath: "/v1/responses", Headers: http.Header{}, Body: source, BodyMeta: &meta,
		Account: storage.Account{ID: "body-version"},
		Token:   storage.AccountToken{AccessToken: "oauth-access", RefreshToken: "oauth-refresh"},
	}
	spec.codexResolvedClientVersion = client.resolveCodexClientVersion(spec)
	if spec.codexResolvedClientVersion != "0.147.0" {
		t.Fatalf("resolved version=%q", spec.codexResolvedClientVersion)
	}
	if normalized, err := normalizeCodexSource(client, &spec, whamBaseURL, false); err != nil || !normalized {
		t.Fatalf("normalize=%v err=%v", normalized, err)
	}
	got, err := bodysource.ReadAll(spec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "client_version") {
		t.Fatalf("client-only version hint reached upstream: %s", got)
	}
}
