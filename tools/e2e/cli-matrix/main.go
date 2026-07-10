// Command cli-matrix runs disposable official Codex clients through a live local
// pool_server. It creates short-lived downstream keys, never prints their secrets,
// and deletes them before exit.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type options struct {
	baseURL     string
	adminToken  string
	image       string
	configPath  string
	timeout     time.Duration
	cacheSettle time.Duration
}

type disposableKey struct {
	Label  string
	Secret string `json:"-"`
	Hash   string
}

type cliCase struct {
	Name          string
	Key           disposableKey
	Model         string
	Effort        string
	ExpectedModel string
}

type cliUsage struct {
	Input     int64 `json:"input_tokens"`
	Cached    int64 `json:"cached_input_tokens"`
	Output    int64 `json:"output_tokens"`
	Reasoning int64 `json:"reasoning_output_tokens"`
}

type cliResult struct {
	Scenario           string   `json:"scenario"`
	Phase              string   `json:"phase"`
	Name               string   `json:"name"`
	KeyLabel           string   `json:"key_label"`
	Requested          string   `json:"requested_model"`
	ExpectedModel      string   `json:"expected_upstream_model"`
	Effort             string   `json:"requested_effort"`
	Final              string   `json:"final"`
	Expected           string   `json:"expected"`
	Exact              bool     `json:"exact"`
	Usage              cliUsage `json:"usage"`
	CacheRate          float64  `json:"cache_rate"`
	DurationMS         int64    `json:"duration_ms"`
	Error              string   `json:"error,omitempty"`
	InferenceCompleted bool     `json:"-"`
}

// generatedInferenceStats is derived exclusively from official Codex
// turn.completed events. Unlike the admin by_api_key rollup it never counts the
// Responses WebSocket generate=false prewarm request as an inference request.
type generatedInferenceStats struct {
	Requests     int64   `json:"requests"`
	HitRequests  int64   `json:"hit_requests"`
	PromptTokens int64   `json:"prompt_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	TokenHitRate float64 `json:"token_hit_rate"`
}

type cacheRow struct {
	APIKeyHashPrefix string  `json:"api_key_hash_prefix"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	TokenHitRate     float64 `json:"token_hit_rate"`
}

type modelRow struct {
	Model        string `json:"model"`
	Requests     int64  `json:"requests"`
	PromptTokens int64  `json:"prompt_tokens"`
	CachedTokens int64  `json:"cached_tokens"`
}

type report struct {
	StartedAt                       int64                              `json:"started_at"`
	FinishedAt                      int64                              `json:"finished_at"`
	Results                         []cliResult                        `json:"results"`
	ScenarioCache                   map[string]float64                 `json:"scenario_cache_rate"`
	ScenarioGeneratedInference      map[string]generatedInferenceStats `json:"scenario_generated_inference"`
	ScenarioGeneratedInferenceScope string                             `json:"scenario_generated_inference_scope"`
	CacheBaselineScenario           string                             `json:"cache_baseline_scenario"`
	CacheRegressionThreshold        float64                            `json:"cache_regression_threshold"`
	CacheSettleMS                   int64                              `json:"cache_settle_ms"`
	APIKeyCache                     map[string]cacheRow                `json:"api_key_cache"`
	APIKeyCacheScope                string                             `json:"api_key_cache_scope"`
	APIKeyCacheIncludesWSPrewarm    bool                               `json:"api_key_cache_includes_ws_generate_false_prewarm"`
	Models                          []modelRow                         `json:"models"`
	AllExact                        bool                               `json:"all_exact"`
	CacheRegression                 bool                               `json:"cache_regression"`
	Notes                           []string                           `json:"notes"`
}

const (
	steadyCacheBaseline     = "sequential-steady"
	cacheRegressionLimit    = 0.10
	coldWarmupScenario      = "sequential-cold"
	generatedInferenceScope = "official Codex turn.completed generation requests only; excludes WebSocket generate=false prewarm"
	adminAPIKeyCacheScope   = "admin /usage/cache by_api_key rollup; includes WebSocket generate=false prewarm"
)

func main() {
	var opt options
	flag.StringVar(&opt.baseURL, "base-url", "http://127.0.0.1:18788", "local pool_server base URL")
	flag.StringVar(&opt.adminToken, "admin-token", os.Getenv("POOL_E2E_ADMIN_TOKEN"), "isolated test server admin token")
	flag.StringVar(&opt.image, "image", "pool-fingerprint-capture:20260710", "Docker image containing Codex")
	flag.StringVar(&opt.configPath, "config", "tools/e2e/codex_pool_config.toml", "non-secret Codex config")
	flag.DurationVar(&opt.timeout, "timeout", 120*time.Second, "timeout per CLI")
	flag.DurationVar(&opt.cacheSettle, "cache-settle", 30*time.Second, "wait after the cold write before measuring steady cache reuse")
	flag.Parse()
	if opt.adminToken == "" {
		fatal(errors.New("-admin-token or POOL_E2E_ADMIN_TOKEN is required"))
	}
	absConfig, err := filepath.Abs(opt.configPath)
	if err != nil {
		fatal(err)
	}
	opt.configPath = absConfig

	ctx := context.Background()
	started := time.Now().Unix()
	keys := make([]disposableKey, 0, 3)
	cleaned := false
	cleanup := func() {
		if cleaned {
			return
		}
		cleaned = true
		for _, key := range keys {
			_ = deleteKey(ctx, opt, key.Hash)
		}
	}
	defer cleanup()
	for _, spec := range []struct {
		label, model, effort string
	}{
		{label: "matrix-unforced-1"},
		{label: "matrix-unforced-2"},
		{label: "matrix-forced-terra-high", model: "gpt-5.6-terra", effort: "high"},
	} {
		key, err := createKey(ctx, opt, spec.label, spec.model, spec.effort)
		if err != nil {
			cleanup()
			fatal(err)
		}
		keys = append(keys, key)
	}

	var results []cliResult
	// The first request is deliberately reported as cold: it establishes the cache
	// entry but is not a meaningful baseline for the already-warm parallel phases.
	// Replay the exact same case synchronously before starting any parallel clients,
	// then compare parallel cache rates only with that steady replay.
	baseline := []cliCase{{Name: "WARM", Key: keys[0], Model: "gpt-5.6-sol", Effort: "low", ExpectedModel: "gpt-5.6-sol"}}
	results = append(results, runScenario(ctx, opt, coldWarmupScenario, baseline)...)
	if opt.cacheSettle > 0 {
		timer := time.NewTimer(opt.cacheSettle)
		select {
		case <-ctx.Done():
			timer.Stop()
			cleanup()
			fatal(ctx.Err())
		case <-timer.C:
		}
	}
	results = append(results, runScenario(ctx, opt, steadyCacheBaseline, baseline)...)
	results = append(results, runScenario(ctx, opt, "same-key-parallel", []cliCase{
		{Name: "SAME_A", Key: keys[0], Model: "gpt-5.6-sol", Effort: "low", ExpectedModel: "gpt-5.6-sol"},
		{Name: "SAME_B", Key: keys[0], Model: "gpt-5.6-sol", Effort: "low", ExpectedModel: "gpt-5.6-sol"},
		{Name: "SAME_C", Key: keys[0], Model: "gpt-5.6-sol", Effort: "low", ExpectedModel: "gpt-5.6-sol"},
	})...)
	results = append(results, runScenario(ctx, opt, "different-key-parallel", []cliCase{
		{Name: "DIFF_A", Key: keys[0], Model: "gpt-5.6-sol", Effort: "low", ExpectedModel: "gpt-5.6-sol"},
		{Name: "DIFF_B", Key: keys[1], Model: "gpt-5.6-sol", Effort: "low", ExpectedModel: "gpt-5.6-sol"},
		{Name: "DIFF_C", Key: keys[1], Model: "gpt-5.6-sol", Effort: "low", ExpectedModel: "gpt-5.6-sol"},
	})...)
	results = append(results, runScenario(ctx, opt, "override-parallel", []cliCase{
		{Name: "OVERRIDE_SOL", Key: keys[0], Model: "gpt-5.6-sol", Effort: "low", ExpectedModel: "gpt-5.6-sol"},
		{Name: "OVERRIDE_LUNA", Key: keys[1], Model: "gpt-5.6-luna", Effort: "xhigh", ExpectedModel: "gpt-5.6-luna"},
		{Name: "OVERRIDE_TERRA", Key: keys[2], Model: "gpt-5.6-luna", Effort: "low", ExpectedModel: "gpt-5.6-terra"},
	})...)

	finished := time.Now().Unix()
	out := report{
		StartedAt:                       started,
		FinishedAt:                      finished,
		Results:                         results,
		ScenarioCache:                   scenarioCacheRates(results),
		ScenarioGeneratedInference:      scenarioGeneratedInferenceStats(results),
		ScenarioGeneratedInferenceScope: generatedInferenceScope,
		CacheBaselineScenario:           steadyCacheBaseline,
		CacheRegressionThreshold:        cacheRegressionLimit,
		CacheSettleMS:                   opt.cacheSettle.Milliseconds(),
		APIKeyCache:                     map[string]cacheRow{},
		APIKeyCacheScope:                adminAPIKeyCacheScope,
		APIKeyCacheIncludesWSPrewarm:    true,
		AllExact:                        true,
		Notes: []string{
			"parallel cache regression is compared with the warm sequential replay, never the cold warm-up",
			"steady replay waits for upstream cache propagation; the delay consumes no model tokens",
			"admin by_api_key includes WebSocket generate=false prewarm; use scenario_generated_inference, not api_key_cache, for generation-turn hit rates",
		},
	}
	for _, result := range results {
		if !result.Exact || result.Error != "" {
			out.AllExact = false
		}
	}
	if rows, err := fetchCacheRows(ctx, opt, started-2, finished+2); err != nil {
		out.Notes = append(out.Notes, "cache diagnostics unavailable: "+err.Error())
	} else {
		for _, key := range keys {
			prefix := key.Hash
			if len(prefix) > 12 {
				prefix = prefix[:12]
			}
			for _, row := range rows {
				if row.APIKeyHashPrefix == prefix {
					out.APIKeyCache[key.Label] = row
				}
			}
		}
	}
	if models, err := fetchModels(ctx, opt, started-2, finished+2); err != nil {
		out.Notes = append(out.Notes, "model diagnostics unavailable: "+err.Error())
	} else {
		out.Models = models
	}
	base, comparable := out.ScenarioCache[steadyCacheBaseline]
	if !comparable {
		out.Notes = append(out.Notes, "steady sequential cache baseline unavailable; parallel cache regression could not be evaluated")
	}
	for _, scenario := range []string{"same-key-parallel", "different-key-parallel"} {
		parallelRate, ok := out.ScenarioCache[scenario]
		if comparable && ok && base-parallelRate > cacheRegressionLimit {
			out.CacheRegression = true
		}
	}
	if len(out.APIKeyCache) != len(keys) {
		out.Notes = append(out.Notes, "not every disposable key appeared in cache diagnostics")
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		cleanup()
		fatal(err)
	}
	cleanup()
	fmt.Println(string(raw))
	if !out.AllExact || out.CacheRegression {
		os.Exit(2)
	}
}

func runScenario(ctx context.Context, opt options, scenario string, cases []cliCase) []cliResult {
	results := make([]cliResult, len(cases))
	var wg sync.WaitGroup
	for i := range cases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = runCLI(ctx, opt, scenario, cases[i])
		}(i)
	}
	wg.Wait()
	return results
}

func runCLI(parent context.Context, opt options, scenario string, tc cliCase) cliResult {
	expected := tc.Name + "|73|ZWYX"
	result := cliResult{Scenario: scenario, Phase: scenarioPhase(scenario), Name: tc.Name, KeyLabel: tc.Key.Label, Requested: tc.Model, ExpectedModel: tc.ExpectedModel, Effort: tc.Effort, Expected: expected}
	prompt := "Solve both tasks. First, find the least nonnegative integer x with x mod 8 = 1, x mod 5 = 3, and x mod 7 = 3. Second, arrange W,X,Y,Z exactly once with Z immediately before W, W before Y, Y before X, and X last. Return exactly " + expected + " and nothing else."
	ctx, cancel := context.WithTimeout(parent, opt.timeout+15*time.Second)
	defer cancel()
	containerName := "codex-matrix-" + strings.ToLower(strings.ReplaceAll(tc.Name, "_", "-")) + "-" + strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 10)
	args := []string{
		"run", "--rm", "--name", containerName, "--network", "host",
		"-e", "CODEX_HOME=/root/.codex",
		"-v", opt.configPath + ":/root/.codex/config.toml:ro",
		"-w", "/tmp", "--entrypoint", "timeout", opt.image,
		strconv.Itoa(int(opt.timeout.Seconds())) + "s", "codex", "exec", "--ephemeral", "--skip-git-repo-check", "--json",
		"-m", tc.Model,
		"-c", "model_reasoning_effort=" + strconv.Quote(tc.Effort),
		"-c", "model_providers.poolserver.experimental_bearer_token=" + strconv.Quote(tc.Key.Secret),
		prompt,
	}
	started := time.Now()
	cmd := exec.CommandContext(ctx, "docker", args...)
	raw, err := cmd.CombinedOutput()
	result.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Error = fmt.Sprintf("codex exit: %v: %s", err, bounded(redactSecrets(string(raw), tc.Key.Secret), 600))
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return result
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event map[string]interface{}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		switch event["type"] {
		case "item.completed":
			item, _ := event["item"].(map[string]interface{})
			if item["type"] == "agent_message" {
				result.Final, _ = item["text"].(string)
			}
		case "turn.completed":
			usage, _ := event["usage"].(map[string]interface{})
			result.InferenceCompleted = true
			result.Usage = cliUsage{
				Input:     intValue(usage["input_tokens"]),
				Cached:    intValue(usage["cached_input_tokens"]),
				Output:    intValue(usage["output_tokens"]),
				Reasoning: intValue(usage["reasoning_output_tokens"]),
			}
		case "turn.failed", "error":
			result.Error = bounded(redactSecrets(string(scanner.Bytes()), tc.Key.Secret), 600)
		}
	}
	if err := scanner.Err(); err != nil && result.Error == "" {
		result.Error = err.Error()
	}
	result.Exact = result.Final == expected
	if result.Usage.Input > 0 {
		result.CacheRate = float64(result.Usage.Cached) / float64(result.Usage.Input)
	}
	return result
}

func createKey(ctx context.Context, opt options, label, model, effort string) (disposableKey, error) {
	payload := map[string]interface{}{
		"label": label, "key_type": "downstream", "group_name": "cyber",
		"force_model": model, "force_effort": effort, "provider_hint": "codex",
		"enabled": true, "expires_at": time.Now().Add(time.Hour).Unix(),
	}
	var response struct {
		Key     string `json:"key"`
		KeyHash string `json:"key_hash"`
	}
	if err := adminJSON(ctx, opt, http.MethodPost, "/admin/api-keys", payload, &response); err != nil {
		return disposableKey{}, fmt.Errorf("create %s: %w", label, err)
	}
	if response.Key == "" || response.KeyHash == "" {
		return disposableKey{}, fmt.Errorf("create %s: response omitted key material", label)
	}
	return disposableKey{Label: label, Secret: response.Key, Hash: response.KeyHash}, nil
}

func deleteKey(ctx context.Context, opt options, hash string) error {
	return adminJSON(ctx, opt, http.MethodDelete, "/admin/api-keys/"+hash, nil, nil)
}

func fetchCacheRows(ctx context.Context, opt options, since, until int64) ([]cacheRow, error) {
	path := fmt.Sprintf("/admin/usage/cache?since=%d&until=%d&fields=by_api_key", since, until)
	var response struct {
		Rows []cacheRow `json:"by_api_key"`
	}
	if err := adminJSON(ctx, opt, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.Rows, nil
}

func fetchModels(ctx context.Context, opt options, since, until int64) ([]modelRow, error) {
	path := fmt.Sprintf("/admin/usage/by-model?since=%d&until=%d", since, until)
	var response struct {
		Models []modelRow `json:"models"`
	}
	err := adminJSON(ctx, opt, http.MethodGet, path, nil, &response)
	sort.Slice(response.Models, func(i, j int) bool { return response.Models[i].Model < response.Models[j].Model })
	return response.Models, err
}

func adminJSON(ctx context.Context, opt options, method, path string, payload interface{}, out interface{}) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(opt.baseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+opt.adminToken)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, bounded(string(raw), 600))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return err
		}
	}
	return nil
}

func scenarioCacheRates(results []cliResult) map[string]float64 {
	totals := map[string][2]int64{}
	for _, result := range results {
		v := totals[result.Scenario]
		v[0] += result.Usage.Cached
		v[1] += result.Usage.Input
		totals[result.Scenario] = v
	}
	out := map[string]float64{}
	for scenario, v := range totals {
		if v[1] > 0 {
			out[scenario] = float64(v[0]) / float64(v[1])
		}
	}
	return out
}

func scenarioGeneratedInferenceStats(results []cliResult) map[string]generatedInferenceStats {
	out := map[string]generatedInferenceStats{}
	for _, result := range results {
		if !result.InferenceCompleted {
			continue
		}
		row := out[result.Scenario]
		row.Requests++
		if result.Usage.Cached > 0 {
			row.HitRequests++
		}
		row.PromptTokens += result.Usage.Input
		row.CachedTokens += result.Usage.Cached
		out[result.Scenario] = row
	}
	for scenario, row := range out {
		if row.PromptTokens > 0 {
			row.TokenHitRate = float64(row.CachedTokens) / float64(row.PromptTokens)
			if row.TokenHitRate > 1 {
				row.TokenHitRate = 1
			}
			if row.TokenHitRate < 0 {
				row.TokenHitRate = 0
			}
		}
		out[scenario] = row
	}
	return out
}

func scenarioPhase(scenario string) string {
	switch {
	case scenario == coldWarmupScenario:
		return "cold"
	case scenario == steadyCacheBaseline:
		return "steady"
	case strings.HasSuffix(scenario, "-parallel"):
		return "parallel"
	default:
		return "other"
	}
}

func redactSecrets(value string, secrets ...string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func intValue(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func bounded(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "cli-matrix:", err)
	os.Exit(1)
}
