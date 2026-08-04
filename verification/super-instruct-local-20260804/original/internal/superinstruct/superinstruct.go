package superinstruct

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const EnvDir = "CODEX_POOL_SUPER_INSTRUCT_DIR"

// Skill is one hot-pluggable Super-Instruct skill directory.
type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled,omitempty"`
	FileCount   int    `json:"file_count"`
	UpdatedAt   int64  `json:"updated_at"`
	Error       string `json:"error,omitempty"`
}

// Library resolves and compiles the migrated Super-Instruct resource bundle.
type Library struct {
	Dir string
}

// New returns a Library rooted at dir. An empty dir uses default discovery.
func New(dir string) Library {
	return Library{Dir: strings.TrimSpace(dir)}
}

// DefaultDir resolves the hot-pluggable skills directory. Runtime deployments can
// replace the directory or point EnvDir elsewhere without changing code.
func DefaultDir() string {
	if env := strings.TrimSpace(os.Getenv(EnvDir)); env != "" {
		return env
	}
	candidates := []string{}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "super-instruct", "codex-skills"))
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, "super-instruct", "codex-skills"),
			filepath.Join(filepath.Dir(exeDir), "super-instruct", "codex-skills"),
		)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return filepath.Join("super-instruct", "codex-skills")
}

func (l Library) root() string {
	if strings.TrimSpace(l.Dir) != "" {
		return strings.TrimSpace(l.Dir)
	}
	return DefaultDir()
}

// NormalizeSkillIDs validates identifier syntax and removes duplicates while
// preserving order. Existence is checked separately because the directory is hot
// pluggable and may change between configuration and request time.
func NormalizeSkillIDs(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		id := strings.TrimSpace(value)
		if id == "" {
			continue
		}
		if !ValidSkillID(id) {
			return nil, fmt.Errorf("invalid Super-Instruct skill id %q", value)
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

func ValidSkillID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// List scans the current directory state every call. This intentionally avoids a
// long-lived cache so adding/removing skill folders is hot-pluggable.
func (l Library) List(ctx context.Context) ([]Skill, error) {
	_ = ctx
	root := l.root()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []Skill{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]Skill, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if !ValidSkillID(id) {
			continue
		}
		skill := Skill{ID: id, Name: id}
		dir := filepath.Join(root, id)
		skill.FileCount, skill.UpdatedAt = countFiles(dir)
		skill.Name, skill.Description, skill.Error = parseMetadata(filepath.Join(dir, "SKILL.md"), id)
		out = append(out, skill)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Compile builds one instruction bundle from selected skill IDs. Empty selected
// progressively discloses only SKILL.md from every currently available skill;
// an explicit selection also includes that skill's supporting resources.
func (l Library) Compile(ctx context.Context, selected []string) (string, []Skill, error) {
	ids, err := NormalizeSkillIDs(selected)
	if err != nil {
		return "", nil, err
	}
	includeResources := len(ids) > 0
	all, err := l.List(ctx)
	if err != nil {
		return "", nil, err
	}
	byID := make(map[string]Skill, len(all))
	allIDs := make([]string, 0, len(all))
	for _, skill := range all {
		byID[skill.ID] = skill
		if skill.Error == "" {
			allIDs = append(allIDs, skill.ID)
		}
	}
	if len(ids) == 0 {
		ids = allIDs
	}
	if len(ids) == 0 {
		return "", nil, errors.New("Super-Instruct module is enabled but no skills are installed")
	}
	root := l.root()
	parts := []string{
		"# Super-Instruct Codex 5.6",
		"The Super-Instruct module is enabled for this user group. The following hot-plugged skill instructions are delivered through the instruction filesystem.",
		"Use the skill whose trigger/description matches the current task. If multiple skills apply, combine them in the order shown.",
	}
	selectedSkills := make([]Skill, 0, len(ids))
	for _, id := range ids {
		skill, ok := byID[id]
		if !ok {
			return "", nil, fmt.Errorf("Super-Instruct skill %q is not installed", id)
		}
		if strings.TrimSpace(skill.Error) != "" {
			return "", nil, fmt.Errorf("Super-Instruct skill %q is invalid: %s", id, skill.Error)
		}
		path := filepath.Join(root, id, "SKILL.md")
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", nil, fmt.Errorf("read Super-Instruct skill %s: %w", id, readErr)
		}
		content := strings.TrimSpace(string(raw))
		if content == "" {
			return "", nil, fmt.Errorf("Super-Instruct skill %s has an empty SKILL.md", id)
		}
		skill.Enabled = true
		selectedSkills = append(selectedSkills, skill)
		parts = append(parts, fmt.Sprintf("## Skill: %s", id), formatSkillFile("SKILL.md", content))
		if !includeResources {
			continue
		}
		extraFiles, fileErr := skillInstructionFiles(filepath.Join(root, id))
		if fileErr != nil {
			return "", nil, fmt.Errorf("scan Super-Instruct skill %s: %w", id, fileErr)
		}
		for _, rel := range extraFiles {
			raw, readErr := os.ReadFile(filepath.Join(root, id, rel))
			if readErr != nil {
				return "", nil, fmt.Errorf("read Super-Instruct skill %s file %s: %w", id, rel, readErr)
			}
			if !utf8.Valid(raw) {
				parts = append(parts, fmt.Sprintf("### File: %s\n\n[omitted: binary or non-UTF-8 file]", rel))
				continue
			}
			if extra := strings.TrimSpace(string(raw)); extra != "" {
				parts = append(parts, formatSkillFile(rel, extra))
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n")), selectedSkills, nil
}

func skillInstructionFiles(dir string) ([]string, error) {
	files := []string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "SKILL.md" {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func formatSkillFile(rel, content string) string {
	lang := languageForFile(rel)
	fence := "````"
	for strings.Contains(content, fence) {
		fence += "`"
	}
	if lang != "" {
		return fmt.Sprintf("### File: %s\n\n%s%s\n%s\n%s", rel, fence, lang, content, fence)
	}
	return fmt.Sprintf("### File: %s\n\n%s\n%s\n%s", rel, fence, content, fence)
}

func languageForFile(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return "markdown"
	case ".py":
		return "python"
	case ".go":
		return "go"
	case ".js", ".jsx":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".json":
		return "json"
	case ".toml":
		return "toml"
	case ".yaml", ".yml":
		return "yaml"
	case ".sh", ".bash":
		return "bash"
	default:
		return ""
	}
}

func parseMetadata(path, fallbackName string) (string, string, string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fallbackName, "", err.Error()
	}
	content := string(raw)
	name := fallbackName
	description := ""
	if strings.HasPrefix(content, "---") {
		rest := content[3:]
		if end := strings.Index(rest, "---"); end >= 0 {
			front := rest[:end]
			for _, line := range strings.Split(front, "\n") {
				line = strings.TrimSpace(line)
				if value, ok := strings.CutPrefix(line, "name:"); ok {
					name = strings.Trim(strings.TrimSpace(value), "\"")
				}
				if value, ok := strings.CutPrefix(line, "description:"); ok {
					description = strings.Trim(strings.TrimSpace(value), "\"")
				}
			}
		}
	}
	return name, description, ""
}

func countFiles(dir string) (int, int64) {
	count := 0
	var newest int64
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		count++
		if info, statErr := d.Info(); statErr == nil && info.ModTime().Unix() > newest {
			newest = info.ModTime().Unix()
		}
		return nil
	})
	return count, newest
}
