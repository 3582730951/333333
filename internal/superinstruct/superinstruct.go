package superinstruct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	EnvDir        = "CODEX_POOL_SUPER_INSTRUCT_DIR"
	EnvBridgeFile = "CODEX_POOL_SUPER_INSTRUCT_BRIDGE_FILE"
)

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

const compileCacheLimit = 128

type compileCacheEntry struct {
	compiled string
	skills   []Skill
	lastUsed uint64
}

var bundleCompileCache = struct {
	sync.Mutex
	entries map[string]compileCacheEntry
}{entries: make(map[string]compileCacheEntry)}

var bundleCompileClock atomic.Uint64
var bundleCompileHits atomic.Uint64
var bundleCompileMisses atomic.Uint64

type CompilerStats struct {
	Hits       uint64 `json:"hits"`
	Misses     uint64 `json:"misses"`
	Entries    int    `json:"entries"`
	MaxEntries int    `json:"max_entries"`
}

func CompileStatsSnapshot() CompilerStats {
	bundleCompileCache.Lock()
	entries := len(bundleCompileCache.entries)
	bundleCompileCache.Unlock()
	return CompilerStats{Hits: bundleCompileHits.Load(), Misses: bundleCompileMisses.Load(), Entries: entries, MaxEntries: compileCacheLimit}
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

// DefaultBridgeFile resolves the M1 bridge beside the skills directory. The
// release installer exports an explicit path; deriving it keeps local checkout
// and test usage zero-configuration.
func DefaultBridgeFile() string {
	if env := strings.TrimSpace(os.Getenv(EnvBridgeFile)); env != "" {
		return env
	}
	return filepath.Join(filepath.Dir(DefaultDir()), "bridge.md")
}

func LoadBridge() (string, error) {
	path := DefaultBridgeFile()
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Super-Instruct bridge %s: %w", path, err)
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("Super-Instruct bridge %s is empty", path)
	}
	return DecodeBridgeContent(raw), nil
}

// DecodeBridgeContent returns the plaintext bridge instructions. The release
// bundle stores bridge.md as base64-encoded UTF-16LE so the injected resource is
// not plaintext on disk; ordinary UTF-8 deployments pass through unchanged.
func DecodeBridgeContent(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 4 {
		return string(raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(trimmed))
	if err != nil || len(decoded) < 2 || len(decoded)%2 != 0 {
		return string(raw)
	}
	// Only treat the file as encoded when its UTF-16LE interpretation is real
	// text: no NUL rune, no unpaired-surrogate replacement char, and a
	// substantial share of zero bytes. ASCII-heavy UTF-16LE puts a NUL in the
	// high byte of every ASCII rune, so a genuine encoded bridge shows ~20%
	// zero bytes; base64 of plain UTF-8 ASCII shows none, which is exactly the
	// false positive a naive decode would mangle into CJK mojibake.
	zeroBytes := 0
	for _, b := range decoded {
		if b == 0 {
			zeroBytes++
		}
	}
	text := string(utf16.Decode(makeUTF16LE(decoded)))
	if strings.ContainsRune(text, 0) || strings.ContainsRune(text, utf8.RuneError) {
		return string(raw)
	}
	if float64(zeroBytes)/float64(len(decoded)) < 0.10 {
		return string(raw)
	}
	if text = strings.TrimSpace(text); text == "" {
		return string(raw)
	}
	return text
}

// makeUTF16LE reinterprets little-endian bytes as UTF-16 code units.
func makeUTF16LE(b []byte) []uint16 {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return units
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
	cacheKey, keyErr := l.compileCacheKey(ids)
	if keyErr == nil {
		bundleCompileCache.Lock()
		if cached, ok := bundleCompileCache.entries[cacheKey]; ok {
			cached.lastUsed = bundleCompileClock.Add(1)
			bundleCompileCache.entries[cacheKey] = cached
			bundleCompileCache.Unlock()
			bundleCompileHits.Add(1)
			return cached.compiled, append([]Skill(nil), cached.skills...), nil
		}
		bundleCompileCache.Unlock()
	}
	bundleCompileMisses.Add(1)
	compiled, skills, err := l.compileUncached(ctx, ids)
	if err != nil {
		return "", nil, err
	}
	if keyErr == nil {
		bundleCompileCache.Lock()
		if len(bundleCompileCache.entries) >= compileCacheLimit {
			oldestKey := ""
			oldestUse := ^uint64(0)
			for key, entry := range bundleCompileCache.entries {
				if entry.lastUsed < oldestUse {
					oldestKey, oldestUse = key, entry.lastUsed
				}
			}
			delete(bundleCompileCache.entries, oldestKey)
		}
		bundleCompileCache.entries[cacheKey] = compileCacheEntry{compiled: compiled, skills: append([]Skill(nil), skills...), lastUsed: bundleCompileClock.Add(1)}
		bundleCompileCache.Unlock()
	}
	return compiled, skills, nil
}

func (l Library) compileCacheKey(ids []string) (string, error) {
	root, err := filepath.Abs(l.root())
	if err != nil {
		return "", err
	}
	includeResources := len(ids) > 0
	targetIDs := append([]string(nil), ids...)
	if len(targetIDs) == 0 {
		entries, readErr := os.ReadDir(root)
		if readErr != nil {
			return "", readErr
		}
		for _, entry := range entries {
			if entry.IsDir() && ValidSkillID(entry.Name()) {
				targetIDs = append(targetIDs, entry.Name())
			}
		}
		sort.Strings(targetIDs)
	}
	var fingerprint strings.Builder
	fingerprint.WriteString("super-instruct-compiled-v2\x00")
	fingerprint.WriteString(root)
	for _, id := range targetIDs {
		fingerprint.WriteByte(0)
		fingerprint.WriteString(id)
		dir := filepath.Join(root, id)
		files := []string{"SKILL.md"}
		if includeResources {
			files, err = skillAllRegularFiles(dir)
			if err != nil {
				return "", err
			}
		}
		for _, rel := range files {
			info, statErr := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
			if statErr != nil {
				return "", statErr
			}
			fingerprint.WriteByte(0)
			fingerprint.WriteString(rel)
			fingerprint.WriteString(fmt.Sprintf(":%d:%d", info.Size(), info.ModTime().UnixNano()))
		}
	}
	digest := sha256.Sum256([]byte(fingerprint.String()))
	return hex.EncodeToString(digest[:]), nil
}

func skillAllRegularFiles(dir string) ([]string, error) {
	files := []string{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(files)
	return files, err
}

func (l Library) compileUncached(ctx context.Context, ids []string) (string, []Skill, error) {
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
