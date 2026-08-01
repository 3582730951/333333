package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"codex-account-pool/internal/storage"
)

func normalizeEmailPoolStatus(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "", "idle", "ready", "available", "unused", "active", "valid":
		return "idle"
	case "in_use", "inuse", "busy", "reserved", "using", "processing":
		return "in_use"
	case "error", "failed", "invalid", "disabled", "dead":
		return "error"
	case "used", "consumed", "done", "completed":
		return "used"
	default:
		return normalized
	}
}

func normalizeEmailPoolCounts(input map[string]int) map[string]int {
	out := map[string]int{}
	for status, count := range input {
		out[normalizeEmailPoolStatus(status)] += count
	}
	return out
}

func decodeEmailPoolDeleteIDs(reader io.Reader) ([]string, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, adminJSONBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > adminJSONBodyLimit {
		return nil, errors.New("email pool delete request exceeds request limit")
	}
	var root interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	values := root
	if object, ok := root.(map[string]interface{}); ok {
		values = firstLegacyValue(object, "ids", "account_ids", "accountIds", "email_ids", "emailIds")
		if values == nil {
			values = firstLegacyValue(object, "id", "account_id", "accountId")
		}
	}
	seen := map[string]bool{}
	ids := make([]string, 0)
	appendID := func(value interface{}) error {
		text, ok := value.(string)
		if !ok {
			return errors.New("email account ids must be strings")
		}
		text = strings.TrimSpace(text)
		if text != "" && !seen[text] {
			seen[text] = true
			ids = append(ids, text)
		}
		return nil
	}
	switch typed := values.(type) {
	case []interface{}:
		for _, value := range typed {
			if err := appendID(value); err != nil {
				return nil, err
			}
		}
	case string:
		if err := appendID(typed); err != nil {
			return nil, err
		}
	case nil:
	default:
		return nil, errors.New("ids must be an array or string")
	}
	return ids, nil
}

func decodeEmailPoolImport(raw []byte, contentTypeJSON bool) ([]storage.EmailAccount, string, []string, error) {
	trimmed := bytes.TrimSpace(raw)
	looksJSON := len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '"')
	if !contentTypeJSON && !looksJSON {
		accounts, problems := parseEmailPoolLines(string(raw), "")
		return accounts, "", problems, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var root interface{}
	if err := decoder.Decode(&root); err != nil {
		return nil, "", nil, err
	}
	return decodeEmailPoolImportValue(root, "")
}

func decodeEmailPoolImportValue(root interface{}, inheritedGroup string) ([]storage.EmailAccount, string, []string, error) {
	switch value := root.(type) {
	case string:
		accounts, problems := parseEmailPoolLines(value, inheritedGroup)
		return accounts, inheritedGroup, problems, nil
	case []interface{}:
		accounts, problems := parseEmailPoolRecords(value, inheritedGroup)
		return accounts, inheritedGroup, problems, nil
	case map[string]interface{}:
		group, err := stringAlias(value, "group_name", "groupName", "group")
		if err != nil {
			return nil, "", nil, fmt.Errorf("group_name: %w", err)
		}
		group = firstNonEmpty(group, inheritedGroup)
		if text := firstLegacyValue(value, "text", "raw", "content", "value"); text != nil {
			textValue, ok := text.(string)
			if !ok {
				return nil, "", nil, errors.New("email pool import text must be a string")
			}
			accounts, problems := parseEmailPoolLines(textValue, group)
			return accounts, group, problems, nil
		}
		if records := firstLegacyValue(value, "accounts", "email_accounts", "emailAccounts", "rows", "items", "list"); records != nil {
			list, ok := records.([]interface{})
			if !ok {
				return nil, "", nil, errors.New("email pool accounts must be an array")
			}
			accounts, problems := parseEmailPoolRecords(list, group)
			return accounts, group, problems, nil
		}
		if nested := firstLegacyValue(value, "data", "result", "payload"); nested != nil {
			return decodeEmailPoolImportValue(nested, group)
		}
		if firstLegacyValue(value, "email", "address", "account") != nil {
			accounts, problems := parseEmailPoolRecords([]interface{}{value}, group)
			return accounts, group, problems, nil
		}
		return nil, group, nil, errors.New("email pool import payload contains no accounts")
	default:
		return nil, "", nil, errors.New("email pool import must be text, an array, or an object")
	}
}

func parseEmailPoolLines(raw, group string) ([]storage.EmailAccount, []string) {
	accounts := make([]storage.EmailAccount, 0)
	problems := make([]string, 0)
	for index, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "----", 4)
		if len(parts) < 4 {
			for _, delimiter := range []string{"\t", "|"} {
				parts = strings.SplitN(line, delimiter, 4)
				if len(parts) == 4 {
					break
				}
			}
		}
		if len(parts) != 4 {
			problems = append(problems, fmt.Sprintf("line %d: expected four email credential fields", index+1))
			continue
		}
		account := storage.EmailAccount{
			Email: strings.TrimSpace(parts[0]), Password: strings.TrimSpace(parts[1]),
			ClientID: strings.TrimSpace(parts[2]), RefreshToken: strings.TrimSpace(parts[3]),
			Status: "idle", GroupName: group,
		}
		if err := validateEmailPoolImportAccount(account); err != nil {
			problems = append(problems, fmt.Sprintf("line %d: %v", index+1, err))
			continue
		}
		accounts = append(accounts, account)
	}
	return accounts, problems
}

func parseEmailPoolRecords(records []interface{}, group string) ([]storage.EmailAccount, []string) {
	accounts := make([]storage.EmailAccount, 0, len(records))
	problems := make([]string, 0)
	for index, value := range records {
		if text, ok := value.(string); ok {
			parsed, errs := parseEmailPoolLines(text, group)
			accounts = append(accounts, parsed...)
			for _, err := range errs {
				problems = append(problems, fmt.Sprintf("record %d: %s", index+1, err))
			}
			continue
		}
		object, ok := value.(map[string]interface{})
		if !ok {
			problems = append(problems, fmt.Sprintf("record %d: expected an object or credential line", index+1))
			continue
		}
		email, emailErr := stringAlias(object, "email", "address", "account", "username")
		password, passwordErr := stringAlias(object, "password", "pass", "mail_password", "mailPassword")
		clientID, clientErr := stringAlias(object, "client_id", "clientId", "oauth_client_id", "oauthClientId")
		refreshToken, tokenErr := stringAlias(object, "refresh_token", "refreshToken", "token", "oauth_refresh_token")
		rowGroup, groupErr := stringAlias(object, "group_name", "groupName", "group")
		status, statusErr := stringAlias(object, "status", "state")
		if err := firstNonNilError(emailErr, passwordErr, clientErr, tokenErr, groupErr, statusErr); err != nil {
			problems = append(problems, fmt.Sprintf("record %d: %v", index+1, err))
			continue
		}
		account := storage.EmailAccount{
			Email: email, Password: password, ClientID: clientID, RefreshToken: refreshToken,
			Status: normalizeEmailPoolStatus(status), GroupName: firstNonEmpty(rowGroup, group),
		}
		if err := validateEmailPoolImportAccount(account); err != nil {
			problems = append(problems, fmt.Sprintf("record %d: %v", index+1, err))
			continue
		}
		accounts = append(accounts, account)
	}
	return accounts, problems
}

func stringAlias(values map[string]interface{}, keys ...string) (string, error) {
	value := firstLegacyValue(values, keys...)
	if value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", errors.New("credential fields must be strings")
	}
	return strings.TrimSpace(text), nil
}

func firstNonNilError(values ...error) error {
	for _, err := range values {
		if err != nil {
			return err
		}
	}
	return nil
}

func validateEmailPoolImportAccount(account storage.EmailAccount) error {
	if strings.TrimSpace(account.Email) == "" || strings.TrimSpace(account.RefreshToken) == "" {
		return errors.New("email and refresh_token are required")
	}
	if len(account.Email) > 320 || len(account.Password) > 4096 || len(account.ClientID) > 1024 ||
		len(account.RefreshToken) > 16384 || len(account.GroupName) > 128 || strings.Count(account.Email, "@") != 1 {
		return errors.New("one or more fields are invalid or exceed their limit")
	}
	return nil
}
