package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	authparse "codex-account-pool/internal/auth"
)

type importConfig struct {
	PoolURL   string
	APIKey    string
	JSONDir   string
	GroupName string
	EgressID  string
	Recursive bool
}

type importSummary struct {
	Imported  int
	Duplicate int
	Failed    int
}

type importResult struct {
	Path       string
	Status     string
	HTTPStatus int
	Error      string
}

func main() {
	cfg, err := promptImportConfig(os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		os.Exit(2)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	summary, err := runImport(context.Background(), cfg, client, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "导入失败: %v\n", err)
		os.Exit(1)
	}
	if summary.Failed > 0 {
		os.Exit(1)
	}
}

func promptImportConfig(in io.Reader, out io.Writer) (importConfig, error) {
	reader := bufio.NewReader(in)
	ask := func(prompt string) (string, error) {
		fmt.Fprint(out, prompt)
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	poolURL, err := ask("账号池 URL: ")
	if err != nil {
		return importConfig{}, err
	}
	apiKey, err := ask("poolimp_ API key: ")
	if err != nil {
		return importConfig{}, err
	}
	jsonDir, err := ask("JSON 目录: ")
	if err != nil {
		return importConfig{}, err
	}
	groupName, err := ask("分组 (可空): ")
	if err != nil {
		return importConfig{}, err
	}
	egressID, err := ask("账号默认出口 (可空，默认 egress_direct): ")
	if err != nil {
		return importConfig{}, err
	}
	recursiveText, err := ask("是否递归扫描子目录? [y/N]: ")
	if err != nil {
		return importConfig{}, err
	}
	cfg := importConfig{
		PoolURL:   poolURL,
		APIKey:    apiKey,
		JSONDir:   jsonDir,
		GroupName: groupName,
		EgressID:  egressID,
		Recursive: parseYes(recursiveText),
	}
	if err := cfg.validate(); err != nil {
		return importConfig{}, err
	}
	return cfg, nil
}

func parseYes(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "y", "yes", "true", "1", "是":
		return true
	default:
		return false
	}
}

func (cfg importConfig) validate() error {
	if strings.TrimSpace(cfg.PoolURL) == "" {
		return errors.New("账号池 URL 必填")
	}
	u, err := url.Parse(strings.TrimSpace(cfg.PoolURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("账号池 URL 必须是完整 URL")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return errors.New("API key 必填")
	}
	if !strings.HasPrefix(strings.TrimSpace(cfg.APIKey), "poolimp_") {
		return errors.New("API key 必须以 poolimp_ 开头")
	}
	if strings.TrimSpace(cfg.JSONDir) == "" {
		return errors.New("JSON 目录必填")
	}
	info, err := os.Stat(cfg.JSONDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("JSON 路径不是目录")
	}
	return nil
}

func collectJSONFiles(dir string, recursive bool) ([]string, error) {
	dir = strings.TrimSpace(dir)
	var files []string
	if recursive {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != dir && isOldDirName(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.EqualFold(filepath.Ext(d.Name()), ".json") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
				files = append(files, filepath.Join(dir, entry.Name()))
			}
		}
	}
	sort.Strings(files)
	return files, nil
}

func runImport(ctx context.Context, cfg importConfig, client *http.Client, out io.Writer) (importSummary, error) {
	if client == nil {
		client = http.DefaultClient
	}
	files, err := collectJSONFiles(cfg.JSONDir, cfg.Recursive)
	if err != nil {
		return importSummary{}, err
	}
	if len(files) == 0 {
		return importSummary{}, fmt.Errorf("目录 %s 中没有 JSON 文件", cfg.JSONDir)
	}
	fmt.Fprintf(out, "扫描到 %d 个 JSON 文件（递归: %s）\n", len(files), yesNo(cfg.Recursive))
	if !cfg.Recursive {
		if nested, err := countNestedJSONFiles(cfg.JSONDir); err == nil && nested > 0 {
			fmt.Fprintf(out, "提示: 子目录里还有 %d 个 JSON 文件；如需导入它们，请重新运行并选择递归扫描。\n", nested)
		}
	}
	printDuplicateIdentityWarnings(files, out)
	fmt.Fprintln(out)
	var summary importSummary
	for _, path := range files {
		result := importOneFile(ctx, cfg, client, path)
		switch result.Status {
		case "imported":
			summary.Imported++
			if archived, err := archiveJSONFile(cfg.JSONDir, path); err != nil {
				summary.Failed++
				fmt.Fprintf(out, "imported  %s archive_failed error=%s\n", filepath.Base(path), trimForDisplay(err.Error()))
			} else {
				fmt.Fprintf(out, "imported  %s -> %s\n", filepath.Base(path), archived)
			}
		case "duplicate":
			summary.Duplicate++
			if archived, err := archiveJSONFile(cfg.JSONDir, path); err != nil {
				summary.Failed++
				fmt.Fprintf(out, "duplicate %s archive_failed error=%s\n", filepath.Base(path), trimForDisplay(err.Error()))
			} else {
				fmt.Fprintf(out, "duplicate %s -> %s\n", filepath.Base(path), archived)
			}
		default:
			summary.Failed++
			if result.HTTPStatus > 0 {
				fmt.Fprintf(out, "failed    %s status=%d error=%s\n", filepath.Base(path), result.HTTPStatus, result.Error)
			} else {
				fmt.Fprintf(out, "failed    %s error=%s\n", filepath.Base(path), result.Error)
			}
		}
	}
	fmt.Fprintf(out, "\n汇总: imported=%d duplicate=%d failed=%d\n", summary.Imported, summary.Duplicate, summary.Failed)
	return summary, nil
}

func yesNo(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

func countNestedJSONFiles(dir string) (int, error) {
	root, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return 0, err
	}
	count := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && isOldDirName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		parent, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return err
		}
		if parent == root {
			return nil
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ".json") {
			count++
		}
		return nil
	})
	return count, err
}

func isOldDirName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "old")
}

func archiveJSONFile(rootDir, path string) (string, error) {
	rootAbs, err := filepath.Abs(strings.TrimSpace(rootDir))
	if err != nil {
		return "", err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return "", err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s 不在 JSON 目录 %s 下", path, rootDir)
	}
	target := filepath.Join(rootAbs, "old", rel)
	target, err = uniqueArchivePath(target)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(pathAbs, target); err != nil {
		return "", err
	}
	displayRel, err := filepath.Rel(rootAbs, target)
	if err != nil {
		return filepath.ToSlash(target), nil
	}
	return filepath.ToSlash(displayRel), nil
}

func uniqueArchivePath(target string) (string, error) {
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return target, nil
	} else if err != nil {
		return "", err
	}
	ext := filepath.Ext(target)
	stem := strings.TrimSuffix(target, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s_%d%s", stem, i, ext)
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
}

func printDuplicateIdentityWarnings(files []string, out io.Writer) {
	byAccountID := map[string][]string{}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		parsed, err := authparse.ParseAuthJSON(raw)
		if err != nil || strings.TrimSpace(parsed.AccountID) == "" {
			continue
		}
		byAccountID[parsed.AccountID] = append(byAccountID[parsed.AccountID], filepath.Base(path))
	}
	ids := make([]string, 0, len(byAccountID))
	for id, names := range byAccountID {
		if len(names) > 1 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		names := append([]string(nil), byAccountID[id]...)
		sort.Strings(names)
		fmt.Fprintf(out, "警告: %d 个 JSON 会映射到同一账号 ID %s，账号池只会保留一个；文件: %s\n", len(names), id, strings.Join(names, ", "))
	}
}

func importOneFile(ctx context.Context, cfg importConfig, client *http.Client, path string) importResult {
	raw, err := os.ReadFile(path)
	if err != nil {
		return importResult{Path: path, Status: "failed", Error: err.Error()}
	}
	body := map[string]string{
		"label":          labelFromPath(path),
		"group_name":     strings.TrimSpace(cfg.GroupName),
		"egress_id":      strings.TrimSpace(cfg.EgressID),
		"auth_json_text": string(raw),
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return importResult{Path: path, Status: "failed", Error: err.Error()}
	}
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.PoolURL), "/") + "/api/account-pool/import"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return importResult{Path: path, Status: "failed", Error: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.APIKey))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return importResult{Path: path, Status: "failed", Error: err.Error()}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	status, msg := classifyImportResponse(resp.StatusCode, respBody)
	return importResult{Path: path, Status: status, HTTPStatus: resp.StatusCode, Error: msg}
}

func labelFromPath(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext)
}

func classifyImportResponse(statusCode int, body []byte) (string, string) {
	if statusCode < 200 || statusCode >= 300 {
		return "failed", responseErrorMessage(body)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "imported", ""
	}
	if duplicate, _ := decoded["duplicate"].(bool); duplicate {
		return "duplicate", ""
	}
	switch strings.ToLower(strings.TrimSpace(fmt.Sprint(decoded["import_status"]))) {
	case "duplicate":
		return "duplicate", ""
	case "failed":
		return "failed", responseErrorMessage(body)
	default:
		return "imported", ""
	}
}

func responseErrorMessage(body []byte) string {
	var decoded map[string]interface{}
	if err := json.Unmarshal(body, &decoded); err == nil {
		switch v := decoded["error"].(type) {
		case string:
			return trimForDisplay(v)
		case map[string]interface{}:
			if msg, _ := v["message"].(string); msg != "" {
				return trimForDisplay(msg)
			}
		}
		if msg, _ := decoded["message"].(string); msg != "" {
			return trimForDisplay(msg)
		}
	}
	return trimForDisplay(string(body))
}

func trimForDisplay(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}
