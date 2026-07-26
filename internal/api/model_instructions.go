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

	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/storage"
	"github.com/tidwall/sjson"
)

type modelInstructionFileView struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	UpdatedAt int64  `json:"updated_at"`
	Error     string `json:"error,omitempty"`
}

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
		Name:                          group.ID,
		SystemPrompt:                  group.SystemPrompt,
		PromptMode:                    group.PromptMode,
		SystemPromptApplyToCompaction: group.SystemPromptApplyToCompaction,
		ModelInstructionsEnabled:      group.ModelInstructionsEnabled,
		ModelInstructionsFiles:        append([]string(nil), group.ModelInstructionsFiles...),
		ModelInstructionProfiles:      profiles,
		ForceModel:                    group.ForceModel,
		ForceEffort:                   group.ForceEffort,
	}
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
	return context.WithValue(ctx, requestUserGroupPolicyContextKey{}, userGroupPolicyAsAccountGroup(group))
}

func requestUserGroupPolicy(ctx context.Context) storage.Group {
	if group, ok := ctx.Value(requestUserGroupPolicyContextKey{}).(storage.Group); ok {
		return group
	}
	return storage.Group{}
}

func (s *Server) compileGroupModelInstructions(ctx context.Context, group storage.Group) (string, string, error) {
	_ = ctx
	return s.compileModelInstructionFiles(group.Name, group.ModelInstructionsEnabled, group.ModelInstructionsFiles)
}

func (s *Server) compileGroupModelInstructionsForModel(ctx context.Context, group storage.Group, model string) (string, string, error) {
	_ = ctx
	enabled, files, family := modelInstructionPolicyForModel(group, model)
	name := group.Name
	if family != "" && family != "legacy" {
		name += "/" + family
	}
	return s.compileModelInstructionFiles(name, enabled, files)
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

func (s *Server) applyModelInstructionsForEntrypoint(ctx context.Context, group storage.Group, model, path string, raw []byte) ([]byte, error) {
	compiled, _, err := s.compileGroupModelInstructionsForModel(ctx, group, model)
	if err != nil || strings.TrimSpace(compiled) == "" {
		return raw, err
	}
	switch path {
	case "/v1/chat/completions":
		updated, _, injectErr := prompt.InjectChatSystemPrompt(raw, compiled)
		return updated, injectErr
	case "/v1/messages", "/v1/messages/count_tokens":
		updated, _, injectErr := prompt.InjectAnthropicSystemPrompt(raw, compiled)
		return updated, injectErr
	default:
		return setResponsesInstructions(raw, compiled), nil
	}
}

// CodexInstructionPlan is fixed once at the gateway entrance and then reused by
// every in-request transport attempt. In strict CPA mode it represents a durable
// tree snapshot, rather than the mutable group configuration visible at a later
// retry, WebSocket frame, compression turn, or native EOF continuation.
type CodexInstructionPlan struct {
	Instructions string
	Revision     string
	TreeID       string
	Strict       bool
	Source       string // disabled, configured, snapshot, lazy_migration, pending_root
}

func (p *CodexInstructionPlan) applies() bool {
	return p != nil && strings.TrimSpace(p.Instructions) != ""
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

func (s *Server) compileCodexInstructionSnapshot(ctx context.Context, group storage.Group, model string) (string, error) {
	compiled, _, err := s.compileGroupModelInstructionsForModel(ctx, group, model)
	if err != nil {
		return "", err
	}
	return compiled, nil
}

// codexGroupInstructionPolicyRevision intentionally covers only administrator
// configuration metadata: group identity, whether the feature is enabled, and
// the ordered file list. The HMAC it returns is suitable for diagnostics but
// neither it nor the bundle ever contains the instruction file contents.
func (s *Server) codexGroupInstructionPolicyRevision(group storage.Group) string {
	parts := make([]string, 0, len(group.ModelInstructionsFiles)+8)
	parts = append(parts, strings.TrimSpace(group.Name))
	if group.ModelInstructionsEnabled {
		parts = append(parts, "enabled")
	} else {
		parts = append(parts, "disabled")
	}
	for _, name := range group.ModelInstructionsFiles {
		parts = append(parts, strings.TrimSpace(name))
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
func (s *Server) codexInstructionPlan(ctx context.Context, group storage.Group, mapping *codexSessionMapping, strict bool, model string) (*CodexInstructionPlan, error) {
	enabled, _, _ := modelInstructionPolicyForModel(group, model)
	if !strict {
		instructions, err := s.compileCodexInstructionSnapshot(ctx, group, model)
		if err != nil {
			return nil, err
		}
		return &CodexInstructionPlan{
			Instructions: instructions,
			Revision:     s.store.CodexInstructionRevision(instructions),
			Source:       map[bool]string{true: "configured", false: "disabled"}[enabled],
		}, nil
	}

	treeID := ""
	if mapping != nil {
		treeID = mapping.instructionTreeID()
	}
	if treeID != "" {
		snapshot, err := s.store.GetCodexInstructionSnapshot(ctx, treeID)
		if err == nil {
			return &CodexInstructionPlan{
				Instructions: snapshot.Instructions,
				Revision:     snapshot.Revision,
				TreeID:       snapshot.TreeID,
				Strict:       true,
				Source:       "snapshot",
			}, nil
		}
		if !errors.Is(err, storage.ErrCodexInstructionSnapshotNotFound) {
			return nil, err
		}
		instructions, compileErr := s.compileCodexInstructionSnapshot(ctx, group, model)
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
		return &CodexInstructionPlan{
			Instructions: stored.Instructions,
			Revision:     stored.Revision,
			TreeID:       stored.TreeID,
			Strict:       true,
			Source:       "lazy_migration",
		}, nil
	}

	instructions, err := s.compileCodexInstructionSnapshot(ctx, group, model)
	if err != nil {
		return nil, err
	}
	return &CodexInstructionPlan{
		Instructions: instructions,
		Revision:     s.store.CodexInstructionRevision(instructions),
		Strict:       true,
		Source:       "pending_root",
	}, nil
}

func setResponsesInstructions(raw []byte, instructions string) []byte {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return raw
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	// Current Codex Lite requests carry base instructions as the item immediately
	// after a leading additional_tools prefix. Preserve the prefix *as raw JSON*,
	// then replace (or insert) exactly one base developer message. Do not use a
	// map[string]interface{} round-trip here: tool call ids and context values can
	// legitimately exceed float64 precision, and all unrelated wire fragments must
	// survive unchanged.
	if input, ok := codexRawInputItems(fields["input"]); ok && len(input) > 0 && codexLiteAdditionalTools(input[0]) {
		developer, err := codexDeveloperInstructionsItem(instructions)
		if err != nil {
			return raw
		}
		updated := make([]json.RawMessage, 0, len(input)+1)
		updated = append(updated, input[0], developer)
		// The Lite envelope reserves the consecutive developer messages directly
		// after additional_tools for its base instructions. Drop all of those
		// placeholders before inserting ours so a malformed/retried client packet
		// cannot leave multiple competing base-instruction messages in the prefix.
		// A later developer message (after a non-developer input item) is ordinary
		// conversation content and remains byte-for-byte intact.
		firstConversationItem := 1
		for firstConversationItem < len(input) && codexDeveloperMessage(input[firstConversationItem]) {
			firstConversationItem++
		}
		updated = append(updated, input[firstConversationItem:]...)
		out, err := sjson.SetRawBytes(raw, "input", codexMarshalRawArray(updated))
		if err != nil {
			return raw
		}
		if _, present := fields["instructions"]; present {
			if out, err = sjson.DeleteBytes(out, "instructions"); err != nil {
				return raw
			}
		}
		return out
	}
	// This is a classic Responses request (or an unknown envelope). Never invent a
	// Lite prefix merely because a client happened to use an input array.
	if out, err := sjson.SetBytes(raw, "instructions", instructions); err == nil {
		return out
	}
	return raw
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
