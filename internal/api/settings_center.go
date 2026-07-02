// settings_center.go — unified settings aggregation API behind the SettingsV2 page.
// Combines the config registry (config_fields.go), node registrar credentials,
// automation policies, lifecycle defaults, and logging/memory knobs into one
// GET/POST surface so the administrator never needs to visit separate pages.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// settingsCenterView is the aggregate returned by GET /admin/settings-center.
type settingsCenterView struct {
	Config     []map[string]interface{} `json:"config,omitempty"`
	Registrar  map[string]interface{}   `json:"registrar,omitempty"`
	Automation map[string]interface{}   `json:"automation,omitempty"`
	Lifecycle  map[string]interface{}   `json:"lifecycle,omitempty"`
	Logging    map[string]interface{}   `json:"logging,omitempty"`
	Memory     map[string]interface{}   `json:"memory,omitempty"`
}

// settingsCenterPatch is the request body for POST /admin/settings-center.
type settingsCenterPatch struct {
	Section string                 `json:"section"`
	Key     string                 `json:"key"`
	Value   interface{}            `json:"value"`
	Values  map[string]interface{} `json:"values"` // bulk patch within a section
	Mode    string                 `json:"mode,omitempty"`
}

// settingsCenterSaveResp is the response for POST /admin/settings-center.
type settingsCenterSaveResp struct {
	Saved []settingsCenterDiff `json:"saved"`
}

type settingsCenterDiff struct {
	Section  string      `json:"section"`
	Key      string      `json:"key"`
	OldValue interface{} `json:"old_value"`
	NewValue interface{} `json:"new_value"`
}

type settingsCenterWritePlan struct {
	settings         map[string]string
	diffs            []settingsCenterDiff
	changedUpstream  bool
	changedScheduler bool
}

func newSettingsCenterWritePlan() *settingsCenterWritePlan {
	return &settingsCenterWritePlan{settings: map[string]string{}}
}

func (p *settingsCenterWritePlan) setSetting(key, value string) {
	p.settings[key] = value
}

func (p *settingsCenterWritePlan) appendDiffs(diffs ...settingsCenterDiff) {
	p.diffs = append(p.diffs, diffs...)
}

// handleSettingsCenter GET returns the full aggregate; POST applies a patch.
func (s *Server) handleSettingsCenter(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.settingsCenterGet(w, r)
	case http.MethodPost:
		s.settingsCenterPost(w, r)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) settingsCenterGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sections, err := settingsCenterSections(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	view := s.settingsCenterView(ctx, sections)
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) settingsCenterView(ctx context.Context, sections map[string]bool) settingsCenterView {
	view := settingsCenterView{}
	if settingsCenterWants(sections, "config") {
		view.Config = s.settingsViewJSON(ctx)
	}
	if settingsCenterWants(sections, "registrar") {
		view.Registrar = s.settingsCenterRegistrar(ctx)
	}
	if settingsCenterWants(sections, "automation") {
		view.Automation = s.settingsCenterAutomation(ctx)
	}
	if settingsCenterWants(sections, "lifecycle") {
		view.Lifecycle = s.settingsCenterLifecycle(ctx)
	}
	if settingsCenterWants(sections, "logging") {
		view.Logging = s.settingsCenterLogging(ctx)
	}
	if settingsCenterWants(sections, "memory") {
		view.Memory = s.settingsCenterMemory(ctx)
	}
	return view
}

func settingsCenterWants(sections map[string]bool, name string) bool {
	return sections == nil || sections[name]
}

func settingsCenterSections(query map[string][]string) (map[string]bool, error) {
	raw := append([]string{}, query["section"]...)
	raw = append(raw, query["sections"]...)
	if len(raw) == 0 {
		return nil, nil
	}
	sections := map[string]bool{}
	for _, item := range raw {
		for _, part := range strings.Split(item, ",") {
			name := strings.TrimSpace(part)
			if name == "" {
				continue
			}
			switch name {
			case "config", "registrar", "automation", "lifecycle", "logging", "memory":
				sections[name] = true
			default:
				return nil, fmt.Errorf("unknown settings section %q", name)
			}
		}
	}
	if len(sections) == 0 {
		return nil, nil
	}
	return sections, nil
}

func (s *Server) settingsCenterPost(w http.ResponseWriter, r *http.Request) {
	raw, err := readJSONRequestBody(r.Body, adminJSONBodyLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var patches []settingsCenterPatch
	if err := json.Unmarshal(raw, &patches); err != nil {
		// Fall back to a single patch object.
		var single settingsCenterPatch
		if err2 := json.Unmarshal(raw, &single); err2 != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("body must be a JSON array of patches or a single patch: %w", err))
			return
		}
		patches = []settingsCenterPatch{single}
	}

	ctx := r.Context()

	for _, p := range patches {
		if err := s.validateSettingsCenterPatch(ctx, p); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}

	plan, err := s.planSettingsCenterPatches(ctx, patches)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.SetSettings(ctx, plan.settings); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if plan.changedUpstream {
		s.upstream.UpdateConfig(s.effectiveUpstreamConfig(ctx))
	}
	if plan.changedScheduler && s.scheduler != nil {
		s.scheduler.UpdateConfig(s.effectiveSchedulerConfig(ctx))
	}

	writeJSON(w, http.StatusOK, settingsCenterSaveResp{Saved: plan.diffs})
}

func settingsCenterPatchBody(p settingsCenterPatch) map[string]interface{} {
	if p.Values != nil {
		return p.Values
	}
	return map[string]interface{}{p.Key: p.Value}
}

func (s *Server) validateSettingsCenterPatch(ctx context.Context, p settingsCenterPatch) error {
	switch p.Section {
	case "config":
		return s.validateConfigPatch(p)
	case "registrar":
		return s.validateRegistrarPatch(p)
	case "automation":
		return s.validateAutomationPatch(ctx, p)
	case "lifecycle":
		return s.validateLifecyclePatch(p)
	case "logging":
		return validateRuntimeSettingsPatch(p, loggingSettingSpecs, "logging")
	case "memory":
		return validateRuntimeSettingsPatch(p, memorySettingSpecs, "memory")
	default:
		return fmt.Errorf("unknown settings section %q", p.Section)
	}
}

type settingsCenterPlanState struct {
	registrarLoaded bool
	registrarCfg    map[string]interface{}
	lifecycleLoaded bool
	lifecycleDefs   map[string]interface{}
	policiesLoaded  bool
	policies        map[string]*Policy
	loggingLoaded   bool
	logging         map[string]interface{}
	memoryLoaded    bool
	memory          map[string]interface{}
}

func (s *Server) planSettingsCenterPatches(ctx context.Context, patches []settingsCenterPatch) (*settingsCenterWritePlan, error) {
	plan := newSettingsCenterWritePlan()
	state := settingsCenterPlanState{}
	for _, p := range patches {
		var err error
		switch p.Section {
		case "config":
			err = s.planConfigPatch(ctx, plan, p)
		case "registrar":
			err = s.planRegistrarPatch(ctx, plan, &state, p)
		case "automation":
			err = s.planAutomationPatch(ctx, plan, &state, p)
		case "lifecycle":
			err = s.planLifecyclePatch(ctx, plan, &state, p)
		case "logging":
			err = s.planRuntimeSettingsPatch(ctx, plan, &state, p, loggingSettingSpecs, "logging")
		case "memory":
			err = s.planRuntimeSettingsPatch(ctx, plan, &state, p, memorySettingSpecs, "memory")
		default:
			err = fmt.Errorf("unknown settings section %q", p.Section)
		}
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}

func cloneSettingsMap(src map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func policyDiffValue(p *Policy) interface{} {
	if p == nil {
		return nil
	}
	return map[string]interface{}{"enabled": p.Enabled, "config": cloneSettingsMap(p.Config)}
}

func setRegistrationDefaultSettings(plan *settingsCenterWritePlan, defaults map[string]interface{}) {
	for field, key := range regDefaultKeys {
		if v, ok := defaults[field]; ok {
			plan.setSetting(key, cfgString(v))
		}
	}
}

func overlayPlannedRegistrationDefaults(plan *settingsCenterWritePlan, defaults map[string]interface{}) {
	for field, key := range regDefaultKeys {
		if v, ok := plan.settings[key]; ok {
			defaults[field] = v
		}
	}
}

func (s *Server) planConfigPatch(ctx context.Context, plan *settingsCenterWritePlan, p settingsCenterPatch) error {
	body := settingsCenterPatchBody(p)
	if len(body) == 0 {
		return nil
	}
	for k, v := range body {
		f, ok := configFieldByKey(k)
		if !ok {
			return fmt.Errorf("unknown config key %q", k)
		}
		if f.Effect == effectRestart {
			return fmt.Errorf("config key %q requires restart and is read-only at runtime", k)
		}
		oldValue := s.configFieldValue(ctx, f)
		if stored, ok := plan.settings[k]; ok {
			oldValue = configFieldCanonicalValue(f, stored)
		}
		stored, err := validateSettingValue(f, v)
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		plan.setSetting(k, stored)
		if f.Effect == effectUpstream {
			plan.changedUpstream = true
		}
		if f.Effect == effectScheduler {
			plan.changedScheduler = true
		}
		plan.appendDiffs(settingsCenterDiff{
			Section:  "config",
			Key:      k,
			OldValue: oldValue,
			NewValue: configFieldCanonicalValue(f, stored),
		})
	}
	return nil
}

func (s *Server) planRegistrarPatch(ctx context.Context, plan *settingsCenterWritePlan, state *settingsCenterPlanState, p settingsCenterPatch) error {
	if !state.registrarLoaded {
		cfg, err := s.loadNodeRegistrarConfig(ctx)
		if err != nil {
			return err
		}
		state.registrarCfg = cloneSettingsMap(cfg)
		state.registrarLoaded = true
	}
	body := settingsCenterPatchBody(p)
	if len(body) == 0 {
		return nil
	}
	if rawDefaults, ok := body["defaults"]; ok {
		if s.regHandler == nil {
			return fmt.Errorf("registration subsystem is not initialized")
		}
		defaults, ok := rawDefaults.(map[string]interface{})
		if !ok {
			return fmt.Errorf("registrar defaults must be an object")
		}
		normalized, err := normalizeRegDefaults(defaults)
		if err != nil {
			return err
		}
		setRegistrationDefaultSettings(plan, normalized)
		if state.lifecycleLoaded {
			overlayPlannedRegistrationDefaults(plan, state.lifecycleDefs)
		}
	}
	configBody := make(map[string]interface{}, len(body))
	for k, v := range body {
		if k != "defaults" {
			configBody[k] = v
		}
	}
	if len(configBody) == 0 && p.Mode != "replace" {
		return nil
	}
	old := cloneSettingsMap(state.registrarCfg)
	if p.Mode == "replace" {
		state.registrarCfg = map[string]interface{}{}
	} else if p.Mode != "" && p.Mode != "merge" {
		return fmt.Errorf("unknown registrar patch mode %q", p.Mode)
	}
	for k, v := range configBody {
		if isRegistrarMetaKey(k) {
			return fmt.Errorf("registrar key %q is read-only metadata", k)
		}
		if shouldClearRegistrarValue(v) {
			delete(state.registrarCfg, k)
			continue
		}
		state.registrarCfg[k] = v
	}
	raw, err := json.Marshal(state.registrarCfg)
	if err != nil {
		return err
	}
	plan.setSetting("node_registrar_config", string(raw))
	plan.appendDiffs(registrarDiffs(old, state.registrarCfg)...)
	return nil
}

func (s *Server) planAutomationPatch(ctx context.Context, plan *settingsCenterWritePlan, state *settingsCenterPlanState, p settingsCenterPatch) error {
	if !state.policiesLoaded {
		policies, err := s.loadPoliciesWithError(ctx)
		if err != nil {
			return err
		}
		state.policies = policies
		state.policiesLoaded = true
	}
	body := settingsCenterPatchBody(p)
	if len(body) == 0 {
		return nil
	}
	if len(body) != 1 {
		return fmt.Errorf("automation patch accepts only policy updates")
	}
	pol, ok := body["policy"]
	if !ok {
		return fmt.Errorf("unknown automation key")
	}
	pmap, ok := pol.(map[string]interface{})
	if !ok {
		return fmt.Errorf("automation policy must be an object")
	}
	typ, _ := pmap["type"].(string)
	if !validAutomationPolicyType(typ) {
		return fmt.Errorf("invalid policy type: %s", typ)
	}
	enabled, ok := pmap["enabled"].(bool)
	if !ok {
		return fmt.Errorf("automation policy enabled must be boolean")
	}
	cfg, ok := pmap["config"].(map[string]interface{})
	if !ok && pmap["config"] != nil {
		return fmt.Errorf("automation policy config must be an object")
	}
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	oldValue := policyDiffValue(state.policies[typ])
	now := time.Now().Unix()
	np := state.policies[typ]
	if np == nil {
		np = &Policy{ID: typ, Type: typ, Created: now}
	}
	np.Enabled = enabled
	np.Config = cloneSettingsMap(cfg)
	np.Updated = now
	state.policies[typ] = np
	raw, err := json.Marshal(state.policies)
	if err != nil {
		return err
	}
	plan.setSetting(automationPoliciesKey, string(raw))
	plan.appendDiffs(settingsCenterDiff{
		Section:  "automation",
		Key:      "policy." + typ,
		OldValue: oldValue,
		NewValue: policyDiffValue(np),
	})
	return nil
}

func (s *Server) planLifecyclePatch(ctx context.Context, plan *settingsCenterWritePlan, state *settingsCenterPlanState, p settingsCenterPatch) error {
	if !state.lifecycleLoaded {
		if s.regHandler == nil {
			return fmt.Errorf("registration subsystem is not initialized")
		}
		defaults, err := s.regHandler.getDefaultsWithError(ctx)
		if err != nil {
			return err
		}
		state.lifecycleDefs = make(map[string]interface{}, len(defaults))
		for k, v := range defaults {
			state.lifecycleDefs[k] = v
		}
		overlayPlannedRegistrationDefaults(plan, state.lifecycleDefs)
		state.lifecycleLoaded = true
	}
	body := settingsCenterPatchBody(p)
	if len(body) == 0 {
		return nil
	}
	if len(body) != 1 {
		return fmt.Errorf("lifecycle patch accepts only defaults updates")
	}
	rawDefaults, ok := body["defaults"]
	if !ok {
		return fmt.Errorf("unknown lifecycle key")
	}
	defaults, ok := rawDefaults.(map[string]interface{})
	if !ok {
		return fmt.Errorf("lifecycle defaults must be an object")
	}
	normalized, err := normalizeRegDefaults(defaults)
	if err != nil {
		return err
	}
	setRegistrationDefaultSettings(plan, normalized)
	for k, v := range normalized {
		plan.appendDiffs(settingsCenterDiff{Section: "lifecycle", Key: k, OldValue: state.lifecycleDefs[k], NewValue: v})
		state.lifecycleDefs[k] = v
	}
	return nil
}

func (s *Server) planRuntimeSettingsPatch(ctx context.Context, plan *settingsCenterWritePlan, state *settingsCenterPlanState, p settingsCenterPatch, specs map[string]settingsSettingSpec, section string) error {
	body := settingsCenterPatchBody(p)
	if len(body) == 0 {
		return nil
	}
	old := map[string]interface{}{}
	switch section {
	case "logging":
		if !state.loggingLoaded {
			state.logging = s.settingsCenterLogging(ctx)
			state.loggingLoaded = true
		}
		old = state.logging
	case "memory":
		if !state.memoryLoaded {
			state.memory = s.settingsCenterMemory(ctx)
			state.memoryLoaded = true
		}
		old = state.memory
	}
	for k, v := range body {
		spec, ok := specs[k]
		if !ok {
			return fmt.Errorf("unknown %s key %q", section, k)
		}
		stored, normalized, err := spec.normalize(v)
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		plan.setSetting(spec.storageKey, stored)
		plan.appendDiffs(settingsCenterDiff{Section: section, Key: k, OldValue: old[k], NewValue: normalized})
		old[k] = normalized
	}
	return nil
}

// ── config section ──────────────────────────────────────────────────────────

func (s *Server) validateConfigPatch(p settingsCenterPatch) error {
	body := settingsCenterPatchBody(p)
	if len(body) == 0 {
		return nil
	}
	for k, v := range body {
		f, ok := configFieldByKey(k)
		if !ok {
			return fmt.Errorf("unknown config key %q", k)
		}
		if f.Effect == effectRestart {
			return fmt.Errorf("config key %q requires restart and is read-only at runtime", k)
		}
		if _, err := validateSettingValue(f, v); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

// ── registrar section ───────────────────────────────────────────────────────

func (s *Server) settingsCenterRegistrar(ctx context.Context) map[string]interface{} {
	out, err := s.loadNodeRegistrarConfig(ctx)
	registrarError := ""
	if err != nil {
		out = map[string]interface{}{}
		registrarError = err.Error()
	}
	out["registrar_error"] = registrarError
	defaultsError := ""
	// Add defaults for known fields
	if s.regHandler != nil {
		defs, err := s.regHandler.getDefaultsWithError(ctx)
		if err != nil {
			defaultsError = err.Error()
		}
		regDefaults := map[string]interface{}{}
		for k, v := range defs {
			regDefaults[k] = v
		}
		out["defaults"] = regDefaults
	}
	out["defaults_error"] = defaultsError
	return out
}

func (s *Server) loadNodeRegistrarConfig(ctx context.Context) (map[string]interface{}, error) {
	cfg := map[string]interface{}{}
	v, ok, err := s.store.GetSetting(ctx, "node_registrar_config")
	if err != nil {
		return nil, err
	}
	if !ok {
		return cfg, nil
	}
	if v = strings.TrimSpace(v); v == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(v), &cfg); err != nil {
		return nil, fmt.Errorf("decode node_registrar_config: %w", err)
	}
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	return cfg, nil
}

func (s *Server) validateRegistrarPatch(p settingsCenterPatch) error {
	body := settingsCenterPatchBody(p)
	if len(body) == 0 {
		return nil
	}
	if p.Mode != "" && p.Mode != "merge" && p.Mode != "replace" {
		return fmt.Errorf("unknown registrar patch mode %q", p.Mode)
	}
	for k, v := range body {
		if k == "defaults" {
			if s.regHandler == nil {
				return fmt.Errorf("registration subsystem is not initialized")
			}
			d, ok := v.(map[string]interface{})
			if !ok {
				return fmt.Errorf("registrar defaults must be an object")
			}
			if _, err := normalizeRegDefaults(d); err != nil {
				return err
			}
			continue
		}
		if isRegistrarMetaKey(k) {
			return fmt.Errorf("registrar key %q is read-only metadata", k)
		}
	}
	return nil
}

func isRegistrarMetaKey(key string) bool {
	switch key {
	case "registrar_error", "defaults_error":
		return true
	default:
		return false
	}
}

func shouldClearRegistrarValue(v interface{}) bool {
	if v == nil {
		return true
	}
	if sv, ok := v.(string); ok && strings.TrimSpace(sv) == "" {
		return true
	}
	return false
}

func registrarDiffs(old, next map[string]interface{}) []settingsCenterDiff {
	keys := make(map[string]struct{}, len(old)+len(next))
	for k := range old {
		if k != "defaults" {
			keys[k] = struct{}{}
		}
	}
	for k := range next {
		keys[k] = struct{}{}
	}
	diffs := make([]settingsCenterDiff, 0, len(keys))
	for k := range keys {
		oldValue, oldOK := old[k]
		nextValue, nextOK := next[k]
		if oldOK == nextOK && reflect.DeepEqual(oldValue, nextValue) {
			continue
		}
		var newValue interface{}
		if nextOK {
			newValue = nextValue
		}
		diffs = append(diffs, settingsCenterDiff{Section: "registrar", Key: k, OldValue: oldValue, NewValue: newValue})
	}
	return diffs
}

// ── automation section ──────────────────────────────────────────────────────

func (s *Server) settingsCenterAutomation(ctx context.Context) map[string]interface{} {
	policies, policyErr := s.loadPoliciesWithError(ctx)
	list := make([]*Policy, 0, len(policies))
	for _, p := range policies {
		list = append(list, p)
	}
	stats, statsErr := s.automationStats(ctx)
	readiness := s.registrationReadiness(ctx)
	policyError := ""
	if policyErr != nil {
		policyError = policyErr.Error()
	}
	statsError := ""
	if statsErr != nil {
		statsError = statsErr.Error()
		stats = map[string]interface{}{}
	}
	return map[string]interface{}{
		"policies":     list,
		"policy_error": policyError,
		"stats_error":  statsError,
		"stats":        stats,
		"readiness":    readiness,
		"templates":    registrationTemplates(),
	}
}

func (s *Server) validateAutomationPatch(ctx context.Context, p settingsCenterPatch) error {
	body := settingsCenterPatchBody(p)
	if len(body) == 0 {
		return nil
	}
	if len(body) != 1 {
		return fmt.Errorf("automation patch accepts only policy updates")
	}
	pol, ok := body["policy"]
	if !ok {
		return fmt.Errorf("unknown automation key")
	}
	pmap, ok := pol.(map[string]interface{})
	if !ok {
		return fmt.Errorf("automation policy must be an object")
	}
	typ, _ := pmap["type"].(string)
	if !validAutomationPolicyType(typ) {
		return fmt.Errorf("invalid policy type: %s", typ)
	}
	if _, ok := pmap["enabled"].(bool); !ok {
		return fmt.Errorf("automation policy enabled must be boolean")
	}
	if _, ok := pmap["config"].(map[string]interface{}); !ok && pmap["config"] != nil {
		return fmt.Errorf("automation policy config must be an object")
	}
	if _, err := s.loadPoliciesWithError(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Server) automationStats(ctx context.Context) (map[string]interface{}, error) {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	alive, dead, cooling := 0, 0, 0
	for _, acc := range accounts {
		switch {
		case acc.QuarantineUntil > now:
			dead++
		case acc.Status == "active":
			alive++
		default:
			dead++
		}
	}
	accountIDs := make([]string, 0, len(accounts))
	for _, acc := range accounts {
		accountIDs = append(accountIDs, acc.ID)
	}
	bindings, err := s.store.ListEgressBindingsByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	for _, b := range bindings {
		if b.CooldownUntil > now {
			cooling++
		}
	}
	plusCount := 0
	for _, acc := range accounts {
		if acc.PlanType == "plus" || acc.PlanType == "chatgptplusplan" {
			plusCount++
		}
	}
	return map[string]interface{}{
		"pool_count": len(accounts),
		"health":     map[string]int{"alive": alive, "dead": dead, "cooling": cooling},
		"plus_count": plusCount,
	}, nil
}

// ── lifecycle section ───────────────────────────────────────────────────────

func (s *Server) settingsCenterLifecycle(ctx context.Context) map[string]interface{} {
	defs := map[string]interface{}{}
	defaultsError := ""
	if s.regHandler != nil {
		regDefaults, err := s.regHandler.getDefaultsWithError(ctx)
		if err != nil {
			defaultsError = err.Error()
		}
		for k, v := range regDefaults {
			defs[k] = v
		}
	}
	return map[string]interface{}{
		"defaults":       defs,
		"defaults_error": defaultsError,
	}
}

func (s *Server) validateLifecyclePatch(p settingsCenterPatch) error {
	body := settingsCenterPatchBody(p)
	if len(body) == 0 {
		return nil
	}
	if len(body) != 1 {
		return fmt.Errorf("lifecycle patch accepts only defaults updates")
	}
	rawDefaults, ok := body["defaults"]
	if !ok {
		return fmt.Errorf("unknown lifecycle key")
	}
	if s.regHandler == nil {
		return fmt.Errorf("registration subsystem is not initialized")
	}
	defaults, ok := rawDefaults.(map[string]interface{})
	if !ok {
		return fmt.Errorf("lifecycle defaults must be an object")
	}
	_, err := normalizeRegDefaults(defaults)
	return err
}

func normalizeRegDefaults(defaults map[string]interface{}) (map[string]interface{}, error) {
	normalized := make(map[string]interface{}, len(defaults))
	for k, v := range defaults {
		if _, ok := regDefaultKeys[k]; !ok {
			return nil, fmt.Errorf("unknown registration default key %q", k)
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s: expected string", k)
		}
		normalized[k] = strings.TrimSpace(s)
	}
	return normalized, nil
}

// ── logging section ─────────────────────────────────────────────────────────

func (s *Server) settingsCenterLogging(ctx context.Context) map[string]interface{} {
	return s.settingsCenterRuntimeSection(ctx, loggingSettingSpecs, map[string]interface{}{
		"verbose_logging":    true,
		"failure_threshold":  0.6,
		"log_retention_days": 7,
		"degraded":           false,
	})
}

// ── memory section ──────────────────────────────────────────────────────────

func (s *Server) settingsCenterMemory(ctx context.Context) map[string]interface{} {
	return s.settingsCenterRuntimeSection(ctx, memorySettingSpecs, map[string]interface{}{
		"lifecycle_batch_size":    200,
		"lifecycle_concurrency":   10,
		"go_memory_limit_mb":      0,
		"reg_combined_output_cap": 1 << 20,
	})
}

func validateRuntimeSettingsPatch(p settingsCenterPatch, specs map[string]settingsSettingSpec, section string) error {
	body := settingsCenterPatchBody(p)
	if len(body) == 0 {
		return nil
	}
	for k, v := range body {
		spec, ok := specs[k]
		if !ok {
			return fmt.Errorf("unknown %s key %q", section, k)
		}
		if _, _, err := spec.normalize(v); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

type settingsSettingSpec struct {
	storageKey string
	normalize  func(interface{}) (string, interface{}, error)
}

func (s *Server) settingsCenterRuntimeSection(ctx context.Context, specs map[string]settingsSettingSpec, defaults map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(defaults)+1)
	for k, v := range defaults {
		out[k] = v
	}
	errors := map[string]string{}
	for sectionKey, spec := range specs {
		raw, ok, err := s.store.GetSetting(ctx, spec.storageKey)
		if err != nil {
			errors[sectionKey] = fmt.Sprintf("read %s: %v", spec.storageKey, err)
			continue
		}
		if !ok {
			continue
		}
		_, normalized, err := spec.normalize(raw)
		if err != nil {
			errors[sectionKey] = fmt.Sprintf("%s=%q: %v", spec.storageKey, raw, err)
			continue
		}
		out[sectionKey] = normalized
	}
	out["settings_errors"] = errors
	return out
}

var loggingSettingSpecs = map[string]settingsSettingSpec{
	"verbose_logging":    {storageKey: "reg_verbose_logging", normalize: normalizeSettingsBool},
	"failure_threshold":  {storageKey: "reg_failure_threshold", normalize: normalizeSettingsFloatRange(0.1, 1.0)},
	"log_retention_days": {storageKey: "reg_log_retention_days", normalize: normalizeSettingsIntRange(1, 90)},
	"degraded":           {storageKey: "reg_degraded", normalize: normalizeSettingsBool},
}

var memorySettingSpecs = map[string]settingsSettingSpec{
	"lifecycle_batch_size":    {storageKey: "lifecycle_batch_size", normalize: normalizeSettingsIntRange(10, 1000)},
	"lifecycle_concurrency":   {storageKey: "lifecycle_concurrency", normalize: normalizeSettingsIntRange(1, 50)},
	"go_memory_limit_mb":      {storageKey: "go_memory_limit_mb", normalize: normalizeSettingsIntRange(0, 32768)},
	"reg_combined_output_cap": {storageKey: "reg_combined_output_cap", normalize: normalizeSettingsIntRange(65536, 10485760)},
}

func normalizeSettingsBool(v interface{}) (string, interface{}, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return "true", true, nil
		}
		return "false", false, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "on", "yes":
			return "true", true, nil
		case "0", "false", "off", "no", "":
			return "false", false, nil
		}
	}
	return "", nil, fmt.Errorf("expected boolean")
}

func normalizeSettingsIntRange(minValue, maxValue int) func(interface{}) (string, interface{}, error) {
	return func(v interface{}) (string, interface{}, error) {
		n, err := settingsIntValue(v)
		if err != nil {
			return "", nil, err
		}
		if n < minValue || n > maxValue {
			return "", nil, fmt.Errorf("must be between %d and %d", minValue, maxValue)
		}
		return strconv.Itoa(n), n, nil
	}
}

func normalizeSettingsFloatRange(minValue, maxValue float64) func(interface{}) (string, interface{}, error) {
	return func(v interface{}) (string, interface{}, error) {
		n, err := settingsFloatValue(v)
		if err != nil {
			return "", nil, err
		}
		if n < minValue || n > maxValue {
			return "", nil, fmt.Errorf("must be between %g and %g", minValue, maxValue)
		}
		return strconv.FormatFloat(n, 'f', -1, 64), n, nil
	}
}

func settingsIntValue(v interface{}) (int, error) {
	switch t := v.(type) {
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case float64:
		n := int(t)
		if float64(n) != t {
			return 0, fmt.Errorf("expected integer")
		}
		return n, nil
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, fmt.Errorf("expected integer")
		}
		return int(n), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("expected integer")
		}
		return n, nil
	default:
		return 0, fmt.Errorf("expected integer")
	}
}

func settingsFloatValue(v interface{}) (float64, error) {
	switch t := v.(type) {
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case float64:
		return t, nil
	case json.Number:
		n, err := t.Float64()
		if err != nil {
			return 0, fmt.Errorf("expected number")
		}
		return n, nil
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, fmt.Errorf("expected number")
		}
		return n, nil
	default:
		return 0, fmt.Errorf("expected number")
	}
}

// ── templates ───────────────────────────────────────────────────────────────

// registrationTemplates returns the built-in registration templates that the
// admin can apply with one click. Each template pre-fills all fields except the
// credential fields that must be operator-supplied (hero-sms API key, proxy
// username/password, mailbox credentials).
func registrationTemplates() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"id":            "email-only",
			"name":          "仅邮箱注册 (ChatGPT)",
			"description":   "使用邮箱 OTP 注册 ChatGPT 账号，无需住宅代理。",
			"platform":      "chatgpt",
			"method":        "node",
			"identity_mode": "email",
			"group":         "",
			"egress":        "egress_direct",
			"mail_provider": "1secmail",
			"needs":         []string{"mailProvider", "mailDomains"},
		},
		{
			"id":            "phone-only",
			"name":          "仅手机注册 (ChatGPT + 住宅代理)",
			"description":   "使用 hero-sms 手机号 + 住宅代理注册 ChatGPT。需填写 hero-sms API Key 和代理凭据。",
			"platform":      "chatgpt",
			"method":        "node",
			"identity_mode": "sms",
			"group":         "",
			"egress":        "",
			"sms_provider":  "herosms",
			"needs":         []string{"heroSmsApiKey", "proxyHost", "proxyPort", "proxyUsername", "proxyPassword"},
		},
		{
			"id":            "full",
			"name":          "邮箱+手机完整注册 (ChatGPT)",
			"description":   "同时启用邮箱和手机注册，先邮箱后手机作为备选。",
			"platform":      "chatgpt",
			"method":        "node",
			"identity_mode": "email",
			"group":         "",
			"egress":        "",
			"mail_provider": "1secmail",
			"sms_provider":  "herosms",
			"needs":         []string{"heroSmsApiKey", "proxyHost", "proxyPort", "proxyUsername", "proxyPassword", "mailProvider", "mailDomains"},
		},
		{
			"id":            "claude",
			"name":          "Claude 注册",
			"description":   "注册 Claude 账号（邮箱验证）。",
			"platform":      "claude",
			"method":        "node",
			"identity_mode": "email",
			"group":         "",
			"egress":        "",
			"mail_provider": "cloudflare",
			"needs":         []string{"mailProvider", "mailDomains"},
		},
	}
}

func optimalSystemTemplateValues() map[string]interface{} {
	return map[string]interface{}{
		"conversation_isolation":                  true,
		"codex_prefer_sidecar_ja3_over_ws":        true,
		"codex_prompt_cache_retention":            "24h",
		"rate_limit_guard_enabled":                true,
		"seamless_failover":                       true,
		"failover_max_attempts":                   float64(3),
		"force_failover_on_429":                   false,
		"leak_scrub":                              true,
		"token_save_enabled":                      false,
		"claude_cache_control_inject":             true,
		"claude_native_cache_breakpoint_inject":   true,
		"claude_cache_ttl":                        "1h",
		"claude_gateway_unknown_target_policy":    "block",
		"claude_gateway_disable_nonessential_env": true,
		"claude_gateway_strict_linux_default":     true,
		"codex_install_model":                     "gpt-5.5",
		"codex_install_effort":                    "xhigh",
		"codex_install_approval_policy":           "never",
		"codex_install_sandbox_mode":              "danger-full-access",
	}
}

func systemConfigTemplates() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"id":          "optimal-codex-pool",
			"name":        "推荐默认系统配置",
			"description": "缓存命中、无缝换号、泄漏擦除和安装默认值的保守最优组合；不启用会改写请求内容的 token 压缩。",
			"section":     "config",
			"values":      optimalSystemTemplateValues(),
		},
	}
}

// handleSettingsCenterTemplate handles POST /admin/settings-center/apply-template.
func (s *Server) handleSettingsCenterTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		TemplateID string `json:"template_id"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	for _, t := range systemConfigTemplates() {
		if t["id"] == req.TemplateID {
			values, _ := t["values"].(map[string]interface{})
			plan, err := s.planSettingsCenterPatches(r.Context(), []settingsCenterPatch{{Section: "config", Values: values}})
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			if err := s.store.SetSettings(r.Context(), plan.settings); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			if plan.changedUpstream {
				s.upstream.UpdateConfig(s.effectiveUpstreamConfig(r.Context()))
			}
			if plan.changedScheduler && s.scheduler != nil {
				s.scheduler.UpdateConfig(s.effectiveSchedulerConfig(r.Context()))
			}
			out := cloneSettingsMap(t)
			out["saved"] = plan.diffs
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	for _, t := range registrationTemplates() {
		if t["id"] == req.TemplateID {
			writeJSON(w, http.StatusOK, t)
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("template %q not found", req.TemplateID))
}

// settingFloat reads a float setting from the DB with a default.
func (s *Server) settingFloat(ctx context.Context, key string, def float64) float64 {
	if v, ok := s.runtimeSetting(ctx, key); ok {
		trimmed := strings.TrimSpace(v)
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return f
		} else {
			logInvalidRuntimeSetting(key, trimmed, "float")
		}
	}
	return def
}
