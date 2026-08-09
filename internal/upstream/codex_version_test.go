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
		{version: "0.144.6", cacheKey: childThread},
		{version: "0.145.0", cacheKey: rootSession},
		{version: "0.146.0", codeMode: true, cacheKey: rootSession},
		{version: "0.146.1", codeMode: true, cacheKey: rootSession},
		{version: "0.147.0", codeMode: true, parentTurnID: true, cacheKey: rootSession},
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
			if metadata.profile.version != tc.version {
				t.Fatalf("profile version = %q, want %q", metadata.profile.version, tc.version)
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
	updated.CodexCLIVersionOverride = "0.147.0"
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
		profile:   codexProtocolProfileForVersion("0.147.0"),
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
		{version: "0.144.6", cacheKey: "019f2000-0000-7000-8000-000000000002"},
		{version: "0.147.0", cacheKey: "019f2000-0000-7000-8000-000000000001", wantCodeMode: true, wantParent: true},
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
