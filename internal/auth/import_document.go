package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"codex-account-pool/internal/agentidentity"
)

const (
	ImportFormatSingle        = "auth-json"
	ImportFormatArray         = "auth-json-array"
	ImportFormatSub2API       = "sub2api-data"
	ImportFormatSub2APILegacy = "sub2api-bundle"
	sub2APIImportDataVersion  = 1
)

// ImportDocument is the normalized representation consumed by both account
// import endpoints. Entry errors are isolated so one malformed account does not
// discard the rest of a sub2api backup.
type ImportDocument struct {
	Format  string
	Entries []ImportEntry
	Proxies []Sub2APIProxy
}

type ImportEntry struct {
	Index    int
	Name     string
	ProxyKey string
	Parsed   ParsedAuth
	Warnings []string
	Err      error
}

type Sub2APIProxy struct {
	ProxyKey        string `json:"proxy_key"`
	Name            string `json:"name"`
	Protocol        string `json:"protocol"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	Status          string `json:"status,omitempty"`
	ExpiresAt       *int64 `json:"expires_at,omitempty"`
	FallbackMode    string `json:"fallback_mode,omitempty"`
	BackupProxyName string `json:"backup_proxy_name,omitempty"`
	ExpiryWarnDays  int    `json:"expiry_warn_days,omitempty"`
}

type sub2APIAccount struct {
	Name           string                 `json:"name"`
	Platform       string                 `json:"platform"`
	Type           string                 `json:"type"`
	Credentials    map[string]interface{} `json:"credentials"`
	Extra          map[string]interface{} `json:"extra"`
	ProxyKey       *string                `json:"proxy_key"`
	Concurrency    int                    `json:"concurrency"`
	Priority       int                    `json:"priority"`
	RateMultiplier *float64               `json:"rate_multiplier"`
	ExpiresAt      *int64                 `json:"expires_at"`
	AutoPause      *bool                  `json:"auto_pause_on_expired"`
}

// ParseImportDocument accepts the historical single auth.json object, an array
// of those objects, and sub2api's version-1 backup envelope.
func ParseImportDocument(raw []byte) (ImportDocument, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ImportDocument{}, errors.New("auth.json content is empty")
	}
	if strings.HasPrefix(trimmed, "[") {
		return parseAuthArray([]byte(trimmed))
	}
	var root map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &root); err != nil {
		return ImportDocument{}, err
	}
	if isSub2APIEnvelope(root) {
		return parseSub2APIEnvelope([]byte(trimmed), root)
	}
	parsed, err := ParseAuthJSON([]byte(trimmed))
	if err != nil {
		return ImportDocument{}, err
	}
	return ImportDocument{Format: ImportFormatSingle, Entries: []ImportEntry{{Index: 1, Parsed: parsed}}}, nil
}

func parseAuthArray(raw []byte) (ImportDocument, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return ImportDocument{}, err
	}
	if len(values) == 0 {
		return ImportDocument{}, errors.New("auth.json array is empty")
	}
	doc := ImportDocument{Format: ImportFormatArray, Entries: make([]ImportEntry, 0, len(values))}
	for index, value := range values {
		entry := ImportEntry{Index: index + 1}
		entry.Parsed, entry.Err = ParseAuthJSON(value)
		doc.Entries = append(doc.Entries, entry)
	}
	return doc, nil
}

func isSub2APIEnvelope(root map[string]interface{}) bool {
	typeName := strings.TrimSpace(stringField(root, "type"))
	if typeName == ImportFormatSub2API || typeName == ImportFormatSub2APILegacy {
		return true
	}
	_, hasAccounts := root["accounts"]
	_, hasVersion := root["version"]
	_, hasExportedAt := root["exported_at"]
	_, hasProxies := root["proxies"]
	return hasAccounts && (hasVersion || hasExportedAt || hasProxies)
}

func parseSub2APIEnvelope(raw []byte, root map[string]interface{}) (ImportDocument, error) {
	typeName := strings.TrimSpace(stringField(root, "type"))
	if typeName != "" && typeName != ImportFormatSub2API && typeName != ImportFormatSub2APILegacy {
		return ImportDocument{}, fmt.Errorf("unsupported sub2api data type %q", typeName)
	}
	version := epochSecondsField(root, "version")
	if version != 0 && version != sub2APIImportDataVersion {
		return ImportDocument{}, fmt.Errorf("unsupported sub2api data version %d", version)
	}
	if proxies, ok := root["proxies"]; !ok || proxies == nil {
		return ImportDocument{}, errors.New("sub2api data has no proxies array")
	}
	var envelope struct {
		Proxies  []Sub2APIProxy  `json:"proxies"`
		Accounts json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ImportDocument{}, err
	}
	if len(envelope.Accounts) == 0 || string(envelope.Accounts) == "null" {
		return ImportDocument{}, errors.New("sub2api data has no accounts array")
	}
	var accounts []sub2APIAccount
	if err := json.Unmarshal(envelope.Accounts, &accounts); err != nil {
		return ImportDocument{}, errors.New("sub2api accounts must be an array")
	}
	doc := ImportDocument{Format: ImportFormatSub2API, Proxies: envelope.Proxies, Entries: make([]ImportEntry, 0, len(accounts))}
	for index, account := range accounts {
		doc.Entries = append(doc.Entries, normalizeSub2APIAccount(index+1, account))
	}
	return doc, nil
}

func normalizeSub2APIAccount(index int, account sub2APIAccount) ImportEntry {
	entry := ImportEntry{Index: index, Name: strings.TrimSpace(account.Name)}
	if account.ProxyKey != nil {
		entry.ProxyKey = strings.TrimSpace(*account.ProxyKey)
	}
	if entry.Name == "" {
		entry.Err = errors.New("sub2api account name is required")
		return entry
	}
	if account.Concurrency < 0 || account.Priority < 0 || (account.RateMultiplier != nil && *account.RateMultiplier < 0) {
		entry.Err = errors.New("sub2api concurrency, priority, and rate_multiplier must be non-negative")
		return entry
	}
	platform := strings.ToLower(strings.TrimSpace(account.Platform))
	if platform != "openai" && platform != "codex" {
		entry.Err = fmt.Errorf("unsupported platform %q; only OpenAI/Codex accounts can be imported here", account.Platform)
		return entry
	}
	if !strings.EqualFold(strings.TrimSpace(account.Type), "oauth") {
		entry.Err = fmt.Errorf("unsupported OpenAI account type %q; API keys require the cost-confirmed key importer", account.Type)
		return entry
	}
	if len(account.Credentials) == 0 {
		entry.Err = errors.New("sub2api account credentials are empty")
		return entry
	}
	credentials := make(map[string]interface{}, len(account.Credentials)+8)
	for key, value := range account.Credentials {
		credentials[key] = value
	}
	copyMissingString := func(key string, sources ...map[string]interface{}) {
		if strings.TrimSpace(stringField(credentials, key)) != "" {
			return
		}
		for _, source := range sources {
			if value := strings.TrimSpace(stringField(source, key)); value != "" {
				credentials[key] = value
				return
			}
		}
	}
	if entry.Name != "" && strings.TrimSpace(stringField(credentials, "name")) == "" {
		credentials["name"] = entry.Name
	}
	for _, key := range []string{"email", "account_id", "chatgpt_account_id", "chatgpt_user_id", "workspace_id", "plan_type"} {
		copyMissingString(key, account.Extra)
	}
	encoded, _ := json.Marshal(credentials)
	entry.Parsed, entry.Err = ParseAuthJSON(encoded)
	if entry.Err != nil {
		return entry
	}
	if entry.Parsed.Name == "" {
		entry.Parsed.Name = entry.Name
	}
	if entry.Parsed.CredentialMode == agentidentity.CredentialMode && strings.TrimSpace(entry.Parsed.AgentTaskID) == "" {
		entry.Warnings = append(entry.Warnings, "agent identity has no task_id; the first request will register one through the account's bound egress")
	}
	if account.Concurrency != 0 || account.Priority != 0 || account.RateMultiplier != nil {
		entry.Warnings = append(entry.Warnings, "sub2api concurrency/priority/rate_multiplier were not copied because this pool uses different scheduling semantics")
	}
	if account.ExpiresAt != nil || account.AutoPause != nil {
		entry.Warnings = append(entry.Warnings, "sub2api expires_at/auto_pause_on_expired were not copied; use this pool's lifecycle controls instead")
	}
	return entry
}
