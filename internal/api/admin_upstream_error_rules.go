package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"codex-account-pool/internal/storage"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
)

// adminUpstreamErrorRules exposes the operator-managed upstream error rule list.
//
//	GET  /admin/upstream-error-rules
//	POST /admin/upstream-error-rules
func (s *Server) adminUpstreamErrorRules(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rules, err := s.store.ListUpstreamErrorRules(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if rules == nil {
			rules = []storage.UpstreamErrorRule{}
		}
		writeJSON(w, http.StatusOK, rules)
	case http.MethodPost:
		var req map[string]json.RawMessage
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		rule := defaultUpstreamErrorRule()
		rule.ID = upstreamErrorRuleID(req)
		if rule.ID == "" {
			rule.ID = "uer_" + strings.TrimPrefix(newRequestID(), "req_")
		}
		if err := overlayUpstreamErrorRule(&rule, req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := validateUpstreamErrorRule(&rule); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.store.UpsertUpstreamErrorRule(r.Context(), rule); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		out, ok, err := s.store.GetUpstreamErrorRule(r.Context(), rule.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeError(w, http.StatusInternalServerError, errors.New("rule was not persisted"))
			return
		}
		writeJSON(w, http.StatusOK, out)
	default:
		methodNotAllowed(w)
	}
}

// adminUpstreamErrorRuleAction handles PATCH/DELETE /admin/upstream-error-rules/{id}.
func (s *Server) adminUpstreamErrorRuleAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/upstream-error-rules/"), "/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		rule, ok, err := s.store.GetUpstreamErrorRule(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("rule not found"))
			return
		}
		var req map[string]json.RawMessage
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		delete(req, "id")
		if err := overlayUpstreamErrorRule(&rule, req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := validateUpstreamErrorRule(&rule); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.store.UpsertUpstreamErrorRule(r.Context(), rule); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		out, _, err := s.store.GetUpstreamErrorRule(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodDelete:
		if err := s.store.DeleteUpstreamErrorRule(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "deleted": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminUpstreamErrorRulesTest(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Provider   string `json:"provider"`
		Entrypoint string `json:"entrypoint"`
		Model      string `json:"model"`
		Status     int    `json:"status"`
		Body       string `json:"body"`
		Streaming  bool   `json:"streaming"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rules, err := s.store.ListUpstreamErrorRules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	input := upstreamrules.MatchInput{Provider: req.Provider, Entrypoint: req.Entrypoint, Model: req.Model, Status: req.Status, Body: []byte(req.Body), Streaming: req.Streaming}
	if match, ok := upstreamrules.Match(rules, input); ok {
		preview := upstreamrules.Preview(match.Rule, input)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"matched":           true,
			"rule":              match.Rule,
			"account_action":    match.AccountAction,
			"downstream_action": match.DownstreamAction,
			"preview":           preview,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"matched":           false,
		"rule":              nil,
		"account_action":    upstreamrules.AccountActionBuiltin,
		"downstream_action": upstreamrules.DownstreamActionBuiltin,
		"preview":           upstreamrules.ResponsePreview{Status: req.Status, DownstreamAction: upstreamrules.DownstreamActionBuiltin},
	})
}

func (s *Server) adminUpstreamErrorRuleModelOptions(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	options, err := s.buildUpstreamErrorRuleModelOptions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, options)
}

func defaultUpstreamErrorRule() storage.UpstreamErrorRule {
	return storage.UpstreamErrorRule{
		Priority:         100,
		MatchMode:        upstreamrules.MatchAny,
		AccountAction:    upstreamrules.AccountActionBuiltin,
		DownstreamAction: upstreamrules.DownstreamActionBuiltin,
		IdlePingSeconds:  15,
	}
}

func upstreamErrorRuleID(req map[string]json.RawMessage) string {
	var id string
	if raw, ok := req["id"]; ok {
		_ = json.Unmarshal(raw, &id)
	}
	return slugify(id)
}

func overlayUpstreamErrorRule(rule *storage.UpstreamErrorRule, req map[string]json.RawMessage) error {
	for key, raw := range req {
		switch key {
		case "id":
			// ID is immutable after create and handled before overlay on POST.
		case "name":
			if err := json.Unmarshal(raw, &rule.Name); err != nil {
				return err
			}
		case "enabled":
			if err := json.Unmarshal(raw, &rule.Enabled); err != nil {
				return err
			}
		case "priority":
			if err := json.Unmarshal(raw, &rule.Priority); err != nil {
				return err
			}
		case "providers":
			if err := json.Unmarshal(raw, &rule.Providers); err != nil {
				return err
			}
		case "entrypoints":
			if err := json.Unmarshal(raw, &rule.Entrypoints); err != nil {
				return err
			}
		case "model_patterns":
			if err := json.Unmarshal(raw, &rule.ModelPatterns); err != nil {
				return err
			}
		case "status_codes":
			if err := json.Unmarshal(raw, &rule.StatusCodes); err != nil {
				return err
			}
		case "body_keywords":
			if err := json.Unmarshal(raw, &rule.BodyKeywords); err != nil {
				return err
			}
		case "match_mode":
			if err := json.Unmarshal(raw, &rule.MatchMode); err != nil {
				return err
			}
		case "account_action":
			if err := json.Unmarshal(raw, &rule.AccountAction); err != nil {
				return err
			}
		case "downstream_action":
			if err := json.Unmarshal(raw, &rule.DownstreamAction); err != nil {
				return err
			}
		case "response_status":
			if err := json.Unmarshal(raw, &rule.ResponseStatus); err != nil {
				return err
			}
		case "custom_message":
			if err := json.Unmarshal(raw, &rule.CustomMessage); err != nil {
				return err
			}
		case "cooldown_seconds":
			if err := json.Unmarshal(raw, &rule.CooldownSeconds); err != nil {
				return err
			}
		case "prefer_retry_after":
			if err := json.Unmarshal(raw, &rule.PreferRetryAfter); err != nil {
				return err
			}
		case "idle_seconds":
			if err := json.Unmarshal(raw, &rule.IdleSeconds); err != nil {
				return err
			}
		case "idle_ping_seconds":
			if err := json.Unmarshal(raw, &rule.IdlePingSeconds); err != nil {
				return err
			}
		case "skip_log":
			if err := json.Unmarshal(raw, &rule.SkipLog); err != nil {
				return err
			}
		case "description":
			if err := json.Unmarshal(raw, &rule.Description); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateUpstreamErrorRule(rule *storage.UpstreamErrorRule) error {
	rule.Name = strings.TrimSpace(rule.Name)
	if rule.Name == "" {
		rule.Name = rule.ID
	}
	rule.MatchMode = strings.ToLower(strings.TrimSpace(rule.MatchMode))
	if rule.MatchMode == "" {
		rule.MatchMode = upstreamrules.MatchAny
	}
	if rule.MatchMode != upstreamrules.MatchAny && rule.MatchMode != upstreamrules.MatchAll {
		return errors.New("match_mode must be any or all")
	}
	rule.AccountAction = normalizeRuleAccountAction(rule.AccountAction)
	rule.DownstreamAction = normalizeRuleDownstreamAction(rule.DownstreamAction)
	if rule.ResponseStatus < 0 || rule.ResponseStatus > 599 {
		return errors.New("response_status must be between 0 and 599")
	}
	if rule.CooldownSeconds < 0 || rule.IdlePingSeconds < 0 || rule.IdleSeconds < -1 {
		return errors.New("duration fields cannot be negative, except idle_seconds=-1")
	}
	if rule.IdlePingSeconds == 0 {
		rule.IdlePingSeconds = 15
	}
	return nil
}

func normalizeRuleAccountAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case upstreamrules.AccountActionNone, upstreamrules.AccountActionCooldown, upstreamrules.AccountActionCooldownRecheck, upstreamrules.AccountActionQuarantine:
		return strings.ToLower(strings.TrimSpace(action))
	default:
		return upstreamrules.AccountActionBuiltin
	}
}

func normalizeRuleDownstreamAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case upstreamrules.DownstreamActionFailover, upstreamrules.DownstreamActionPass, upstreamrules.DownstreamActionCustomError, upstreamrules.DownstreamActionNeutralize, upstreamrules.DownstreamActionIdleStream:
		return strings.ToLower(strings.TrimSpace(action))
	default:
		return upstreamrules.DownstreamActionBuiltin
	}
}

type upstreamRuleModelOptions struct {
	Providers []upstreamRuleProviderOption `json:"providers"`
}

type upstreamRuleProviderOption struct {
	ID       string                     `json:"id"`
	Label    string                     `json:"label"`
	Families []upstreamRuleFamilyOption `json:"families"`
}

type upstreamRuleFamilyOption struct {
	ID     string                    `json:"id"`
	Label  string                    `json:"label"`
	Models []upstreamRuleModelOption `json:"models"`
}

type upstreamRuleModelOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func (s *Server) buildUpstreamErrorRuleModelOptions(ctx context.Context) (upstreamRuleModelOptions, error) {
	builders := map[string]*modelProviderBuilder{}
	order := []string{"chatgpt", "claude"}
	ensure := func(id, label string) *modelProviderBuilder {
		if b := builders[id]; b != nil {
			return b
		}
		b := &modelProviderBuilder{id: id, label: label, families: map[string]*modelFamilyBuilder{}, familyOrder: []string{}}
		builders[id] = b
		if !containsString(order, id) {
			order = append(order, id)
		}
		return b
	}
	chatgpt := ensure("chatgpt", "ChatGPT / Codex")
	chatgpt.add("gpt-5", "GPT-5 系列", "gpt-5", "GPT-5")
	chatgpt.add("gpt-5", "GPT-5 系列", "gpt-5.6-sol", "GPT-5.6 Sol")
	chatgpt.add("gpt-5", "GPT-5 系列", "gpt-5.6-terra", "GPT-5.6 Terra")
	chatgpt.add("gpt-5", "GPT-5 系列", "gpt-5.6-luna", "GPT-5.6 Luna")
	chatgpt.add("gpt-5", "GPT-5 系列", "gpt-5.5", "GPT-5.5")
	chatgpt.add("gpt-4", "GPT-4 系列", "gpt-4.1", "GPT-4.1")
	chatgpt.add("gpt-4", "GPT-4 系列", "gpt-4o", "GPT-4o")
	claude := ensure("claude", "Claude")
	claude.add("claude-sonnet", "Claude Sonnet", "claude-sonnet-4.5", "Claude Sonnet 4.5")
	claude.add("claude-opus", "Claude Opus", "claude-opus-4.1", "Claude Opus 4.1")
	claude.add("claude-haiku", "Claude Haiku", "claude-haiku-4.5", "Claude Haiku 4.5")

	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return upstreamRuleModelOptions{}, err
	}
	accountProvider := map[string]string{}
	for _, acc := range accounts {
		provider := strings.TrimSpace(acc.Provider)
		if provider == "" {
			provider = "chatgpt"
		}
		accountProvider[acc.ID] = provider
	}
	caps, err := s.store.ListCapabilities(ctx, "")
	if err != nil {
		return upstreamRuleModelOptions{}, err
	}
	for _, cap := range caps {
		model := strings.TrimSpace(cap.ModelSlug)
		if model == "" {
			continue
		}
		provider := accountProvider[cap.AccountID]
		if provider == "" {
			provider = inferProviderForModel(model)
		}
		label := provider
		if provider == "chatgpt" {
			label = "ChatGPT / Codex"
		} else if provider == "claude" {
			label = "Claude"
		}
		ensure(provider, label).add(familyIDForModel(provider, model), familyLabelForModel(provider, model), model, displayModelLabel(model))
	}
	custom, err := s.store.ListCustomProviders(ctx)
	if err != nil {
		return upstreamRuleModelOptions{}, err
	}
	for _, p := range custom {
		label := firstNonEmpty(p.Name, p.ID)
		b := ensure(p.ID, label)
		for _, model := range p.Models {
			b.add(familyIDForModel(p.ID, model), "自定义模型", model, displayModelLabel(model))
		}
	}

	out := upstreamRuleModelOptions{Providers: make([]upstreamRuleProviderOption, 0, len(order))}
	for _, id := range order {
		if b := builders[id]; b != nil {
			out.Providers = append(out.Providers, b.option())
		}
	}
	return out, nil
}

type modelProviderBuilder struct {
	id, label   string
	families    map[string]*modelFamilyBuilder
	familyOrder []string
}
type modelFamilyBuilder struct {
	id, label string
	models    map[string]upstreamRuleModelOption
	order     []string
}

func (b *modelProviderBuilder) add(familyID, familyLabel, modelID, modelLabel string) {
	if strings.TrimSpace(modelID) == "" {
		return
	}
	familyID = strings.TrimSpace(familyID)
	if familyID == "" {
		familyID = "models"
	}
	f := b.families[familyID]
	if f == nil {
		f = &modelFamilyBuilder{id: familyID, label: familyLabel, models: map[string]upstreamRuleModelOption{}}
		b.families[familyID] = f
		b.familyOrder = append(b.familyOrder, familyID)
	}
	if _, ok := f.models[modelID]; ok {
		return
	}
	f.models[modelID] = upstreamRuleModelOption{ID: modelID, Label: modelLabel}
	f.order = append(f.order, modelID)
}
func (b *modelProviderBuilder) option() upstreamRuleProviderOption {
	opt := upstreamRuleProviderOption{ID: b.id, Label: b.label, Families: make([]upstreamRuleFamilyOption, 0, len(b.familyOrder))}
	for _, id := range b.familyOrder {
		f := b.families[id]
		opt.Families = append(opt.Families, f.option())
	}
	return opt
}
func (f *modelFamilyBuilder) option() upstreamRuleFamilyOption {
	sort.Strings(f.order)
	opt := upstreamRuleFamilyOption{ID: f.id, Label: f.label, Models: make([]upstreamRuleModelOption, 0, len(f.order))}
	for _, id := range f.order {
		opt.Models = append(opt.Models, f.models[id])
	}
	return opt
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func inferProviderForModel(model string) string {
	if isClaudeModel(model) {
		return "claude"
	}
	return "chatgpt"
}
func familyIDForModel(provider, model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "gpt-5"):
		return "gpt-5"
	case strings.HasPrefix(m, "gpt-4"):
		return "gpt-4"
	case strings.Contains(m, "sonnet"):
		return "claude-sonnet"
	case strings.Contains(m, "opus"):
		return "claude-opus"
	case strings.Contains(m, "haiku"):
		return "claude-haiku"
	default:
		if provider != "" && provider != "chatgpt" && provider != "claude" {
			return provider + "-models"
		}
		return "models"
	}
}
func familyLabelForModel(provider, model string) string {
	switch familyIDForModel(provider, model) {
	case "gpt-5":
		return "GPT-5 系列"
	case "gpt-4":
		return "GPT-4 系列"
	case "claude-sonnet":
		return "Claude Sonnet"
	case "claude-opus":
		return "Claude Opus"
	case "claude-haiku":
		return "Claude Haiku"
	default:
		if provider != "chatgpt" && provider != "claude" {
			return "自定义模型"
		}
		return "其他模型"
	}
}
func displayModelLabel(model string) string { return strings.TrimSpace(model) }
