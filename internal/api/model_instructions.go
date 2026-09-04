package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/superinstruct"
	"codex-account-pool/internal/upstream"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type modelInstructionFileView struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	UpdatedAt int64  `json:"updated_at"`
	Error     string `json:"error,omitempty"`
}

// legacySuperInstructBundleHeader is the stable marker emitted by every
// pre-profile and current Super-Instruct skill bundle.  It lets a request whose
// group/client gate is now closed remove only gateway-owned M1 carriers that may
// have survived in an old Codex config, CPA recovery body, or Messages goal
// checkpoint.  Ordinary client prompts are otherwise left byte-for-byte alone.
const legacySuperInstructBundleHeader = "# Super-Instruct Codex 5.6"

func (s *Server) adminModelInstructions(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		files, err := s.listModelInstructionFiles()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, files)
	case http.MethodPost:
		var req struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		name, err := normalizeModelInstructionFileName(req.Name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := os.MkdirAll(s.modelInstructionsDir(), 0o700); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		path := filepath.Join(s.modelInstructionsDir(), name)
		if err := os.WriteFile(path, []byte(req.Content), 0o600); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		info, _ := os.Stat(path)
		view := modelInstructionFileView{Name: name}
		if info != nil {
			view.Size = info.Size()
			view.UpdatedAt = info.ModTime().Unix()
		}
		writeJSON(w, http.StatusOK, view)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) listModelInstructionFiles() ([]modelInstructionFileView, error) {
	dir := s.modelInstructionsDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []modelInstructionFileView{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]modelInstructionFileView, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if _, err := normalizeModelInstructionFileName(name); err != nil {
			continue
		}
		info, err := entry.Info()
		view := modelInstructionFileView{Name: name}
		if err != nil {
			view.Error = err.Error()
		} else {
			view.Size = info.Size()
			view.UpdatedAt = info.ModTime().Unix()
		}
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) modelInstructionsDir() string {
	db := strings.TrimSpace(s.cfg.DatabasePath)
	if db == "" || db == ":memory:" {
		db = "."
	}
	return filepath.Join(filepath.Dir(db), "model-instructions")
}

func normalizeModelInstructionFileNames(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		name, err := normalizeModelInstructionFileName(value)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

func normalizeModelInstructionFileName(value string) (string, error) {
	name := strings.TrimSpace(value)
	if name == "" {
		return "", errors.New("model instructions file name required")
	}
	if strings.Contains(name, "/") || strings.Contains(name, `\`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid model instructions file name %q", value)
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".md" && ext != ".txt" {
		return "", fmt.Errorf("model instructions file %q must end with .md or .txt", name)
	}
	return name, nil
}

type requestUserGroupPolicyContextKey struct{}

func userGroupPolicyAsAccountGroup(group storage.UserGroup) storage.Group {
	profiles := make(storage.ModelInstructionProfiles, len(group.ModelInstructionProfiles))
	for family, profile := range group.ModelInstructionProfiles {
		profile.Files = append([]string(nil), profile.Files...)
		profiles[family] = profile
	}
	return storage.Group{
		Name:                                group.ID,
		SystemPrompt:                        group.SystemPrompt,
		PromptMode:                          group.PromptMode,
		SystemPromptApplyToCompaction:       group.SystemPromptApplyToCompaction,
		ModelInstructionsEnabled:            group.ModelInstructionsEnabled,
		ModelInstructionsFiles:              append([]string(nil), group.ModelInstructionsFiles...),
		ModelInstructionProfiles:            profiles,
		SuperInstructEnabled:                group.SuperInstructEnabled,
		SuperInstructSkillIDs:               append([]string(nil), group.SuperInstructSkillIDs...),
		SuperInstructProfiles:               cloneSuperInstructProfiles(group.SuperInstructProfiles),
		SuperInstructResponseRewriteEnabled: group.SuperInstructResponseRewriteEnabled,
		SuperInstructMemoryEnabled:          group.SuperInstructMemoryEnabled,
		SuperInstructMonitorEnabled:         group.SuperInstructMonitorEnabled,
		ForceModel:                          group.ForceModel,
		ForceEffort:                         group.ForceEffort,
		EgressRPMBalanceEnabled:             group.EgressRPMBalanceEnabled,
		EgressRPMBalanceThreshold:           group.EgressRPMBalanceThreshold,
		EgressRPMBalanceEgressIDs:           append([]string(nil), group.EgressRPMBalanceEgressIDs...),
	}
}

func cloneSuperInstructProfiles(profiles storage.SuperInstructProfiles) storage.SuperInstructProfiles {
	if len(profiles) == 0 {
		return nil
	}
	out := make(storage.SuperInstructProfiles, len(profiles))
	for family, profile := range profiles {
		profile.SkillIDs = append([]string(nil), profile.SkillIDs...)
		out[family] = profile
	}
	return out
}

func modelInstructionFamily(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "claude-"), model == "claude", model == "opus", model == "sonnet", model == "haiku", model == "fable":
		return storage.ModelInstructionFamilyClaude
	case strings.HasPrefix(model, "gemini-"), model == "gemini":
		return storage.ModelInstructionFamilyGemini
	case strings.HasPrefix(model, "gpt-"), strings.HasPrefix(model, "chatgpt-"), strings.HasPrefix(model, "codex-"),
		model == "gpt", model == "chatgpt", model == "codex",
		strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
		return storage.ModelInstructionFamilyGPT
	default:
		return ""
	}
}

func modelInstructionPolicyForModel(group storage.Group, model string) (bool, []string, string) {
	if len(group.ModelInstructionProfiles) == 0 {
		return group.ModelInstructionsEnabled, append([]string(nil), group.ModelInstructionsFiles...), "legacy"
	}
	family := modelInstructionFamily(model)
	if family == "" {
		return false, nil, ""
	}
	profile, ok := group.ModelInstructionProfiles[family]
	if !ok {
		return false, nil, family
	}
	return profile.Enabled, append([]string(nil), profile.Files...), family
}

func superInstructPolicyForModel(group storage.Group, model string) (storage.SuperInstructProfile, string) {
	if len(group.SuperInstructProfiles) == 0 {
		return storage.SuperInstructProfile{
			Enabled:                group.SuperInstructEnabled,
			SkillIDs:               append([]string(nil), group.SuperInstructSkillIDs...),
			ResponseRewriteEnabled: group.SuperInstructResponseRewriteEnabled,
			MemoryEnabled:          group.SuperInstructMemoryEnabled,
			MonitorEnabled:         group.SuperInstructMonitorEnabled,
		}, "legacy"
	}
	family := modelInstructionFamily(model)
	if family == "" {
		return storage.SuperInstructProfile{}, ""
	}
	profile, ok := group.SuperInstructProfiles[family]
	if !ok {
		return storage.SuperInstructProfile{}, family
	}
	profile.SkillIDs = append([]string(nil), profile.SkillIDs...)
	return profile, family
}

func normalizeUserGroupInstructionConfig(group *storage.UserGroup) error {
	if group == nil {
		return nil
	}
	legacyFiles, err := normalizeModelInstructionFileNames(group.ModelInstructionsFiles)
	if err != nil {
		return err
	}
	if group.ModelInstructionsEnabled && len(legacyFiles) == 0 && len(group.ModelInstructionProfiles) == 0 {
		return errors.New("model instructions are enabled but no files were selected")
	}
	group.ModelInstructionsFiles = legacyFiles
	if len(group.ModelInstructionProfiles) == 0 {
		return nil
	}
	profiles := make(storage.ModelInstructionProfiles, len(group.ModelInstructionProfiles))
	for rawFamily, profile := range group.ModelInstructionProfiles {
		family := strings.ToLower(strings.TrimSpace(rawFamily))
		switch family {
		case storage.ModelInstructionFamilyGPT, storage.ModelInstructionFamilyClaude, storage.ModelInstructionFamilyGemini:
		default:
			return fmt.Errorf("unsupported model instruction family %q", rawFamily)
		}
		files, fileErr := normalizeModelInstructionFileNames(profile.Files)
		if fileErr != nil {
			return fmt.Errorf("%s instruction profile: %w", family, fileErr)
		}
		if profile.Enabled && len(files) == 0 {
			return fmt.Errorf("%s instruction profile is enabled but no files were selected", family)
		}
		profiles[family] = storage.ModelInstructionProfile{Enabled: profile.Enabled, Files: files}
	}
	group.ModelInstructionProfiles = profiles
	return nil
}

func withRequestUserGroupPolicy(ctx context.Context, group storage.UserGroup) context.Context {
	return withRequestAccountGroupPolicy(ctx, userGroupPolicyAsAccountGroup(group))
}

func withRequestAccountGroupPolicy(ctx context.Context, group storage.Group) context.Context {
	return context.WithValue(ctx, requestUserGroupPolicyContextKey{}, group)
}

func requestUserGroupPolicy(ctx context.Context) storage.Group {
	if group, ok := ctx.Value(requestUserGroupPolicyContextKey{}).(storage.Group); ok {
		return group
	}
	return storage.Group{}
}

func (s *Server) compileGroupModelInstructions(ctx context.Context, group storage.Group) (string, string, error) {
	compiled, _, err := s.compileModelInstructionFiles(group.Name, group.ModelInstructionsEnabled, group.ModelInstructionsFiles)
	if err != nil {
		return "", "", err
	}
	superCompiled, _, err := s.compileGroupSuperInstruct(ctx, group)
	if err != nil {
		return "", "", err
	}
	combined := joinInstructionParts(compiled, superCompiled)
	if strings.TrimSpace(combined) == "" {
		return "", "", nil
	}
	sum := sha256.Sum256([]byte(combined))
	return combined, hex.EncodeToString(sum[:]), nil
}

func (s *Server) compileGroupModelInstructionsForModel(ctx context.Context, group storage.Group, model string) (string, string, error) {
	components, err := s.compileGroupInstructionComponentsForModel(ctx, group, model)
	if err != nil {
		return "", "", err
	}
	combined := joinInstructionParts(components.Administrator, components.SuperInstruct)
	if strings.TrimSpace(combined) == "" {
		return "", "", nil
	}
	sum := sha256.Sum256([]byte(combined))
	return combined, hex.EncodeToString(sum[:]), nil
}

type modelInstructionComponents struct {
	Administrator string `json:"administrator,omitempty"`
	SuperInstruct string `json:"super_instruct,omitempty"`
}

func (s *Server) compileGroupInstructionComponentsForModel(ctx context.Context, group storage.Group, model string) (modelInstructionComponents, error) {
	enabled, files, family := modelInstructionPolicyForModel(group, model)
	name := group.Name
	if family != "" && family != "legacy" {
		name += "/" + family
	}
	administrator, _, err := s.compileModelInstructionFiles(name, enabled, files)
	if err != nil {
		return modelInstructionComponents{}, err
	}
	superCompiled, _, err := s.compileGroupSuperInstructForModel(ctx, group, model)
	if err != nil {
		return modelInstructionComponents{}, err
	}
	return modelInstructionComponents{
		Administrator: strings.TrimSpace(administrator),
		SuperInstruct: strings.TrimSpace(superCompiled),
	}, nil
}

func joinInstructionParts(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return strings.Join(out, "\n\n")
}

func (s *Server) compileModelInstructionFiles(groupName string, enabled bool, files []string) (string, string, error) {
	if !enabled {
		return "", "", nil
	}
	if len(files) == 0 {
		return "", "", fmt.Errorf("group %q enables model_instructions_file but has no files", groupName)
	}
	parts := make([]string, 0, len(files))
	for _, rawName := range files {
		name, err := normalizeModelInstructionFileName(rawName)
		if err != nil {
			return "", "", err
		}
		path := filepath.Join(s.modelInstructionsDir(), name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("read model instructions file %s: %w", name, err)
		}
		content := strings.TrimSpace(string(raw))
		if content == "" {
			return "", "", fmt.Errorf("model instructions file %s is empty", name)
		}
		parts = append(parts, content)
	}
	compiled := strings.Join(parts, "\n\n")
	sum := sha256.Sum256([]byte(compiled))
	return compiled, hex.EncodeToString(sum[:]), nil
}

func (s *Server) superInstructLibrary() superinstruct.Library {
	return superinstruct.New("")
}

func (s *Server) compileGroupSuperInstruct(ctx context.Context, group storage.Group) (string, string, error) {
	if len(group.SuperInstructProfiles) > 0 {
		families := make([]string, 0, len(group.SuperInstructProfiles))
		for family := range group.SuperInstructProfiles {
			families = append(families, family)
		}
		sort.Strings(families)
		parts := make([]string, 0, len(families))
		for _, family := range families {
			profile := group.SuperInstructProfiles[family]
			if !profile.Enabled {
				continue
			}
			compiled, _, err := s.superInstructLibrary().Compile(ctx, profile.SkillIDs)
			if err != nil {
				return "", "", err
			}
			if strings.TrimSpace(compiled) != "" {
				parts = append(parts, "## Super-Instruct profile: "+family+"\n\n"+strings.TrimSpace(compiled))
			}
		}
		combined := joinInstructionParts(parts...)
		if strings.TrimSpace(combined) == "" {
			return "", "", nil
		}
		sum := sha256.Sum256([]byte(combined))
		return combined, hex.EncodeToString(sum[:]), nil
	}
	if !group.SuperInstructEnabled {
		return "", "", nil
	}
	compiled, _, err := s.superInstructLibrary().Compile(ctx, group.SuperInstructSkillIDs)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(compiled) == "" {
		return "", "", nil
	}
	sum := sha256.Sum256([]byte(compiled))
	return compiled, hex.EncodeToString(sum[:]), nil
}

func (s *Server) compileGroupSuperInstructForModel(ctx context.Context, group storage.Group, model string) (string, string, error) {
	profile, _ := superInstructPolicyForModel(group, model)
	if !profile.Enabled {
		return "", "", nil
	}
	compiled, _, err := s.superInstructLibrary().Compile(ctx, profile.SkillIDs)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(compiled) == "" {
		return "", "", nil
	}
	sum := sha256.Sum256([]byte(compiled))
	return compiled, hex.EncodeToString(sum[:]), nil
}

func (s *Server) applyModelInstructionsForEntrypoint(ctx context.Context, group storage.Group, model, path string, raw []byte, metadata ...*bodysource.BodyMeta) ([]byte, error) {
	components, err := s.compileGroupInstructionComponentsForModel(ctx, group, model)
	if err != nil {
		return raw, err
	}
	groupPrompt := ""
	// Most groups have no prompt override. Avoid classifying (and therefore scanning)
	// a potentially million-token body until there is an instruction to apply.
	if configuredPrompt := strings.TrimSpace(group.SystemPrompt); configuredPrompt != "" &&
		prompt.ShouldRewrite(configuredPrompt, routing.IsCompaction(path, raw), group.SystemPromptApplyToCompaction) {
		groupPrompt = configuredPrompt
	}
	superPolicy, _ := superInstructPolicyForModel(group, model)
	if !superPolicy.Enabled {
		// M1 replaces complete system carriers while it is active.  A carrier with
		// our exact bundle marker is therefore stale gateway state, not a client
		// fragment to retain after the group/client gate has closed.
		var meta *bodysource.BodyMeta
		if len(metadata) > 0 {
			meta = metadata[0]
		}
		if meta == nil || meta.Size != int64(len(raw)) || meta.LegacyInstructionMark {
			raw, err = stripLegacySuperInstructCarriers(raw)
			if err != nil {
				return raw, err
			}
		}
	}
	if superPolicy.Enabled {
		bridge, bridgeErr := superinstruct.LoadBridge()
		if bridgeErr != nil {
			return raw, bridgeErr
		}
		// M1 replacement is selected by this request's client-masked model-family
		// group policy. Response-only module flags never inject the bridge.
		instructions := joinInstructionParts(bridge, groupPrompt, components.Administrator, components.SuperInstruct)
		if path == "/v1/messages" || path == "/v1/messages/count_tokens" {
			withoutSystem, deleteErr := stripAnthropicSystemCarriers(raw)
			if deleteErr != nil {
				return raw, deleteErr
			}
			updated, _, injectErr := prompt.InjectAnthropicSystemPrompt(withoutSystem, instructions)
			return updated, injectErr
		}
		updated, _, injectErr := superinstruct.InjectSystem(raw, instructions)
		return updated, injectErr
	}
	compiled := joinInstructionParts(groupPrompt, components.Administrator, components.SuperInstruct)
	if strings.TrimSpace(compiled) == "" {
		return raw, nil
	}
	switch path {
	case "/v1/chat/completions":
		updated, _, injectErr := prompt.InjectChatSystemPrompt(raw, compiled)
		return updated, injectErr
	case "/v1/messages", "/v1/messages/count_tokens":
		updated, _, injectErr := prompt.InjectAnthropicSystemPrompt(raw, compiled)
		return updated, injectErr
	default:
		return setResponsesInstructionParts(raw, groupPrompt, components.Administrator, components.SuperInstruct), nil
	}
}

// stripLegacySuperInstructCarriers removes only complete carriers previously
// owned by Super-Instruct M1.  It intentionally does not search ordinary user,
// assistant, developer, or tool content: a user discussing the marker must not
// lose their message.  Deleting array entries from the end keeps indices stable.
func stripLegacySuperInstructCarriers(raw []byte) ([]byte, error) {
	if !bytes.Contains(raw, []byte(legacySuperInstructBundleHeader)) {
		return raw, nil
	}
	if !gjson.ValidBytes(raw) {
		return raw, errors.New("invalid JSON while removing stale Super-Instruct carriers")
	}
	out := raw
	for _, field := range []string{"instructions", "system", "system_prompt", "personality"} {
		value := gjson.GetBytes(out, field)
		if !value.Exists() || !strings.Contains(value.Raw, legacySuperInstructBundleHeader) {
			continue
		}
		updated, err := sjson.DeleteBytes(out, field)
		if err != nil {
			return raw, fmt.Errorf("remove stale Super-Instruct %s carrier: %w", field, err)
		}
		out = updated
	}
	for _, field := range []string{"messages", "input"} {
		items := gjson.GetBytes(out, field)
		if !items.IsArray() {
			continue
		}
		values := items.Array()
		for index := len(values) - 1; index >= 0; index-- {
			item := values[index]
			if !strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "system") ||
				!strings.Contains(item.Raw, legacySuperInstructBundleHeader) {
				continue
			}
			updated, err := sjson.DeleteBytes(out, fmt.Sprintf("%s.%d", field, index))
			if err != nil {
				return raw, fmt.Errorf("remove stale Super-Instruct %s carrier: %w", field, err)
			}
			out = updated
		}
	}
	return out, nil
}

func stripAnthropicSystemCarriers(raw []byte) ([]byte, error) {
	updated, err := sjson.DeleteBytes(raw, "system")
	if err != nil {
		return nil, err
	}
	messages := gjson.GetBytes(updated, "messages")
	if !messages.IsArray() {
		return updated, nil
	}
	items := messages.Array()
	for index := len(items) - 1; index >= 0; index-- {
		if strings.TrimSpace(items[index].Get("role").String()) != "system" {
			continue
		}
		updated, err = sjson.DeleteBytes(updated, fmt.Sprintf("messages.%d", index))
		if err != nil {
			return nil, err
		}
	}
	return updated, nil
}

// CodexInstructionPlan is fixed once at the gateway entrance and then reused by
// every in-request transport attempt. In strict CPA mode it represents a durable
// tree snapshot, rather than the mutable group configuration visible at a later
// retry, WebSocket frame, compression turn, or native EOF continuation.
type CodexInstructionPlan struct {
	Instructions  string // versioned tree snapshot payload; legacy rows may be plain text
	Bridge        string
	GroupPrompt   string
	Administrator string
	SuperInstruct string
	Revision      string
	TreeID        string
	Strict        bool
	Source        string // disabled, configured, snapshot, lazy_migration, pending_root
	LocalM1       bool
}

func (p *CodexInstructionPlan) applies() bool {
	return p != nil && (strings.TrimSpace(p.Bridge) != "" || strings.TrimSpace(p.GroupPrompt) != "" ||
		strings.TrimSpace(p.Administrator) != "" || strings.TrimSpace(p.SuperInstruct) != "")
}

func (p *CodexInstructionPlan) apply(raw []byte) []byte {
	if !p.applies() {
		return raw
	}
	if p.LocalM1 {
		instructions := joinInstructionParts(p.Bridge, p.GroupPrompt, p.Administrator, p.SuperInstruct)
		if updated, injected, err := superinstruct.InjectSystem(raw, instructions); err == nil {
			if injected {
				return updated
			}
			// A native Responses continuation may use the compact string form
			// {"input":"..."} and omit every system carrier. M1 still belongs to
			// the immutable CPA tree, so add the classic top-level carrier instead
			// of letting upstream normalization synthesize a generic instruction.
			if updated, setErr := sjson.SetBytes(raw, "instructions", instructions); setErr == nil {
				return updated
			}
		}
		return raw
	}
	return setResponsesInstructionParts(raw, p.GroupPrompt, p.Administrator, p.SuperInstruct)
}

func (p *CodexInstructionPlan) snapshotCommit(treeID string, expiresAt int64) *storage.CodexInstructionSnapshot {
	if p == nil || !p.Strict {
		return nil
	}
	return &storage.CodexInstructionSnapshot{
		TreeID:       strings.TrimSpace(treeID),
		Instructions: p.Instructions,
		Revision:     p.Revision,
		ExpiresAt:    expiresAt,
	}
}

const codexInstructionSnapshotV2Prefix = "codex-pool-instruction-plan-v2\n"

type codexInstructionSnapshotV2 struct {
	Bridge        string `json:"bridge,omitempty"`
	GroupPrompt   string `json:"group_prompt,omitempty"`
	Administrator string `json:"administrator,omitempty"`
	SuperInstruct string `json:"super_instruct,omitempty"`
}

func encodeCodexInstructionSnapshot(parts codexInstructionSnapshotV2) string {
	parts.Bridge = strings.TrimSpace(parts.Bridge)
	parts.GroupPrompt = strings.TrimSpace(parts.GroupPrompt)
	parts.Administrator = strings.TrimSpace(parts.Administrator)
	parts.SuperInstruct = strings.TrimSpace(parts.SuperInstruct)
	if parts.Bridge == "" && parts.GroupPrompt == "" && parts.Administrator == "" && parts.SuperInstruct == "" {
		return ""
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return joinInstructionParts(parts.Bridge, parts.GroupPrompt, parts.Administrator, parts.SuperInstruct)
	}
	return codexInstructionSnapshotV2Prefix + string(raw)
}

func decodeCodexInstructionSnapshot(snapshot string) codexInstructionSnapshotV2 {
	if raw, ok := strings.CutPrefix(snapshot, codexInstructionSnapshotV2Prefix); ok {
		var parts codexInstructionSnapshotV2
		if json.Unmarshal([]byte(raw), &parts) == nil {
			parts.Bridge = strings.TrimSpace(parts.Bridge)
			parts.GroupPrompt = strings.TrimSpace(parts.GroupPrompt)
			parts.Administrator = strings.TrimSpace(parts.Administrator)
			parts.SuperInstruct = strings.TrimSpace(parts.SuperInstruct)
			return parts
		}
	}
	administrator, superCompiled := splitLegacyInstructionBundle(snapshot)
	return codexInstructionSnapshotV2{Administrator: administrator, SuperInstruct: superCompiled}
}

func newCodexInstructionPlan(instructions, revision, treeID, source string, strict bool) *CodexInstructionPlan {
	parts := decodeCodexInstructionSnapshot(instructions)
	return &CodexInstructionPlan{
		Instructions:  instructions,
		Bridge:        parts.Bridge,
		GroupPrompt:   parts.GroupPrompt,
		Administrator: parts.Administrator,
		SuperInstruct: parts.SuperInstruct,
		Revision:      revision,
		TreeID:        treeID,
		Strict:        strict,
		Source:        source,
	}
}

// configureSuperInstructM1Plan applies the current request's strict group ×
// client gate to a tree snapshot. A disabled continuation keeps its snapshotted
// group/admin instructions but must not reuse the root's bridge or skill bundle.
func (s *Server) configureSuperInstructM1Plan(plan *CodexInstructionPlan, enabled bool) (*CodexInstructionPlan, error) {
	if plan == nil {
		return nil, nil
	}
	if !enabled {
		plan.Bridge = ""
		plan.SuperInstruct = ""
		plan.LocalM1 = false
		return plan, nil
	}
	if strings.TrimSpace(plan.Bridge) == "" && strings.TrimSpace(plan.SuperInstruct) == "" {
		plan.LocalM1 = false
		return plan, nil
	}
	plan.LocalM1 = true
	if strings.TrimSpace(plan.Bridge) == "" {
		bridge, err := superinstruct.LoadBridge()
		if err != nil {
			return nil, err
		}
		plan.Bridge = bridge
	}
	return plan, nil
}

func (s *Server) compileCodexInstructionSnapshot(ctx context.Context, group storage.Group, model string, includeGroupPrompt bool) (string, error) {
	components, err := s.compileGroupInstructionComponentsForModel(ctx, group, model)
	if err != nil {
		return "", err
	}
	groupPrompt := ""
	if includeGroupPrompt {
		groupPrompt = group.SystemPrompt
	}
	bridge := ""
	superPolicy, _ := superInstructPolicyForModel(group, model)
	if superPolicy.Enabled {
		bridge, err = superinstruct.LoadBridge()
		if err != nil {
			return "", err
		}
	}
	return encodeCodexInstructionSnapshot(codexInstructionSnapshotV2{
		Bridge:        bridge,
		GroupPrompt:   groupPrompt,
		Administrator: components.Administrator,
		SuperInstruct: components.SuperInstruct,
	}), nil
}

// codexGroupInstructionPolicyRevision covers the complete tree-scoped instruction
// policy. The HMAC it returns is suitable for diagnostics; it does not expose file
// contents or the group prompt outside the keyed digest.
func (s *Server) codexGroupInstructionPolicyRevision(group storage.Group) string {
	parts := make([]string, 0, len(group.ModelInstructionsFiles)+8)
	parts = append(parts,
		strings.TrimSpace(group.Name),
		"prompt_mode:"+strings.TrimSpace(group.PromptMode),
		"system_prompt:"+group.SystemPrompt,
		fmt.Sprintf("system_prompt_compaction:%t", group.SystemPromptApplyToCompaction),
	)
	bridgeEnabled := group.SuperInstructEnabled
	for _, profile := range group.SuperInstructProfiles {
		bridgeEnabled = bridgeEnabled || profile.Enabled
	}
	if bridgeEnabled {
		parts = append(parts, "super_instruct_m1")
		if bridge, err := superinstruct.LoadBridge(); err == nil {
			sum := sha256.Sum256([]byte(bridge))
			parts = append(parts, "bridge:"+hex.EncodeToString(sum[:]))
		}
	}
	if group.ModelInstructionsEnabled {
		parts = append(parts, "enabled")
	} else {
		parts = append(parts, "disabled")
	}
	for _, name := range group.ModelInstructionsFiles {
		parts = append(parts, strings.TrimSpace(name))
	}
	if group.SuperInstructEnabled {
		parts = append(parts, "super_instruct_enabled")
	} else {
		parts = append(parts, "super_instruct_disabled")
	}
	for _, id := range group.SuperInstructSkillIDs {
		parts = append(parts, "super_skill:"+strings.TrimSpace(id))
	}
	superFamilies := make([]string, 0, len(group.SuperInstructProfiles))
	for family := range group.SuperInstructProfiles {
		superFamilies = append(superFamilies, family)
	}
	sort.Strings(superFamilies)
	for _, family := range superFamilies {
		profile := group.SuperInstructProfiles[family]
		parts = append(parts, "super_profile:"+family)
		if profile.Enabled {
			parts = append(parts, "instructions_enabled")
		} else {
			parts = append(parts, "instructions_disabled")
		}
		if profile.ResponseRewriteEnabled {
			parts = append(parts, "rewrite_enabled")
		}
		if profile.MemoryEnabled {
			parts = append(parts, "memory_enabled")
		}
		if profile.MonitorEnabled {
			parts = append(parts, "monitor_enabled")
		}
		for _, id := range profile.SkillIDs {
			parts = append(parts, "super_profile_skill:"+strings.TrimSpace(id))
		}
	}
	families := make([]string, 0, len(group.ModelInstructionProfiles))
	for family := range group.ModelInstructionProfiles {
		families = append(families, family)
	}
	sort.Strings(families)
	for _, family := range families {
		profile := group.ModelInstructionProfiles[family]
		parts = append(parts, family)
		if profile.Enabled {
			parts = append(parts, "enabled")
		} else {
			parts = append(parts, "disabled")
		}
		for _, name := range profile.Files {
			parts = append(parts, strings.TrimSpace(name))
		}
	}
	return s.store.CodexGroupPolicyRevision(strings.Join(parts, "\x00"))
}

// codexInstructionPlan resolves mutable group files only for a new root. Existing
// strict CPA trees always load the encrypted snapshot elected by their first root;
// a legacy tree with no row is migrated through a tree-level storage CAS before its
// first continuation is sent upstream.
func (s *Server) codexInstructionPlan(ctx context.Context, group storage.Group, mapping *codexSessionMapping, strict bool, model string, includeGroupPrompt bool) (*CodexInstructionPlan, error) {
	modelInstructionsEnabled, _, _ := modelInstructionPolicyForModel(group, model)
	superPolicy, _ := superInstructPolicyForModel(group, model)
	enabled := modelInstructionsEnabled || superPolicy.Enabled || (includeGroupPrompt && strings.TrimSpace(group.SystemPrompt) != "")
	if !strict {
		instructions, err := s.compileCodexInstructionSnapshot(ctx, group, model, includeGroupPrompt)
		if err != nil {
			return nil, err
		}
		return s.configureSuperInstructM1Plan(newCodexInstructionPlan(
			instructions,
			s.store.CodexInstructionRevision(instructions),
			"",
			map[bool]string{true: "configured", false: "disabled"}[enabled],
			false,
		), superPolicy.Enabled)
	}

	treeID := ""
	if mapping != nil {
		treeID = mapping.instructionTreeID()
	}
	if treeID != "" {
		snapshot, err := s.store.GetCodexInstructionSnapshot(ctx, treeID)
		if err == nil {
			return s.configureSuperInstructM1Plan(newCodexInstructionPlan(snapshot.Instructions, snapshot.Revision, snapshot.TreeID, "snapshot", true), superPolicy.Enabled)
		}
		if !errors.Is(err, storage.ErrCodexInstructionSnapshotNotFound) {
			return nil, err
		}
		instructions, compileErr := s.compileCodexInstructionSnapshot(ctx, group, model, includeGroupPrompt)
		if compileErr != nil {
			return nil, compileErr
		}
		stored, ensureErr := s.store.EnsureCodexInstructionSnapshot(ctx, storage.CodexInstructionSnapshot{
			TreeID:       treeID,
			Instructions: instructions,
			Revision:     s.store.CodexInstructionRevision(instructions),
			ExpiresAt:    time.Now().Add(s.codexSessionMappingRetention(ctx)).Unix(),
		})
		if ensureErr != nil {
			return nil, ensureErr
		}
		return s.configureSuperInstructM1Plan(newCodexInstructionPlan(stored.Instructions, stored.Revision, stored.TreeID, "lazy_migration", true), superPolicy.Enabled)
	}

	instructions, err := s.compileCodexInstructionSnapshot(ctx, group, model, includeGroupPrompt)
	if err != nil {
		return nil, err
	}
	return s.configureSuperInstructM1Plan(newCodexInstructionPlan(instructions, s.store.CodexInstructionRevision(instructions), "", "pending_root", true), superPolicy.Enabled)
}

func setResponsesInstructions(raw []byte, instructions string) []byte {
	administrator, superCompiled := splitLegacyInstructionBundle(instructions)
	return setResponsesInstructionParts(raw, "", administrator, superCompiled)
}

func splitLegacyInstructionBundle(instructions string) (string, string) {
	trimmed := strings.TrimSpace(instructions)
	if strings.HasPrefix(trimmed, legacySuperInstructBundleHeader) {
		return "", trimmed
	}
	if index := strings.Index(trimmed, "\n\n"+legacySuperInstructBundleHeader); index >= 0 {
		return strings.TrimSpace(trimmed[:index]), strings.TrimSpace(trimmed[index+2:])
	}
	return trimmed, ""
}

// setResponsesInstructionParts preserves the official Codex request envelope and
// applies the three gateway instruction sources with distinct semantics:
//   - administrator files retain their historical base-replacement behavior;
//   - the group prompt and Super-Instruct are additive;
//   - a Lite top-level instructions string becomes its own developer item.
//
// Every input item is retained as raw JSON. This is important for future tool
// fields and integer identifiers that cannot survive map[string]interface{}.
func setResponsesInstructionParts(raw []byte, groupPrompt, administrator, superCompiled string) []byte {
	groupPrompt = strings.TrimSpace(groupPrompt)
	administrator = strings.TrimSpace(administrator)
	superCompiled = strings.TrimSpace(superCompiled)
	if groupPrompt == "" && administrator == "" && superCompiled == "" {
		return raw
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	topInstructions := ""
	if topRaw, present := fields["instructions"]; present {
		if err := json.Unmarshal(topRaw, &topInstructions); err != nil {
			return raw
		}
		topInstructions = strings.TrimSpace(topInstructions)
	}

	if upstream.CodexRequestUsesResponsesLite(raw) {
		input, ok := codexRawInputItems(fields["input"])
		if !ok || len(input) == 0 {
			return raw
		}
		updated := append([]json.RawMessage(nil), input...)
		baseIndex := 0
		if codexLiteAdditionalTools(updated[0]) {
			baseIndex = 1
		}
		insertAt := baseIndex
		if baseIndex < len(updated) && codexDeveloperMessage(updated[baseIndex]) {
			insertAt = baseIndex + 1
			if administrator != "" {
				developer, err := codexDeveloperInstructionsItem(administrator)
				if err != nil {
					return raw
				}
				updated[baseIndex] = developer
			}
		} else if administrator != "" {
			developer, err := codexDeveloperInstructionsItem(administrator)
			if err != nil {
				return raw
			}
			updated = insertCodexRawItem(updated, insertAt, developer)
			insertAt++
		}

		// A non-Super-Instruct administrator bundle retains its historical full
		// replacement semantics, including replacement of the Lite top-level
		// instructions field. Without that explicit override, native top-level
		// instructions are preserved as their own developer item.
		additiveTopInstructions := topInstructions
		if administrator != "" {
			additiveTopInstructions = ""
		}
		for _, text := range []string{additiveTopInstructions, groupPrompt, superCompiled} {
			if text == "" || codexDeveloperTextExists(updated, text) {
				continue
			}
			developer, err := codexDeveloperInstructionsItem(text)
			if err != nil {
				return raw
			}
			updated = insertCodexRawItem(updated, insertAt, developer)
			insertAt++
		}
		out, err := sjson.SetRawBytes(raw, "input", codexMarshalRawArray(updated))
		if err != nil {
			return raw
		}
		if _, present := fields["instructions"]; present {
			out, err = sjson.DeleteBytes(out, "instructions")
			if err != nil {
				return raw
			}
		}
		return out
	}

	// Classic Responses keeps administrator replacement semantics. Additive sources
	// are composed around that base, with Super-Instruct appended after the original
	// client instructions when no administrator override is active.
	base := topInstructions
	if administrator != "" {
		base = administrator
	}
	base = prependInstructionBlock(base, groupPrompt)
	base = appendInstructionBlock(base, superCompiled)
	if base == "" {
		return raw
	}
	if out, err := sjson.SetBytes(raw, "instructions", base); err == nil {
		return out
	}
	return raw
}

func insertCodexRawItem(items []json.RawMessage, index int, item json.RawMessage) []json.RawMessage {
	if index < 0 {
		index = 0
	}
	if index > len(items) {
		index = len(items)
	}
	items = append(items, nil)
	copy(items[index+1:], items[index:])
	items[index] = item
	return items
}

func instructionBlockExists(text, block string) bool {
	text = strings.TrimSpace(text)
	block = strings.TrimSpace(block)
	if text == "" || block == "" {
		return false
	}
	return text == block || strings.HasPrefix(text, block+"\n\n") ||
		strings.HasSuffix(text, "\n\n"+block) || strings.Contains(text, "\n\n"+block+"\n\n")
}

func prependInstructionBlock(text, block string) string {
	if block = strings.TrimSpace(block); block == "" || instructionBlockExists(text, block) {
		return strings.TrimSpace(text)
	}
	return joinInstructionParts(block, text)
}

func appendInstructionBlock(text, block string) string {
	if block = strings.TrimSpace(block); block == "" || instructionBlockExists(text, block) {
		return strings.TrimSpace(text)
	}
	return joinInstructionParts(text, block)
}

func codexRawInputItems(raw json.RawMessage) ([]json.RawMessage, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false
	}
	return items, true
}

func codexLiteAdditionalTools(raw json.RawMessage) bool {
	var item struct {
		Type  string          `json:"type"`
		Role  string          `json:"role"`
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || item.Type != "additional_tools" || item.Role != "developer" {
		return false
	}
	var tools []json.RawMessage
	return json.Unmarshal(item.Tools, &tools) == nil
}

func codexDeveloperMessage(raw json.RawMessage) bool {
	var item struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	return json.Unmarshal(raw, &item) == nil && item.Type == "message" && item.Role == "developer"
}

func codexDeveloperInstructionsItem(instructions string) (json.RawMessage, error) {
	return json.Marshal(struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{
		Type: "message",
		Role: "developer",
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "input_text", Text: instructions}},
	})
}

func codexDeveloperTextExists(messages []json.RawMessage, instructions string) bool {
	for _, message := range messages {
		if !codexDeveloperMessage(message) {
			continue
		}
		var item struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(message, &item) != nil {
			continue
		}
		var content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(item.Content, &content) == nil {
			for _, block := range content {
				if block.Type == "input_text" && block.Text == instructions {
					return true
				}
			}
			continue
		}
		var text string
		if json.Unmarshal(item.Content, &text) == nil && text == instructions {
			return true
		}
	}
	return false
}

func codexMarshalRawArray(items []json.RawMessage) json.RawMessage {
	var out bytes.Buffer
	out.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(bytes.TrimSpace(item))
	}
	out.WriteByte(']')
	return out.Bytes()
}

// writeCodexInstructionConfigurationError makes a bad administrator instruction
// file distinguishable from a client-side Responses validation failure. It is
// deliberately emitted before account selection or an upstream attempt, so it
// cannot affect an existing CPA tree whose encrypted snapshot is already present.
func writeCodexInstructionConfigurationError(w http.ResponseWriter, err error) {
	writePoolCodeError(w, http.StatusBadRequest, "codex_instruction_configuration_error", err.Error())
}
