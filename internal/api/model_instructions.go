package api

import (
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

	"codex-account-pool/internal/storage"
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
	for _, value := range values {
		name, err := normalizeModelInstructionFileName(value)
		if err != nil {
			return nil, err
		}
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

func (s *Server) compileGroupModelInstructions(ctx context.Context, group storage.Group) (string, string, error) {
	_ = ctx
	if !group.ModelInstructionsEnabled {
		return "", "", nil
	}
	if len(group.ModelInstructionsFiles) == 0 {
		return "", "", fmt.Errorf("group %q enables model_instructions_file but has no files", group.Name)
	}
	parts := make([]string, 0, len(group.ModelInstructionsFiles))
	for _, rawName := range group.ModelInstructionsFiles {
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

func setResponsesInstructions(raw []byte, instructions string) []byte {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return raw
	}
	root, ok := unmarshalObject(raw)
	if !ok {
		return raw
	}
	root["instructions"] = instructions
	return marshalObjectOrRaw(root, raw)
}

func unmarshalObject(raw []byte) (map[string]interface{}, bool) {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false
	}
	return root, true
}

func marshalObjectOrRaw(root map[string]interface{}, raw []byte) []byte {
	out, err := json.Marshal(root)
	if err != nil {
		return raw
	}
	return out
}
