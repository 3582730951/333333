package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

// TestCodexConfigScript verifies GET /file/{key} returns a bash script that
// configures the codex CLI against THIS pool: a custom model_provider whose
// base_url is the pool origin + /v1, wire_api=responses, and the caller's key as
// the bearer token. The model defaults to the key's force_model and no unrelated
// Codex feature/configuration is added.
func TestCodexConfigScript(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	const plain = "cap_codextest"
	if err := h.store.UpsertAPIKey(ctx, storage.APIKey{
		KeyHash:    hashAPIKey(plain),
		Label:      "codex",
		GroupName:  "cyber",
		ForceModel: "gpt-5.6-sol",
		Enabled:    true,
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(h.pool.URL + "/file/" + plain)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{
		"#!/usr/bin/env bash",
		`model = "$MODEL"`,
		`model_provider = "$PROVIDER_ID"`,
		`wire_api = "responses"`,
		`supports_websockets = true`,
		`API_KEY='` + plain + `'`,
		`MODEL='gpt-5.6-sol'`,
		`name = "OpenAI"`,
		`experimental_bearer_token = "$API_KEY"`,
		"/v1\"", // base_url ends with /v1
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("codex config script missing %q\n---\n%s", want, s)
		}
	}
	for _, forbidden := range []string{
		"model_context_window =",
		"model_auto_compact_token_limit =",
		`http_headers = { "X-Pool-Client-ID"`,
		"pool-client-id",
		"pool-token",
		"rtk init --codex",
		`goals = true`,
		`model_reasoning_effort =`,
		`approval_policy =`,
		`sandbox_mode =`,
		"super-instruct",
		"super_instruct",
		superInstructClientChoiceHeader,
	} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("Codex branch exceeded the requested configuration allowlist; found %q\n---\n%s", forbidden, s)
		}
	}
	// chat wire_api was removed upstream — never emit it.
	if strings.Contains(s, `wire_api = "chat"`) {
		t.Fatalf("script must not use the removed chat wire_api")
	}
}

func TestCodexSetupScriptsPassBashSyntaxForEntitledAndUnentitledKeys(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	for _, codexOnly := range []bool{false, true} {
		for _, entitled := range []bool{false, true} {
			name := "combined"
			if codexOnly {
				name = "codex-only"
			}
			if entitled {
				name += "-entitled"
			} else {
				name += "-unentitled"
			}
			t.Run(name, func(t *testing.T) {
				policy := CodexSuperInstructInstallPolicy{}
				if entitled {
					policy = CodexSuperInstructInstallPolicy{
						GroupName: "syntax-team", Assigned: true, Entitled: true,
						Capabilities: "指令与分组 skills、响应改写、Memory、Monitor",
					}
				}
				script := buildCodexConfigScript(
					"https://pool.example", "cap_syntax", "gpt-5.6-sol", "", "", "",
					CodexSetupScriptOptions{CodexOnly: codexOnly, SuperInstruct: policy},
				)
				cmd := exec.Command("bash", "-n")
				cmd.Stdin = strings.NewReader(script)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("generated script syntax failed: %v\n%s\n--- script ---\n%s", err, output, script)
				}
			})
		}
	}
}

func TestCodexOnlyConfigScriptEndpointContainsNoOtherInstallerBranches(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	const plain = "cap_codex_only"
	if err := h.store.UpsertAPIKey(context.Background(), storage.APIKey{
		KeyHash: hashAPIKey(plain), Label: "codex-only", GroupName: "cyber",
		ForceModel: "gpt-5.6-sol", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(h.pool.URL + "/file/" + plain + "?client=codex")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	script := string(body)
	if got := resp.Header.Get("Content-Disposition"); got != "attachment; filename=setup-pool-codex.sh" {
		t.Fatalf("content disposition=%q", got)
	}
	for _, want := range []string{
		`model = "$MODEL"`,
		`base_url = "$ORIGIN/v1"`,
		`supports_websockets = true`,
		`experimental_bearer_token = "$API_KEY"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("Codex-only script missing %q\n---\n%s", want, script)
		}
	}
	for _, forbidden := range []string{
		"select_client", "configure_claude", "gateway", "claude", "rtk",
		"curl ", "models_cache.json", "pool-token", "pool-client-id",
		"X-Pool-Client-ID", "mcp_servers", "plugins.",
		"super-instruct", "super_instruct", "X-Pool-Super-Instruct", "skills",
		`goals = true`, `model_reasoning_effort =`, `approval_policy =`, `sandbox_mode =`,
	} {
		if strings.Contains(strings.ToLower(script), strings.ToLower(forbidden)) {
			t.Fatalf("Codex-only endpoint contains unrelated branch/config %q\n---\n%s", forbidden, script)
		}
	}
}

func TestCodexConfigScriptCarriesReadOnlyUserGroupSuperInstructEntitlement(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	const (
		plain       = "cap_super_install"
		userGroupID = "ug_super_install"
	)
	if err := h.store.CreateUserGroup(ctx, storage.UserGroup{
		ID:   userGroupID,
		Name: "Codex 团队",
		SuperInstructProfiles: storage.SuperInstructProfiles{
			storage.ModelInstructionFamilyGPT: {
				Enabled:                true,
				SkillIDs:               []string{"anti-debug", "reverse-engineering"},
				ResponseRewriteEnabled: true,
				MemoryEnabled:          true,
				MonitorEnabled:         true,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAPIKey(ctx, storage.APIKey{
		KeyHash: hashAPIKey(plain), Label: "super-install", UserGroupID: userGroupID,
		ForceModel: "gpt-5.6-sol", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(h.pool.URL + "/file/" + plain + "?client=codex")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	script := string(body)
	for _, want := range []string{
		"SUPER_INSTRUCT_GROUP='Codex 团队'",
		"指令与 2 个分组 skills、响应改写、Memory、Monitor",
		"POOL_SUPER_INSTRUCT=enabled|disabled",
		"--super-instruct enabled|disabled",
		"[y/N]",
		`SUPER_INSTRUCT_HEADER='` + superInstructClientChoiceHeader + `'`,
		`http_headers = { "$SUPER_INSTRUCT_HEADER" = "$SUPER_INSTRUCT_CHOICE" }`,
		"不会修改服务器用户分组策略",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("Codex installer missing Super-Instruct contract %q\n---\n%s", want, script)
		}
	}
	for _, forbidden := range []string{"/admin/user-groups", "super_instruct_enabled=true", "PATCH "} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("API-key installer may not mutate user-group policy; found %q\n---\n%s", forbidden, script)
		}
	}
}

func TestCodexOnlySuperInstructChoiceWritesExplicitProviderHeader(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	policy := CodexSuperInstructInstallPolicy{
		GroupName:    "team-super",
		Assigned:     true,
		Entitled:     true,
		Capabilities: "指令与分组允许的全部 skills、响应改写、Memory、Monitor",
	}
	for _, tc := range []struct {
		name   string
		choice string
		args   []string
		want   string
	}{
		{name: "enabled", choice: "enabled", want: `http_headers = { "X-Pool-Super-Instruct" = "enabled" }`},
		{name: "disabled flag", args: []string{"--super-instruct=disabled"}, want: `http_headers = { "X-Pool-Super-Instruct" = "disabled" }`},
		{name: "non-interactive default", want: `http_headers = { "X-Pool-Super-Instruct" = "disabled" }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			options := CodexSetupScriptOptions{CodexOnly: true, SuperInstruct: policy}
			script := buildCodexConfigScript("https://pool.example", "cap_choice", "gpt-5.6-sol", "", "", "", options)
			path := filepath.Join(t.TempDir(), "setup.sh")
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", append([]string{path}, tc.args...)...)
			cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
			if tc.choice != "" {
				cmd.Env = append(cmd.Env, "POOL_SUPER_INSTRUCT="+tc.choice)
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("setup failed: %v\n%s", err, output)
			}
			config, err := os.ReadFile(filepath.Join(home, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(config), tc.want) != 1 {
				t.Fatalf("choice must be persisted exactly once as provider header; want %q\n---\n%s", tc.want, config)
			}
		})
	}
}

func TestCombinedCodexHeaderRequiresEntitlementAndClientChoice(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	entitled := CodexSuperInstructInstallPolicy{
		GroupName: "team-super", Assigned: true, Entitled: true,
		Capabilities: "指令与分组允许的 skills",
	}
	for _, tc := range []struct {
		name   string
		policy CodexSuperInstructInstallPolicy
		choice string
		want   string
	}{
		{name: "unentitled ignores forced environment", choice: "enabled"},
		{name: "entitled disabled", policy: entitled, choice: "disabled", want: `http_headers = { "X-Pool-Super-Instruct" = "disabled" }`},
		{name: "entitled enabled", policy: entitled, choice: "enabled", want: `http_headers = { "X-Pool-Super-Instruct" = "enabled" }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			script := buildCodexConfigScript(
				"https://pool.example", "cap_combined", "gpt-5.6-sol", "", "", "",
				CodexSetupScriptOptions{SuperInstruct: tc.policy},
			)
			path := filepath.Join(t.TempDir(), "setup.sh")
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", path)
			cmd.Env = append(os.Environ(), "CODEX_HOME="+home, "POOL_CLIENT=codex", "POOL_SUPER_INSTRUCT="+tc.choice)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("combined Codex setup failed: %v\n%s", err, output)
			}
			config, err := os.ReadFile(filepath.Join(home, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if strings.Contains(string(config), "http_headers") || strings.Contains(string(config), superInstructClientChoiceHeader) {
					t.Fatalf("unentitled combined installer wrote a client-choice header:\n%s", config)
				}
			} else if strings.Count(string(config), tc.want) != 1 {
				t.Fatalf("combined installer header mismatch; want %q\n---\n%s", tc.want, config)
			}
		})
	}
}

func TestCodexOnlyUnentitledScriptHasNoSuperInstructSurface(t *testing.T) {
	home := t.TempDir()
	options := CodexSetupScriptOptions{
		CodexOnly: true,
		SuperInstruct: CodexSuperInstructInstallPolicy{
			GroupName: "team-standard", Assigned: true,
			Capabilities: "该分组未为 Codex 启用 Super-Instruct",
		},
	}
	script := buildCodexConfigScript("https://pool.example", "cap_no_entitlement", "gpt-5.6-sol", "", "", "", options)
	path := filepath.Join(t.TempDir(), "setup.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", path)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home, "POOL_SUPER_INSTRUCT=enabled")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unentitled installer should silently force the absent header path: err=%v output=%s", err, output)
	}
	for _, leaked := range []string{
		"team-standard", "该分组未为 Codex 启用 Super-Instruct", "Super-Instruct",
		"SUPER_INSTRUCT", "POOL_SUPER_INSTRUCT", superInstructClientChoiceHeader, "skills",
	} {
		if strings.Contains(script, leaked) || strings.Contains(string(output), leaked) {
			t.Fatalf("unentitled installer disclosed %q\n--- script ---\n%s\n--- output ---\n%s", leaked, script, output)
		}
	}
	config, readErr := os.ReadFile(filepath.Join(home, "config.toml"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(config), "http_headers") || strings.Contains(string(config), superInstructClientChoiceHeader) {
		t.Fatalf("unentitled installer must omit the client-choice header entirely:\n%s", config)
	}
}

func TestCodexOnlyConfigMergePreservesClientOwnedSettings(t *testing.T) {
	home := t.TempDir()
	existing := `  model = "client-model"
  model_provider = "client-provider"
model_reasoning_effort = "medium"
plan_mode_reasoning_effort = "high"
approval_policy = "on-request"
sandbox_mode = "workspace-write"
personality = "pragmatic"
service_tier = "priority"
model_instructions_file = "/tmp/client-instructions.md"

[features]
skills = true
web_search = true

[mcp_servers.workspace]
command = "client-mcp"

[plugins.client]
enabled = true

[model_providers.poolserver]
name = "stale pool"
base_url = "https://stale.invalid/v1"
experimental_bearer_token = "stale"

[model_providers.client]
name = "client provider"
base_url = "https://client.invalid/v1"
`
	configPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(configPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	runCodexSetupScript(t, home, buildCodexConfigScript(
		"https://pool.example", "cap_keep", "gpt-5.6-sol", "", "", "",
		CodexSetupScriptOptions{CodexOnly: true},
	))
	gotBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(gotBytes)
	for _, preserved := range []string{
		`model_reasoning_effort = "medium"`,
		`plan_mode_reasoning_effort = "high"`,
		`approval_policy = "on-request"`,
		`sandbox_mode = "workspace-write"`,
		`personality = "pragmatic"`,
		`service_tier = "priority"`,
		`model_instructions_file = "/tmp/client-instructions.md"`,
		"[features]\nskills = true\nweb_search = true",
		"[mcp_servers.workspace]\ncommand = \"client-mcp\"",
		"[plugins.client]\nenabled = true",
		"[model_providers.client]\nname = \"client provider\"",
	} {
		if !strings.Contains(got, preserved) {
			t.Fatalf("client-owned config changed or disappeared: missing %q\n---\n%s", preserved, got)
		}
	}
	for _, want := range []string{
		`model = "gpt-5.6-sol"`,
		`model_provider = "poolserver"`,
		`base_url = "https://pool.example/v1"`,
		`experimental_bearer_token = "cap_keep"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("managed config missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "client-model") || strings.Contains(got, `model_provider = "client-provider"`) ||
		strings.Contains(got, "stale pool") || strings.Count(got, "[model_providers.poolserver]") != 1 {
		t.Fatalf("stale Pool provider table survived merge:\n%s", got)
	}
}

func TestCodexOnlyConfigMergeRemovesOnlyLegacyPoolInstructionsBinding(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	existing := `model = "old"
model_provider = "old"
model_instructions_file = "/root/.codex/gpt-5.6-sol-unrestricted-v42.md" # old Pool binding
personality = "pragmatic"

[features]
skills = true

[model_providers.client]
name = "client provider"
`
	if err := os.WriteFile(configPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	runCodexSetupScript(t, home, buildCodexConfigScript(
		"https://pool.example", "cap_cleanup", "gpt-5.6-sol", "", "", "",
		CodexSetupScriptOptions{CodexOnly: true},
	))
	gotBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(gotBytes)
	if strings.Contains(got, "gpt-5.6-sol-unrestricted-v42.md") || strings.Contains(got, "model_instructions_file") {
		t.Fatalf("legacy Pool instructions binding survived merge:\n%s", got)
	}
	for _, preserved := range []string{
		`personality = "pragmatic"`,
		"[features]\nskills = true",
		"[model_providers.client]\nname = \"client provider\"",
	} {
		if !strings.Contains(got, preserved) {
			t.Fatalf("unrelated client setting disappeared: missing %q\n---\n%s", preserved, got)
		}
	}
	backups, err := filepath.Glob(configPath + ".bak.*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("expected one rollback backup, backups=%v err=%v", backups, err)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil || !strings.Contains(string(backup), "gpt-5.6-sol-unrestricted-v42.md") {
		t.Fatalf("backup did not preserve the pre-merge binding: err=%v backup=%s", err, backup)
	}
}

func TestCodexOnlyConfigMergeRemovesAllHistoricalPoolInstructionBasenames(t *testing.T) {
	for _, basename := range []string{
		"gpt-5.6-sol-unrestricted.md",
		"gpt-5.6-sol-unrestricted-v7.md",
		"gpt-5.6-sol-unrestricted-v999.md",
	} {
		t.Run(basename, func(t *testing.T) {
			home := t.TempDir()
			configPath := filepath.Join(home, "config.toml")
			existing := "model_instructions_file = \"/root/.codex/" + basename + "\" # historical Pool binding\n" +
				"model = \"old\"\nmodel_provider = \"old\"\n"
			if err := os.WriteFile(configPath, []byte(existing), 0o600); err != nil {
				t.Fatal(err)
			}
			runCodexSetupScript(t, home, buildCodexConfigScript(
				"https://pool.example", "cap_historical_cleanup", "gpt-5.6-sol", "", "", "",
				CodexSetupScriptOptions{CodexOnly: true},
			))
			got, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(got), basename) || strings.Contains(string(got), "model_instructions_file") {
				t.Fatalf("historical Pool binding %q survived merge:\n%s", basename, got)
			}
		})
	}
}

func TestCodexOnlyConfigFreshInstallManagesMinimalKeys(t *testing.T) {
	home := t.TempDir()
	runCodexSetupScript(t, home, buildCodexConfigScript(
		"https://pool.example", "cap_fresh", "gpt-5.6-sol", "", "", "",
		CodexSetupScriptOptions{CodexOnly: true},
	))
	gotBytes, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(gotBytes)
	for _, want := range []string{
		`model = "gpt-5.6-sol"`,
		`model_provider = "poolserver"`,
		`[model_providers.poolserver]`,
		`experimental_bearer_token = "cap_fresh"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("fresh config missing %q\n---\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"model_reasoning_effort", "plan_mode_reasoning_effort", "approval_policy",
		"sandbox_mode", "personality", "service_tier", "model_instructions_file",
		"[features]", "[mcp_servers.", "[plugins.", "[skills",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("fresh config unexpectedly manages %q\n---\n%s", forbidden, got)
		}
	}
}

func TestCodexOnlyConfigMergeAppliesExplicitPolicyOverrides(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(configPath, []byte(`  model = "old"
  model_provider = "old"
  model_reasoning_effort = "medium"
plan_mode_reasoning_effort = "high"
  approval_policy = "on-request"
  sandbox_mode = "workspace-write"
personality = "friendly"
service_tier = "priority"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	runCodexSetupScript(t, home, buildCodexConfigScript(
		"https://pool.example", "cap_override", "gpt-5.6-sol", "xhigh", "never", "danger-full-access",
		CodexSetupScriptOptions{CodexOnly: true},
	))
	gotBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(gotBytes)
	for _, want := range []string{
		`model_reasoning_effort = "xhigh"`,
		`approval_policy = "never"`,
		`sandbox_mode = "danger-full-access"`,
		`plan_mode_reasoning_effort = "high"`,
		`personality = "friendly"`,
		`service_tier = "priority"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("explicit override merge missing %q\n---\n%s", want, got)
		}
	}
	for _, old := range []string{
		`model_reasoning_effort = "medium"`,
		`approval_policy = "on-request"`,
		`sandbox_mode = "workspace-write"`,
	} {
		if strings.Contains(got, old) {
			t.Fatalf("old explicitly managed setting survived: %q\n---\n%s", old, got)
		}
	}
}

func runCodexSetupScript(t *testing.T, codexHome, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "setup.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", path)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("setup script failed: %v\n%s", err, output)
	}
}

func TestCodexSetupScriptContextWindowIsExplicitAndNonDestructive(t *testing.T) {
	t.Run("positive override replaces only the managed root key", func(t *testing.T) {
		home := t.TempDir()
		configPath := filepath.Join(home, "config.toml")
		if err := os.WriteFile(configPath, []byte("model_context_window = 128000\npersonality = \"pragmatic\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runCodexSetupScript(t, home, buildCodexConfigScript(
			"https://pool.example", "cap_context", "gpt-5.6-sol", "", "", "",
			CodexSetupScriptOptions{CodexOnly: true, ContextWindow: 262144},
		))
		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		got := string(data)
		if strings.Count(got, "model_context_window = 262144") != 1 || strings.Contains(got, "model_context_window = 128000") {
			t.Fatalf("context window was not replaced exactly once:\n%s", got)
		}
		if !strings.Contains(got, `personality = "pragmatic"`) {
			t.Fatalf("context window update removed client-owned config:\n%s", got)
		}
	})

	t.Run("zero preserves an existing client choice", func(t *testing.T) {
		home := t.TempDir()
		configPath := filepath.Join(home, "config.toml")
		if err := os.WriteFile(configPath, []byte("model_context_window = 128000\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runCodexSetupScript(t, home, buildCodexConfigScript(
			"https://pool.example", "cap_context", "gpt-5.6-sol", "", "", "",
			CodexSetupScriptOptions{CodexOnly: true, ContextWindow: 0},
		))
		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(data), "model_context_window = 128000") != 1 {
			t.Fatalf("zero override changed the client-owned context window:\n%s", data)
		}
	})
}

// TestCodexConfigScriptUnknownKeyRequired confirms that, when downstream keys are
// required, an unknown key is refused with a clear message rather than a script
// that would 401 on first use.
func TestCodexConfigScriptUnknownKeyRequired(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.app.cfg.RequireDownstreamKey = true
	resp, err := http.Get(h.pool.URL + "/file/cap_does_not_exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for unknown key when keys required", resp.StatusCode)
	}
}

// TestDescribeNoAccountGroupMismatch is the core of requirement #4: when the
// routed group holds no usable account but accounts DO exist in another group, the
// 503 must say so (the group-mismatch smoking gun) instead of the opaque
// "no active account available".
func TestDescribeNoAccountGroupMismatch(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	// A GPT/Codex account exists, but in group "other" — not the routed "cyber".
	if err := h.store.UpsertAccount(ctx, storage.Account{ID: "gpt-1", Label: "gpt-1", GroupName: "other", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	got := h.app.describeNoAccount(ctx, "cyber", "", "gpt-5.4", scheduler.ErrNoAccount)
	msg := got.Error()
	for _, want := range []string{"cyber", "other", "no available account"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("diagnostic missing %q: %s", want, msg)
		}
	}
}

// TestDescribeNoAccountPassThrough confirms a non-ErrNoAccount error is returned
// unchanged so HTTP status mapping (e.g. strict-sticky 409) is preserved.
func TestDescribeNoAccountPassThrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	other := errors.New("boom")
	if got := h.app.describeNoAccount(context.Background(), "cyber", "", "", other); got != other {
		t.Fatalf("non-ErrNoAccount error must pass through unchanged, got %v", got)
	}
	if got := h.app.describeNoAccount(context.Background(), "cyber", "", "", scheduler.ErrStrictUnavailable); !errors.Is(got, scheduler.ErrStrictUnavailable) {
		t.Fatalf("strict-unavailable must pass through, got %v", got)
	}
}

func TestDescribeNoAccountReportsAntigravityCapabilityCandidates(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.UpsertAccount(ctx, storage.Account{
		ID: "antigravity-diagnostic", GroupName: "cyber", Provider: "antigravity", Status: "active",
	}, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	err := &scheduler.NoAccountError{
		Group: "cyber", AllowedProviders: []string{"claude", "kiro", "antigravity"}, Model: "gemini-unverified",
		Counters: scheduler.NoAccountCounters{ModelUnsupported: 1},
	}
	message := h.app.describeNoAccount(ctx, "cyber", "claude,kiro,antigravity", "gemini-unverified", err).Error()
	for _, want := range []string{"antigravity=1", "model_unsupported=1", "gemini-unverified"} {
		if !strings.Contains(message, want) {
			t.Fatalf("diagnostic missing %q: %s", want, message)
		}
	}
}
