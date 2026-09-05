package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/anthropicwire"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/cloak"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/usage"
)

const (
	qualityErrorUnavailableThreshold = 3
	qualityProbeResponseLimit        = 2 << 20
	qualityActualLimit               = 256
	modelQualityCommonInstruction    = "Integrity check: solve exactly. Return only the requested compact answer; no explanation or formatting."
)

type modelQualityProbe struct {
	ID       string
	Family   string
	Category string
	Prompt   string
	Expected string
}

// Primary probes rotate hourly. Each requires several deterministic reasoning
// steps but only a one-token/short-code answer, which is a much better
// intelligence-per-token signal than trivia or open-ended grading.
var modelQualityPrimaryProbes = []modelQualityProbe{
	{ID: "modular-state-v1", Family: "generic", Category: "reasoning", Prompt: "Start n=17. Repeat exactly four times: n=(3*n+5) mod 97. Then compute the bitwise XOR of n and 23. Reply with only the final base-10 integer.", Expected: "14"},
	{ID: "code-trace-v1", Family: "generic", Category: "reasoning", Prompt: "Trace this exactly: x=2; for i in [1,2,3,4], set x=x*i+(i mod 2). Reply with only the final integer x.", Expected: "76"},
	{ID: "truth-box-v1", Family: "generic", Category: "logic", Prompt: "A coin is in exactly one of boxes A, B, C. A says 'the coin is not in B'. B says 'the coin is in C'. C says 'the coin is not in C'. Exactly one statement is true. Reply with only A, B, or C for the coin's box.", Expected: "B"},
	{ID: "crt-v1", Family: "generic", Category: "reasoning", Prompt: "Find the smallest positive integer n such that n mod 5=2, n mod 7=4, and n mod 9=8. Reply with only n.", Expected: "242"},
}

// Family probes are short, deterministic and deliberately different from the generic
// bank. They test arithmetic/state tracking, constraints, contradiction handling and
// exact instruction following without asking for a long answer that would add noise.
var modelQualityFamilyProbes = map[string][]modelQualityProbe{
	"gpt": {
		{ID: "gpt-arithmetic-v2", Family: "gpt", Category: "reasoning", Prompt: "Compute (19*23+7) mod 29. Reply with only the integer.", Expected: "9"},
		{ID: "gpt-order-v2", Family: "gpt", Category: "logic", Prompt: "A is left of B; C is immediately right of A; D is left of C. Reply only the four-card order.", Expected: "DACB"},
		{ID: "gpt-contradiction-v1", Family: "gpt", Category: "consistency", Prompt: "Can an integer n satisfy n mod 4=1 and n mod 4=2? Reply exactly NO-SOLUTION or POSSIBLE.", Expected: "NO-SOLUTION"},
		{ID: "gpt-format-v1", Family: "gpt", Category: "instruction", Prompt: "Reply exactly G7|31|Q. Do not add text, spaces, or punctuation.", Expected: "G7|31|Q"},
	},
	"claude": {
		{ID: "claude-state-v1", Family: "claude", Category: "reasoning", Prompt: "Start n=11. Repeat three times: n=(5*n+1) mod 31. Then add 7. Reply with only the integer.", Expected: "18"},
		{ID: "claude-order-v1", Family: "claude", Category: "logic", Prompt: "Order A,B,C,D: C is before A; A is immediately before D; B is after D. Reply only the order.", Expected: "CADB"},
		{ID: "claude-contradiction-v1", Family: "claude", Category: "consistency", Prompt: "Can an integer be both even and odd? Reply exactly NO or YES.", Expected: "NO"},
		{ID: "claude-format-v1", Family: "claude", Category: "instruction", Prompt: "Reply exactly C4|OK|17. Do not add text, spaces, or punctuation.", Expected: "C4|OK|17"},
	},
	"gpt-6": {
		{ID: "gpt6-symbolic-v1", Family: "gpt-6", Category: "reasoning", Prompt: "Start a=7,b=11. Three times simultaneously set a,b=(a+b mod 29,a+2*b mod 29), using old values. Then reply with a XOR b.", Expected: "30"},
		{ID: "gpt6-state-v1", Family: "gpt-6", Category: "reasoning", Prompt: "Start x=3. For i=1..5 set x=(x*x+i) mod 101. Reply only x.", Expected: "1"},
		{ID: "gpt6-constraint-v1", Family: "gpt-6", Category: "logic", Prompt: "A is left of B; C is immediately right of A; D is left of C. Reply only the four-card order.", Expected: "DACB"},
		{ID: "gpt6-contradiction-v1", Family: "gpt-6", Category: "consistency", Prompt: "Can an integer n satisfy n mod 6=1 and n mod 6=4? Reply exactly NO-SOLUTION or POSSIBLE.", Expected: "NO-SOLUTION"},
	},
}

var modelQualityConfirmationProbe = modelQualityProbe{
	ID:       "independent-confirm-v1",
	Family:   "generic",
	Category: "confirmation",
	Prompt:   "Solve both independent checks. (1) f(1)=2 and f(n)=2*f(n-1)+n; find f(5). (2) Order W,X,Y,Z given Z is before W, W is immediately before Y, and X is after Y. Reply only as number|four-letter-order, with no spaces.",
	Expected: "73|ZWYX",
}

var modelQualityMetadataProbe = modelQualityProbe{
	ID:       "upstream-metadata-v1",
	Family:   "generic",
	Category: "metadata",
	Prompt:   "Reply with only one compact JSON object, no Markdown: {\"model\":\"exact model identity or unknown\",\"knowledge_cutoff\":\"YYYY-MM-DD or unknown\",\"knowledge_base_updated_at\":\"YYYY-MM-DD or unknown\"}. Do not guess; use unknown when unavailable.",
}

var modelQualityFamilyConfirmationProbes = map[string]modelQualityProbe{
	"gpt":    {ID: "gpt-independent-confirm-v1", Family: "gpt", Category: "confirmation", Prompt: "Solve both checks. (1) Start y=4; repeat three times y=3*y-2; (2) order P,Q,R,S with Q before P, P immediately before R, S after R. Reply only number|order.", Expected: "76|QPRS"},
	"claude": {ID: "claude-independent-confirm-v1", Family: "claude", Category: "confirmation", Prompt: "Solve both checks. (1) Find (17*17+5) mod 23; (2) order L,M,N,O with M before L, L immediately before N, O after N. Reply only number|order.", Expected: "18|MLNO"},
	"gpt-6":  {ID: "gpt6-independent-confirm-v1", Family: "gpt-6", Category: "confirmation", Prompt: "Solve both checks. (1) Start y=5; repeat four times y=(2*y+3) mod 41; (2) order H,I,J,K with I before H, H immediately before J, K after J. Reply only number|order.", Expected: "2|IHJK"},
}

type modelQualityCombo struct {
	Group    string `json:"group_name"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

func (c modelQualityCombo) key() string { return c.Group + "\x00" + c.Model + "\x00" + c.Provider }

type modelQualityProbeResult struct {
	Run        storage.ModelQualityRun
	Pass       bool
	ModelMatch bool
	Err        error
}

type modelQualityRunRequest struct {
	Group    string `json:"group_name"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

type modelQualityStatusView struct {
	storage.ModelQualityStatus
	ModelFingerprint       string `json:"model_fingerprint,omitempty"`
	ModelFingerprintSource string `json:"model_fingerprint_source,omitempty"`
	KnowledgeBaseUpdatedAt string `json:"knowledge_base_updated_at,omitempty"`
	KnowledgeBaseSource    string `json:"knowledge_base_source,omitempty"`
	CatalogObservedAt      int64  `json:"catalog_observed_at,omitempty"`
	MetadataProbeAt        int64  `json:"metadata_probe_at,omitempty"`
}

type modelQualityCapabilitySignal struct {
	ObservedAt int64
}

func (s *Server) adminModelQuality(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	statuses, err := s.store.ListModelQualityStatuses(r.Context(), group, model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	combos, comboErr := s.modelQualityCombos(r.Context())
	if comboErr == nil {
		statuses = mergeUnknownModelQualityStatuses(statuses, combos, group, model)
	}
	runs, err := s.store.ListModelQualityRuns(r.Context(), group, model, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	statusViews := s.modelQualityStatusViews(r.Context(), statuses, runs)
	s.qualityMu.Lock()
	running := s.qualityRunning
	s.qualityMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":                   s.flagEnabled(r.Context(), "model_quality_monitor_enabled", s.cfg.ModelQualityMonitorEnabled),
		"interval_minutes":          s.modelQualityIntervalMinutes(r.Context()),
		"reasoning_effort":          s.modelQualityReasoningEffort(r.Context()),
		"primary_reasoning_effort":  s.modelQualityReasoningEffortForPhase(r.Context(), "primary"),
		"confirm_reasoning_effort":  s.modelQualityReasoningEffortForPhase(r.Context(), "confirmation"),
		"degraded_threshold":        s.modelQualityDegradedThreshold(r.Context()),
		"scope":                     "group_model",
		"per_account_testing":       false,
		"anomaly_only_confirmation": true,
		"suite_version":             "family-short-metadata-v3",
		"suite_policy":              "每个分组×模型每轮只发一条短题；模型身份和知识库字段最多每天向上游询问一次；只有答案或返回模型异常时才发独立复核。模型自报字段用于交叉校验，不能单凭自报证明官方权重。",
		"probe_catalog":             modelQualityProbeCatalog(),
		"running":                   running,
		"statuses":                  statusViews,
		"runs":                      runs,
	})
}

func (s *Server) adminModelQualityRun(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req modelQualityRunRequest
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	statuses, err := s.runModelQualityChecks(r.Context(), req, true)
	if errors.Is(err, errModelQualityRunning) {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"checked": len(statuses), "statuses": statuses})
}

func (s *Server) adminModelQualityReset(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req modelQualityRunRequest
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.DeleteModelQualityStatus(r.Context(), strings.TrimSpace(req.Group), strings.TrimSpace(req.Model), strings.TrimSpace(req.Provider)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

var errModelQualityRunning = errors.New("model-quality check already running")

func (s *Server) beginModelQualityRun() bool {
	s.qualityMu.Lock()
	defer s.qualityMu.Unlock()
	if s.qualityRunning {
		return false
	}
	s.qualityRunning = true
	return true
}

func (s *Server) endModelQualityRun() {
	s.qualityMu.Lock()
	s.qualityRunning = false
	s.qualityMu.Unlock()
}

// StartModelQualityMonitor checks for due group×model rows once per minute. The
// expensive operation is still hourly: due timestamps gate upstream calls, while
// the short local tick makes settings changes hot without restarting the server.
func (s *Server) StartModelQualityMonitor(ctx context.Context) {
	supervisor.Go(ctx, "model-quality-monitor", func(ctx context.Context) {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			if s.flagEnabled(ctx, "model_quality_monitor_enabled", s.cfg.ModelQualityMonitorEnabled) {
				statuses, err := s.runModelQualityChecks(ctx, modelQualityRunRequest{}, false)
				if err != nil && !errors.Is(err, errModelQualityRunning) {
					log.Printf("[MODEL-QUALITY] sweep failed: %v", err)
				} else if len(statuses) > 0 {
					log.Printf("[MODEL-QUALITY] checked=%d scope=group_model", len(statuses))
				}
				historyDays := s.settingInt(ctx, "model_quality_history_days", s.cfg.ModelQualityHistoryDays)
				if historyDays <= 0 {
					historyDays = config.DefaultModelQualityHistoryDays
				}
				_, _ = s.store.PurgeModelQualityRunsBefore(ctx, storage.Now()-int64(historyDays*86400))
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func (s *Server) runModelQualityChecks(ctx context.Context, filter modelQualityRunRequest, force bool) ([]storage.ModelQualityStatus, error) {
	if !s.beginModelQualityRun() {
		return nil, errModelQualityRunning
	}
	defer s.endModelQualityRun()
	combos, err := s.modelQualityCombos(ctx)
	if err != nil {
		return nil, err
	}
	filter.Group = strings.TrimSpace(filter.Group)
	filter.Model = strings.TrimSpace(filter.Model)
	filter.Provider = strings.TrimSpace(filter.Provider)
	interval := int64(s.modelQualityIntervalMinutes(ctx) * 60)
	now := storage.Now()
	out := []storage.ModelQualityStatus{}
	for _, combo := range combos {
		if filter.Group != "" && combo.Group != filter.Group || filter.Model != "" && combo.Model != filter.Model || filter.Provider != "" && combo.Provider != filter.Provider {
			continue
		}
		previous, found, err := s.store.GetModelQualityStatus(ctx, combo.Group, combo.Model, combo.Provider)
		if err != nil {
			return out, err
		}
		if !force && found && previous.LastProbeAt+interval > now {
			continue
		}
		status, err := s.evaluateModelQualityCombo(ctx, combo, previous, found)
		if err != nil {
			log.Printf("[MODEL-QUALITY] group=%s model=%s provider=%s: %v", combo.Group, combo.Model, combo.Provider, err)
			continue
		}
		out = append(out, status)
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
	}
	return out, nil
}

func (s *Server) modelQualityCombos(ctx context.Context) ([]modelQualityCombo, error) {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	active := make([]storage.Account, 0, len(accounts))
	for _, account := range accounts {
		if account.Status == "active" {
			active = append(active, account)
		}
	}
	providers, err := s.store.ResolveAccountProviders(ctx, active)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(active))
	for _, account := range active {
		ids = append(ids, account.ID)
	}
	capsByAccount, err := s.store.ListCapabilitiesByAccountIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	customProviders, _ := s.store.ListCustomProviders(ctx)
	customModels := map[string][]string{}
	for _, provider := range customProviders {
		if provider.Enabled {
			customModels[provider.ID] = provider.Models
		}
	}
	allowed := map[string]bool{}
	for _, model := range s.settingCSV(ctx, "model_quality_models", s.cfg.ModelQualityModels) {
		allowed[strings.ToLower(strings.TrimSpace(model))] = true
	}
	seen := map[string]modelQualityCombo{}
	for _, account := range active {
		provider := strings.TrimSpace(providers[account.ID])
		if provider == "" || provider == "unknown" {
			continue
		}
		models := make([]string, 0)
		for _, c := range capsByAccount[account.ID] {
			models = append(models, c.ModelSlug)
		}
		if len(models) == 0 {
			switch provider {
			case "codex":
				for _, c := range capability.StaticCodexModels(account.ID) {
					models = append(models, c.ModelSlug)
				}
			case "claude":
				for _, c := range capability.StaticClaudeModels(account.ID) {
					models = append(models, c.ModelSlug)
				}
			default:
				models = append(models, customModels[provider]...)
			}
		}
		for _, model := range models {
			model = strings.TrimSpace(model)
			if model == "" || len(allowed) > 0 && !allowed[strings.ToLower(model)] {
				continue
			}
			if provider == "kiro" && (!capability.KiroSupportsAdaptiveThinking(model) || !capability.KiroPlanAllowsBootstrap(account.PlanType, model)) {
				continue
			}
			combo := modelQualityCombo{Group: account.GroupName, Model: model, Provider: provider}
			seen[combo.key()] = combo
		}
	}
	out := make([]modelQualityCombo, 0, len(seen))
	for _, combo := range seen {
		out = append(out, combo)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Provider < out[j].Provider
	})
	return out, nil
}

func mergeUnknownModelQualityStatuses(statuses []storage.ModelQualityStatus, combos []modelQualityCombo, group, model string) []storage.ModelQualityStatus {
	active := map[string]bool{}
	for _, combo := range combos {
		if group != "" && combo.Group != group || model != "" && combo.Model != model {
			continue
		}
		active[combo.key()] = true
	}
	filtered := make([]storage.ModelQualityStatus, 0, len(active))
	seen := map[string]bool{}
	for _, status := range statuses {
		key := modelQualityCombo{Group: status.GroupName, Model: status.ModelSlug, Provider: status.Provider}.key()
		if active[key] {
			filtered = append(filtered, status)
			seen[key] = true
		}
	}
	statuses = filtered
	for _, combo := range combos {
		if !active[combo.key()] || seen[combo.key()] {
			continue
		}
		statuses = append(statuses, storage.ModelQualityStatus{GroupName: combo.Group, ModelSlug: combo.Model, Provider: combo.Provider, State: "unknown"})
	}
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].GroupName != statuses[j].GroupName {
			return statuses[i].GroupName < statuses[j].GroupName
		}
		if statuses[i].ModelSlug != statuses[j].ModelSlug {
			return statuses[i].ModelSlug < statuses[j].ModelSlug
		}
		return statuses[i].Provider < statuses[j].Provider
	})
	return statuses
}

func (s *Server) evaluateModelQualityCombo(ctx context.Context, combo modelQualityCombo, previous storage.ModelQualityStatus, found bool) (storage.ModelQualityStatus, error) {
	now := storage.Now()
	probe := selectModelQualityProbe(combo, now, s.modelQualityIntervalMinutes(ctx))
	primary := s.executeModelQualityProbe(ctx, combo, probe, "primary", nil)
	if _, err := s.store.InsertModelQualityRun(ctx, primary.Run); err != nil {
		return storage.ModelQualityStatus{}, err
	}
	var metadataTokens int64
	if s.modelQualityMetadataDue(ctx, combo, now) {
		metadata := s.executeModelQualityProbe(ctx, combo, modelQualityMetadataProbe, "metadata", nil)
		metadataTokens = metadata.Run.TotalTokens
		if metadata.Run.Outcome == "metadata" {
			if _, err := s.store.InsertModelQualityRun(ctx, metadata.Run); err != nil {
				return storage.ModelQualityStatus{}, err
			}
		} else {
			// Metadata is advisory and must never turn a passing quality probe
			// into a degraded verdict. Keep failures in history for diagnosis.
			metadata.Run.Outcome = "metadata_error"
			_, _ = s.store.InsertModelQualityRun(ctx, metadata.Run)
		}
	}
	status := previous
	if !found {
		status = storage.ModelQualityStatus{GroupName: combo.Group, ModelSlug: combo.Model, Provider: combo.Provider, State: "unknown"}
	}
	status.GroupName, status.ModelSlug, status.Provider = combo.Group, combo.Model, combo.Provider
	status.LastProbeAt, status.UpdatedAt = now, now
	status.TotalChecks++
	status.TotalTokens += primary.Run.TotalTokens + metadataTokens
	status.LastProbeID = primary.Run.ProbeID
	status.LastExpected = primary.Run.Expected
	status.LastActual = primary.Run.Actual
	status.LastReturnedModel = primary.Run.ReturnedModel
	status.LastLatencyMS = primary.Run.LatencyMS

	if primary.Err != nil {
		status.LastOutcome = "error"
		status.ConsecutiveErrors++
		if status.State == "" {
			status.State = "unknown"
		}
		if status.ConsecutiveErrors >= qualityErrorUnavailableThreshold && status.State != "degraded" {
			status.State = "unavailable"
		}
		return status, s.store.UpsertModelQualityStatus(ctx, status)
	}

	if primary.Pass && primary.ModelMatch {
		applyModelQualityPass(&status, now, "pass")
		return status, s.store.UpsertModelQualityStatus(ctx, status)
	}

	exclude := map[string]bool{}
	if primary.Run.AccountID != "" {
		exclude[primary.Run.AccountID] = true
	}
	confirmationProbe := modelQualityConfirmationProbeFor(combo)
	confirmation := s.executeModelQualityProbe(ctx, combo, confirmationProbe, "confirmation", exclude)
	if confirmation.Err != nil && strings.Contains(confirmation.Run.ErrorKind, "scheduler") && len(exclude) > 0 {
		confirmation = s.executeModelQualityProbe(ctx, combo, confirmationProbe, "confirmation", nil)
	}
	if _, err := s.store.InsertModelQualityRun(ctx, confirmation.Run); err != nil {
		return storage.ModelQualityStatus{}, err
	}
	status.TotalTokens += confirmation.Run.TotalTokens
	status.LastLatencyMS += confirmation.Run.LatencyMS
	if confirmation.Err != nil {
		status.LastOutcome = "inconclusive"
		status.ConsecutiveErrors++
		if status.State == "" {
			status.State = "unknown"
		}
		if status.ConsecutiveErrors >= qualityErrorUnavailableThreshold && status.State != "degraded" {
			status.State = "unavailable"
		}
		return status, s.store.UpsertModelQualityStatus(ctx, status)
	}
	status.ConsecutiveErrors = 0
	if confirmation.Pass && confirmation.ModelMatch {
		status.LastProbeID = confirmation.Run.ProbeID
		status.LastExpected = confirmation.Run.Expected
		status.LastActual = confirmation.Run.Actual
		status.LastReturnedModel = confirmation.Run.ReturnedModel
		applyModelQualityPass(&status, now, "false_alarm")
		return status, s.store.UpsertModelQualityStatus(ctx, status)
	}
	status.LastOutcome = "confirmed_anomaly"
	status.LastProbeID = confirmation.Run.ProbeID
	status.LastExpected = confirmation.Run.Expected
	status.LastActual = confirmation.Run.Actual
	status.LastReturnedModel = confirmation.Run.ReturnedModel
	status.ConsecutiveAnomalies++
	if status.ConsecutiveAnomalies >= s.modelQualityDegradedThreshold(ctx) {
		status.State = "degraded"
	} else {
		status.State = "suspect"
	}
	return status, s.store.UpsertModelQualityStatus(ctx, status)
}

func (s *Server) modelQualityMetadataDue(ctx context.Context, combo modelQualityCombo, now int64) bool {
	runs, err := s.store.ListModelQualityRuns(ctx, combo.Group, combo.Model, 20)
	if err != nil {
		return true
	}
	for _, run := range runs {
		if run.ProbeID == modelQualityMetadataProbe.ID && run.Outcome == "metadata" && now-run.CreatedAt < 24*60*60 {
			return false
		}
	}
	return true
}

func applyModelQualityPass(status *storage.ModelQualityStatus, now int64, outcome string) {
	status.LastOutcome = outcome
	status.LastPassAt = now
	status.ConsecutiveErrors = 0
	if status.ConsecutiveAnomalies > 0 {
		status.ConsecutiveAnomalies--
	}
	if status.ConsecutiveAnomalies == 0 {
		status.State = "healthy"
	} else {
		status.State = "suspect"
	}
}

func selectModelQualityProbe(combo modelQualityCombo, now int64, intervalMinutes int) modelQualityProbe {
	if intervalMinutes < 1 {
		intervalMinutes = config.DefaultModelQualityIntervalMinutes
	}
	probes := modelQualityProbesForCombo(combo)
	h := fnv.New32a()
	_, _ = h.Write([]byte(combo.key()))
	bucket := uint32(now / int64(intervalMinutes*60))
	return probes[(h.Sum32()+bucket)%uint32(len(probes))]
}

func modelQualityFamily(combo modelQualityCombo) string {
	model := strings.ToLower(strings.TrimSpace(combo.Model))
	if strings.Contains(model, "gpt-6") || strings.Contains(model, "gpt6") {
		return "gpt-6"
	}
	// Kiro exposes Claude model names but has a separate adapter and response
	// contract; keep its established generic probe path so adapter health is
	// measured without pretending it is a direct Anthropic endpoint.
	if combo.Provider == "kiro" {
		return "generic"
	}
	if combo.Provider == "claude" || strings.Contains(model, "claude") {
		return "claude"
	}
	if combo.Provider == "codex" || strings.HasPrefix(model, "gpt") {
		return "gpt"
	}
	return "generic"
}

func modelQualityProbesForCombo(combo modelQualityCombo) []modelQualityProbe {
	if probes := modelQualityFamilyProbes[modelQualityFamily(combo)]; len(probes) > 0 {
		return probes
	}
	return modelQualityPrimaryProbes
}

func modelQualityConfirmationProbeFor(combo modelQualityCombo) modelQualityProbe {
	if probe, ok := modelQualityFamilyConfirmationProbes[modelQualityFamily(combo)]; ok {
		return probe
	}
	return modelQualityConfirmationProbe
}

func modelQualityProbeCatalog() []map[string]interface{} {
	families := []string{"gpt", "claude", "gpt-6", "generic"}
	catalog := make([]map[string]interface{}, 0, len(families))
	for _, family := range families {
		probes := modelQualityPrimaryProbes
		if familyProbes := modelQualityFamilyProbes[family]; len(familyProbes) > 0 {
			probes = familyProbes
		}
		ids := make([]string, 0, len(probes))
		categories := make([]string, 0, len(probes))
		seenCategories := map[string]bool{}
		for _, probe := range probes {
			ids = append(ids, probe.ID)
			if probe.Category != "" && !seenCategories[probe.Category] {
				categories = append(categories, probe.Category)
				seenCategories[probe.Category] = true
			}
		}
		confirmation := modelQualityConfirmationProbe
		if familyProbe, ok := modelQualityFamilyConfirmationProbes[family]; ok {
			confirmation = familyProbe
		}
		catalog = append(catalog, map[string]interface{}{
			"family": family, "categories": categories, "primary_probe_ids": ids,
			"confirmation_probe_id": confirmation.ID, "metadata_probe_id": modelQualityMetadataProbe.ID,
		})
	}
	return catalog
}

func (s *Server) modelQualityStatusViews(ctx context.Context, statuses []storage.ModelQualityStatus, runs []storage.ModelQualityRun) []modelQualityStatusView {
	signals := s.modelQualityCapabilitySignals(ctx)
	metadata := map[string]struct {
		answer modelQualityMetadataAnswer
		at     int64
	}{}
	for _, run := range runs {
		if run.ProbeID != modelQualityMetadataProbe.ID || run.Outcome != "metadata" {
			continue
		}
		key := modelQualityCombo{Group: run.GroupName, Model: run.ModelSlug, Provider: run.Provider}.key()
		if current, ok := metadata[key]; ok && current.at >= run.CreatedAt {
			continue
		}
		if answer, ok := parseModelQualityMetadata(run.Actual); ok {
			metadata[key] = struct {
				answer modelQualityMetadataAnswer
				at     int64
			}{answer: answer, at: run.CreatedAt}
		}
	}
	byStatus := make([]modelQualityStatusView, 0, len(statuses))
	for _, status := range statuses {
		key := modelQualityCombo{Group: status.GroupName, Model: status.ModelSlug, Provider: status.Provider}.key()
		signal := signals[key]
		fingerprint, source, knowledge, knowledgeSource, metadataAt := "", "", "", "", int64(0)
		if item, ok := metadata[key]; ok {
			metadataAt = item.at
			if item.answer.Model != "" {
				fingerprint, source = "reported:"+item.answer.Model, "upstream_model_probe"
			}
			knowledge = firstNonEmpty(item.answer.KnowledgeBaseUpdatedAt, item.answer.KnowledgeCutoff)
			if knowledge != "" {
				knowledgeSource = "upstream_model_probe"
			}
		}
		if fingerprint == "" && status.LastReturnedModel != "" {
			fingerprint, source = "response:"+status.LastReturnedModel, "upstream_response_header"
		}
		if knowledge == "" {
			knowledgeSource = "model_probe_unreported"
		}
		byStatus = append(byStatus, modelQualityStatusView{
			ModelQualityStatus:     status,
			ModelFingerprint:       fingerprint,
			ModelFingerprintSource: source,
			KnowledgeBaseUpdatedAt: knowledge,
			KnowledgeBaseSource:    knowledgeSource,
			CatalogObservedAt:      signal.ObservedAt,
			MetadataProbeAt:        metadataAt,
		})
	}
	return byStatus
}

// modelQualityCapabilitySignals only supplies the last catalog observation time.
// The displayed identity now comes from an explicit upstream model question;
// catalog hashes are deliberately not presented as model fingerprints.
func (s *Server) modelQualityCapabilitySignals(ctx context.Context) map[string]modelQualityCapabilitySignal {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return nil
	}
	active := make([]storage.Account, 0, len(accounts))
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if account.Status == "active" {
			active = append(active, account)
			ids = append(ids, account.ID)
		}
	}
	providers, err := s.store.ResolveAccountProviders(ctx, active)
	if err != nil {
		return nil
	}
	capsByAccount, err := s.store.ListCapabilitiesSummaryByAccountIDs(ctx, ids)
	if err != nil {
		return nil
	}
	out := map[string]modelQualityCapabilitySignal{}
	for _, account := range active {
		provider := strings.TrimSpace(providers[account.ID])
		for _, cap := range capsByAccount[account.ID] {
			key := modelQualityCombo{Group: account.GroupName, Model: cap.ModelSlug, Provider: provider}.key()
			current := out[key]
			if cap.LastProbeAt < current.ObservedAt {
				continue
			}
			current.ObservedAt = cap.LastProbeAt
			out[key] = current
		}
	}
	return out
}

type modelQualityMetadataAnswer struct {
	Model                  string `json:"model"`
	KnowledgeCutoff        string `json:"knowledge_cutoff"`
	KnowledgeBaseUpdatedAt string `json:"knowledge_base_updated_at"`
}

func parseModelQualityMetadata(actual string) (modelQualityMetadataAnswer, bool) {
	actual = strings.TrimSpace(actual)
	if strings.HasPrefix(actual, "```") {
		actual = strings.TrimSpace(strings.TrimPrefix(actual, "```json"))
		actual = strings.TrimSpace(strings.TrimSuffix(actual, "```"))
	}
	var answer modelQualityMetadataAnswer
	if json.Unmarshal([]byte(actual), &answer) != nil {
		return modelQualityMetadataAnswer{}, false
	}
	answer.Model = strings.TrimSpace(answer.Model)
	answer.KnowledgeCutoff = strings.TrimSpace(answer.KnowledgeCutoff)
	answer.KnowledgeBaseUpdatedAt = strings.TrimSpace(answer.KnowledgeBaseUpdatedAt)
	if strings.EqualFold(answer.Model, "unknown") {
		answer.Model = ""
	}
	if strings.EqualFold(answer.KnowledgeCutoff, "unknown") {
		answer.KnowledgeCutoff = ""
	}
	if strings.EqualFold(answer.KnowledgeBaseUpdatedAt, "unknown") {
		answer.KnowledgeBaseUpdatedAt = ""
	}
	return answer, answer.Model != "" || answer.KnowledgeCutoff != "" || answer.KnowledgeBaseUpdatedAt != ""
}

func (s *Server) executeModelQualityProbe(ctx context.Context, combo modelQualityCombo, probe modelQualityProbe, phase string, exclude map[string]bool) modelQualityProbeResult {
	started := time.Now()
	run := storage.ModelQualityRun{GroupName: combo.Group, ModelSlug: combo.Model, Provider: combo.Provider, ProbeID: probe.ID, Phase: phase, Expected: probe.Expected, CreatedAt: storage.Now()}
	result := modelQualityProbeResult{Run: run}
	probeCtx, cancel := context.WithTimeout(ctx, s.modelQualityProbeTimeout())
	defer cancel()
	route := scheduler.Route{Group: combo.Group, Provider: combo.Provider, Model: combo.Model, EstimatedTokens: 256, Exclude: exclude}
	if combo.Provider == "kiro" {
		kiroCfg := s.effectiveKiroConfig(probeCtx)
		route.ThinkingRequired = true
		route.KiroEndpointAllowlist = kiroCfg.KiroEndpointAllowlist
		route.KiroDefaultRegion = kiroCfg.KiroDefaultAPIRegion
	}
	lease, err := s.scheduler.Select(probeCtx, route)
	if err != nil {
		result.Err = err
		result.Run.Outcome = "error"
		result.Run.ErrorKind = "scheduler"
		result.Run.ErrorMessage = qualitySnippet(err.Error(), qualityActualLimit)
		result.Run.LatencyMS = time.Since(started).Milliseconds()
		return result
	}
	defer lease.Release()
	result.Run.AccountID = lease.Account.ID
	if combo.Provider == "kiro" {
		return s.executeKiroModelQualityProbe(probeCtx, combo, probe, phase, lease, result, started)
	}
	token, err := s.store.GetToken(probeCtx, lease.Account.ID)
	if err != nil {
		return qualityProbeError(result, "token", err, started)
	}
	if combo.Provider == "claude" {
		token, err = s.prepareClaudeToken(probeCtx, lease.Account, token, "model_quality")
		if err != nil {
			return qualityProbeError(result, "token_refresh", err, started)
		}
	}
	spec, err := s.modelQualityUpstreamRequest(probeCtx, combo, probe, phase, lease, token)
	if err != nil {
		return qualityProbeError(result, "request", err, started)
	}
	resp, err := s.upstream.Do(probeCtx, spec)
	if err != nil {
		return qualityProbeError(result, "transport", err, started)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, qualityProbeResponseLimit))
	result.Run.LatencyMS = time.Since(started).Milliseconds()
	result.Run.HTTPStatus = resp.StatusCode
	if readErr != nil {
		return qualityProbeError(result, "read", readErr, started)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = fmt.Errorf("upstream status %d: %s", resp.StatusCode, bodySnippet(raw, 160))
		return qualityProbeError(result, "http_status", err, started)
	}
	actual, returnedModel, parsed := parseModelQualityResponse(raw, combo.Provider, resp.Header.Get("Content-Type"))
	result.Run.Actual = qualitySnippet(strings.TrimSpace(actual), qualityActualLimit)
	result.Run.ReturnedModel = qualitySnippet(returnedModel, 128)
	result.Run.PromptTokens = parsed.PromptTokens
	result.Run.CompletionTokens = parsed.CompletionTokens
	result.Run.TotalTokens = parsed.TotalTokens
	if probe.Category == "metadata" {
		if answer, ok := parseModelQualityMetadata(actual); ok {
			compact, _ := json.Marshal(answer)
			result.Run.Actual = string(compact)
			result.Run.Outcome = "metadata"
			result.Pass = true
			result.ModelMatch = true
			return result
		}
		result.Run.Outcome = "metadata_error"
		result.Run.ErrorKind = "invalid_metadata"
		result.Run.ErrorMessage = "upstream metadata response was not a compact JSON object"
		return result
	}
	result.Pass = qualityAnswerMatches(actual, probe.Expected)
	result.ModelMatch = qualityModelMatches(combo.Model, returnedModel)
	switch {
	case strings.TrimSpace(actual) == "":
		result.Err = errors.New("empty model response")
		result.Run.Outcome = "error"
		result.Run.ErrorKind = "empty_response"
		result.Run.ErrorMessage = "empty model response"
	case !result.ModelMatch:
		result.Run.Outcome = "model_mismatch"
	case !result.Pass:
		result.Run.Outcome = "incorrect"
	default:
		result.Run.Outcome = "pass"
	}
	return result
}

func (s *Server) executeKiroModelQualityProbe(ctx context.Context, combo modelQualityCombo, probe modelQualityProbe, phase string, lease scheduler.Lease, result modelQualityProbeResult, started time.Time) modelQualityProbeResult {
	payload := map[string]interface{}{
		"model": combo.Model, "max_tokens": 64, "stream": false,
		"system":        modelQualityCommonInstruction,
		"messages":      []interface{}{map[string]interface{}{"role": "user", "content": probe.Prompt}},
		"thinking":      map[string]interface{}{"type": "adaptive"},
		"output_config": map[string]interface{}{"effort": "max"},
	}
	raw, _ := json.Marshal(payload)
	affinity := routing.AffinityFromKey("model-quality:"+combo.key()+":"+probe.ID+":"+phase, "model_quality")
	converted, err := s.convertKiroRequest(ctx, raw, affinity, lease)
	if err != nil {
		return qualityProbeError(result, "request", err, started)
	}
	recorder := &probeResponseRecorder{header: http.Header{}}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://kiro.internal/model-quality", nil)
	data, endpointHash, _ := s.doKiroAttempt(recorder, request, &converted, lease)
	result.Run.LatencyMS = time.Since(started).Milliseconds()
	if data == nil {
		status := recorder.status
		if status <= 0 {
			status = http.StatusBadGateway
		}
		result.Run.HTTPStatus = status
		message := strings.TrimSpace(recorder.body.String())
		if message == "" {
			message = "Kiro model-quality request failed"
		}
		return qualityProbeError(result, "kiro_upstream", errors.New(message), started)
	}
	result.Run.HTTPStatus = http.StatusOK
	data.Model = firstNonEmpty(data.Model, converted.Model, combo.Model)
	s.observeKiroResponse(ctx, lease.Account.ID, endpointHash, converted, *data)
	result.Run.Actual = qualitySnippet(strings.TrimSpace(data.Text), qualityActualLimit)
	result.Run.ReturnedModel = qualitySnippet(data.Model, 128)
	result.Run.PromptTokens = data.InputTokens
	result.Run.CompletionTokens = data.OutputTokens
	result.Run.TotalTokens = data.InputTokens + data.OutputTokens
	if probe.Category == "metadata" {
		if answer, ok := parseModelQualityMetadata(data.Text); ok {
			compact, _ := json.Marshal(answer)
			result.Run.Actual = string(compact)
			result.Run.Outcome = "metadata"
			result.Pass = true
			result.ModelMatch = true
			return result
		}
		result.Run.Outcome = "metadata_error"
		result.Run.ErrorKind = "invalid_metadata"
		result.Run.ErrorMessage = "upstream metadata response was not a compact JSON object"
		return result
	}
	result.Pass = qualityAnswerMatches(data.Text, probe.Expected)
	result.ModelMatch = qualityModelMatches(combo.Model, data.Model)
	switch {
	case strings.TrimSpace(data.Text) == "":
		result.Err = errors.New("empty model response")
		result.Run.Outcome = "error"
		result.Run.ErrorKind = "empty_response"
		result.Run.ErrorMessage = "empty model response"
	case !result.ModelMatch:
		result.Run.Outcome = "model_mismatch"
	case !result.Pass:
		result.Run.Outcome = "incorrect"
	default:
		result.Run.Outcome = "pass"
	}
	return result
}

func qualityProbeError(result modelQualityProbeResult, kind string, err error, started time.Time) modelQualityProbeResult {
	result.Err = err
	result.Run.Outcome = "error"
	result.Run.ErrorKind = kind
	if err != nil {
		result.Run.ErrorMessage = qualitySnippet(err.Error(), qualityActualLimit)
	}
	result.Run.LatencyMS = time.Since(started).Milliseconds()
	return result
}

func (s *Server) modelQualityUpstreamRequest(ctx context.Context, combo modelQualityCombo, probe modelQualityProbe, phase string, lease scheduler.Lease, token storage.AccountToken) (upstream.Request, error) {
	commonInstruction := modelQualityCommonInstruction
	effort := s.modelQualityReasoningEffortForPhase(ctx, phase)
	spec := upstream.Request{Method: http.MethodPost, Provider: combo.Provider, Headers: http.Header{}, Account: lease.Account, Token: token, Egress: lease.Egress, CookieJarKey: lease.Binding.CookieJarKey, Model: combo.Model}
	switch combo.Provider {
	case "codex":
		payload := map[string]interface{}{
			"model": combo.Model,
			"input": []interface{}{
				map[string]interface{}{"role": "developer", "content": []interface{}{map[string]interface{}{"type": "input_text", "text": commonInstruction}}},
				map[string]interface{}{"role": "user", "content": []interface{}{map[string]interface{}{"type": "input_text", "text": probe.Prompt}}},
			},
			"tools": []interface{}{}, "tool_choice": "auto", "parallel_tool_calls": false,
			"reasoning": map[string]interface{}{"effort": effort, "context": "all_turns"},
			"text":      map[string]interface{}{"verbosity": "low"}, "store": false, "stream": true,
		}
		raw, _ := json.Marshal(payload)
		spec.SetBodyBytes(raw)
		spec.DownstreamPath = "/v1/responses"
		spec.CodexClientVersion = s.codexClientVersionForModel(combo.Model)
		spec.CodexResponsesWebSocket = s.codexResponsesWebSocketForModel(combo.Model, false, false, raw)
		if strings.EqualFold(lease.Egress.Type, "curl_cffi_sidecar") && s.flagEnabled(ctx, "codex_prefer_sidecar_ja3_over_ws", s.cfg.CodexPreferSidecarJA3OverWS) {
			spec.CodexResponsesWebSocket = false
		}
	case "claude":
		payload := map[string]interface{}{
			"model": combo.Model, "max_tokens": 64, "stream": false,
			"system": []interface{}{
				map[string]interface{}{"type": "text", "text": "You are a Claude agent, built on Anthropic's Claude Agent SDK.", "cache_control": map[string]interface{}{"type": "ephemeral"}},
				map[string]interface{}{"type": "text", "text": commonInstruction},
			},
			"messages":      []interface{}{map[string]interface{}{"role": "user", "content": []interface{}{map[string]interface{}{"type": "text", "text": probe.Prompt}}}},
			"thinking":      map[string]interface{}{"type": "adaptive", "display": "omitted"},
			"output_config": map[string]interface{}{"effort": effort},
		}
		raw, _ := anthropicwire.MarshalPreservingOrder(nil, payload)
		osHint := s.osHint(raw, lease.Egress)
		id := s.virtualIdentity(ctx, lease.Account.ID, osHint)
		oauth := claudeIsOAuth(token)
		billingVersion := s.cfg.ClaudeCLIVersionOrDefault(id.ClaudeCLIVersion)
		spec.SetBodyBytes(cloak.VirtualizeClaudeCode(raw, id, s.cfg.SensitiveWordsFor("claude"), oauth, billingVersion).Body)
		spec.OSHint = osHint
		spec.DownstreamPath = "/v1/messages"
	default:
		provider, ok := s.customProviderByID(ctx, combo.Provider)
		if !ok || !provider.Enabled {
			return upstream.Request{}, fmt.Errorf("custom provider %s is unavailable", combo.Provider)
		}
		spec.BaseURL = provider.BaseURL
		spec.TransportProfile = provider.TransportProfile
		spec.UpstreamProtocol = provider.UpstreamProtocol
		switch provider.UpstreamProtocol {
		case storage.CustomProviderProtocolResponses:
			payload := map[string]interface{}{
				"model":             combo.Model,
				"input":             []interface{}{map[string]interface{}{"role": "developer", "content": commonInstruction}, map[string]interface{}{"role": "user", "content": probe.Prompt}},
				"max_output_tokens": 32, "stream": false,
			}
			raw, _ := json.Marshal(payload)
			spec.SetBodyBytes(raw)
			spec.DownstreamPath = "/responses"
		case storage.CustomProviderProtocolAnthropicMessages:
			raw, osHint := s.claudeCodeMinimalProbeBody(
				ctx,
				lease.Account,
				token,
				lease.Egress,
				combo.Model,
				commonInstruction+"\n\n"+probe.Prompt,
				32,
			)
			spec.SetBodyBytes(raw)
			spec.OSHint = osHint
			spec.DownstreamPath = "/messages"
		default:
			payload := map[string]interface{}{
				"model":      combo.Model,
				"messages":   []interface{}{map[string]interface{}{"role": "system", "content": commonInstruction}, map[string]interface{}{"role": "user", "content": probe.Prompt}},
				"max_tokens": 32, "temperature": 0, "stream": false,
			}
			raw, _ := json.Marshal(payload)
			spec.SetBodyBytes(raw)
			spec.DownstreamPath = "/chat/completions"
		}
	}
	return spec, nil
}

func parseModelQualityResponse(raw []byte, provider, contentType string) (string, string, usage.Parsed) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") || bytes.Contains(raw, []byte("data:")) {
		scanner := usage.NewStreamScanner(provider)
		_, _ = scanner.Write(raw)
		parsed, _ := scanner.Parsed()
		var textOut strings.Builder
		returnedModel := parsed.Model
		lineScanner := bufio.NewScanner(bytes.NewReader(raw))
		lineScanner.Buffer(make([]byte, 1024), 1<<20)
		for lineScanner.Scan() {
			line := bytes.TrimSpace(lineScanner.Bytes())
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(line[len("data:"):])
			if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
				continue
			}
			var event map[string]interface{}
			if json.Unmarshal(data, &event) != nil {
				continue
			}
			if delta, _ := event["delta"].(string); delta != "" {
				textOut.WriteString(delta)
			}
			if delta, ok := event["delta"].(map[string]interface{}); ok {
				if value, _ := delta["text"].(string); value != "" {
					textOut.WriteString(value)
				}
			}
			if returnedModel == "" {
				returnedModel = qualityReturnedModel(event)
			}
			if textOut.Len() == 0 {
				if value := qualityTextFromJSON(event); value != "" {
					textOut.WriteString(value)
				}
			}
		}
		return textOut.String(), returnedModel, parsed
	}
	parsed := usage.ParseResponse(raw)
	var root map[string]interface{}
	if json.Unmarshal(raw, &root) != nil {
		return "", "", parsed
	}
	returnedModel := parsed.Model
	if returnedModel == "" {
		returnedModel = qualityReturnedModel(root)
	}
	return qualityTextFromJSON(root), returnedModel, parsed
}

func qualityReturnedModel(root map[string]interface{}) string {
	if model, _ := root["model"].(string); model != "" {
		return model
	}
	for _, key := range []string{"response", "message"} {
		if nested, ok := root[key].(map[string]interface{}); ok {
			if model, _ := nested["model"].(string); model != "" {
				return model
			}
		}
	}
	return ""
}

func qualityTextFromJSON(root map[string]interface{}) string {
	if value, _ := root["output_text"].(string); value != "" {
		return value
	}
	if value, _ := root["text"].(string); value != "" {
		return value
	}
	if content, ok := root["content"].([]interface{}); ok {
		var out strings.Builder
		for _, item := range content {
			if block, ok := item.(map[string]interface{}); ok {
				out.WriteString(qualityTextFromJSON(block))
			}
		}
		if out.Len() > 0 {
			return out.String()
		}
	}
	if output, ok := root["output"].([]interface{}); ok {
		for _, item := range output {
			if block, ok := item.(map[string]interface{}); ok {
				if value := qualityTextFromJSON(block); value != "" {
					return value
				}
			}
		}
	}
	if choices, ok := root["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			for _, key := range []string{"message", "delta"} {
				if nested, ok := choice[key].(map[string]interface{}); ok {
					if content, _ := nested["content"].(string); content != "" {
						return content
					}
				}
			}
		}
	}
	if response, ok := root["response"].(map[string]interface{}); ok {
		return qualityTextFromJSON(response)
	}
	return ""
}

func qualityAnswerMatches(actual, expected string) bool {
	return canonicalQualityAnswer(actual) == canonicalQualityAnswer(expected)
}

func canonicalQualityAnswer(value string) string {
	value = strings.TrimSpace(value)
	if lines := strings.FieldsFunc(value, func(r rune) bool { return r == '\n' || r == '\r' }); len(lines) > 0 {
		value = lines[len(lines)-1]
	}
	value = strings.TrimSpace(value)
	upper := strings.ToUpper(value)
	for _, prefix := range []string{"FINAL ANSWER:", "FINAL:", "ANSWER:"} {
		upper = strings.TrimSpace(strings.TrimPrefix(upper, prefix))
	}
	for _, tag := range []string{"<ANSWER>", "</ANSWER>", "`", "*"} {
		upper = strings.ReplaceAll(upper, tag, "")
	}
	upper = strings.Trim(upper, " \t\"'.,;:![](){}")
	upper = strings.Join(strings.Fields(upper), "")
	return upper
}

func qualityModelMatches(requested, returned string) bool {
	requested = strings.ToLower(strings.TrimSpace(requested))
	returned = strings.ToLower(strings.TrimSpace(returned))
	if returned == "" || requested == returned {
		return true
	}
	if !strings.HasPrefix(returned, requested+"-") {
		return false
	}
	// Providers commonly append an immutable date/build tag. Do not treat a
	// named smaller variant (for example "-mini") as the requested model.
	suffix := strings.TrimPrefix(returned, requested+"-")
	if suffix == "latest" || suffix == "stable" || suffix == "preview" {
		return true
	}
	if suffix == "" {
		return false
	}
	for _, r := range suffix {
		if (r < '0' || r > '9') && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func qualitySnippet(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit > 0 && len(value) > limit {
		return value[:limit]
	}
	return value
}

func (s *Server) modelQualityIntervalMinutes(ctx context.Context) int {
	minutes := s.settingInt(ctx, "model_quality_interval_minutes", s.cfg.ModelQualityIntervalMinutes)
	if minutes < 60 {
		return config.DefaultModelQualityIntervalMinutes
	}
	return minutes
}

func (s *Server) modelQualityReasoningEffort(ctx context.Context) string {
	effort := strings.ToLower(strings.TrimSpace(s.settingString(ctx, "model_quality_reasoning_effort", s.cfg.ModelQualityReasoningEffort)))
	switch effort {
	case "low", "medium", "high":
		return effort
	default:
		return config.DefaultModelQualityReasoningEffort
	}
}

// modelQualityReasoningEffortForPhase keeps the hourly healthy-path probe as
// cheap as configured, while giving an anomalous answer a stronger independent
// check before it can affect the group's quality state. High/medium operator
// choices are respected; only low is raised to medium for confirmation.
func (s *Server) modelQualityReasoningEffortForPhase(ctx context.Context, phase string) string {
	effort := s.modelQualityReasoningEffort(ctx)
	if phase == "confirmation" && effort == "low" {
		return "medium"
	}
	return effort
}

func (s *Server) modelQualityDegradedThreshold(ctx context.Context) int {
	threshold := s.settingInt(ctx, "model_quality_degraded_threshold", s.cfg.ModelQualityDegradedThreshold)
	if threshold < 2 {
		return config.DefaultModelQualityDegradedThreshold
	}
	return threshold
}

func (s *Server) modelQualityProbeTimeout() time.Duration {
	timeout := s.cfg.RequestTimeout()
	if timeout <= 0 || timeout > 2*time.Minute {
		return 2 * time.Minute
	}
	return timeout
}
