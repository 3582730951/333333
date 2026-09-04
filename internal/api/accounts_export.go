package api

import (
	"archive/zip"
	"bytes"
	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/storage"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type accountExportRecord struct {
	Account storage.Account
	Token   storage.AccountToken
}

type codexAuthTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id,omitempty"`
}

// codexAuthDocument tracks openai/codex's current AuthDotJson layout. The
// OPENAI_API_KEY member intentionally has no omitempty: OAuth auth.json files
// serialize it as null, exactly as Codex does.
type codexAuthDocument struct {
	AuthMode     string           `json:"auth_mode,omitempty"`
	OpenAIAPIKey any              `json:"OPENAI_API_KEY"`
	Tokens       *codexAuthTokens `json:"tokens,omitempty"`
	LastRefresh  string           `json:"last_refresh,omitempty"`
}

type cliProxyCodexDocument struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
	LastRefresh  string `json:"last_refresh"`
	Email        string `json:"email"`
	Type         string `json:"type"`
	Expired      string `json:"expired"`
}

// adminAccountsExport exports either the pool's legacy tabular formats, a
// complete portable backup, CLIProxyAPI Codex credentials, or the official
// Codex auth.json format. Compatibility credential formats are deliberately
// generated one account per file; multiple accounts are returned as a ZIP.
// auditAccountCredentialExport records one bulk credential-export access under the
// same action the confirmed POST path uses, so both land in a single queryable
// stream.
//
// The GET path emits exactly the same plaintext access tokens, refresh tokens and
// API keys as the confirmed POST path — in JSON, CSV, a bare token list, the
// upstream-compatible formats, and the full portable archive — but it wrote no audit
// row at all. Two doors to the same secrets where only one is recorded means the
// after-the-fact question "who pulled the pool's credentials, and when" was
// answerable only for whoever used the door that asks for confirmation.
//
// Only the account-ID digest is recorded, never the IDs themselves and never any
// credential material.
func (s *Server) auditAccountCredentialExport(r *http.Request, format, state, reason string, requested, exported int) {
	ids := requestedAccountBackupIDs(r)
	sort.Strings(ids)
	digest := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	scope := "selection"
	if len(ids) == 0 {
		scope = "entire_pool"
	}
	s.enqueueAudit(storage.AuditLogRow{
		Action: "account_credentials_export", State: state, Reason: reason,
		Detail: fmt.Sprintf("actor=%s method=%s format=%s scope=%s accounts_hash=%s requested=%d exported=%d",
			strictExportActor(s, r), r.Method, format, scope,
			hex.EncodeToString(digest[:8]), requested, exported),
		CreatedAt: storage.Now(),
	})
}

func (s *Server) adminAccountsExport(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method == http.MethodPost {
		s.handleStrictAccountExport(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "json"
	}
	// Audited before emission, not after: the response body streams, so a row written
	// only on clean completion would lose exactly the truncated export an
	// investigation cares about.
	requestedIDs := len(requestedAccountBackupIDs(r))
	if format == "backup" || format == "archive" || format == "portable" {
		s.auditAccountCredentialExport(r, format, "success", "unconfirmed_download", requestedIDs, requestedIDs)
		s.writeAccountsBackupDownload(w, r)
		return
	}
	records, explicitSelection, err := s.accountExportRecords(r)
	if err != nil {
		s.auditAccountCredentialExport(r, format, "failed", "load_records", requestedIDs, 0)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(records) == 0 {
		s.auditAccountCredentialExport(r, format, "denied", "empty_pool", requestedIDs, 0)
		writeError(w, http.StatusNotFound, errors.New("account pool is empty"))
		return
	}
	s.auditAccountCredentialExport(r, format, "success", "unconfirmed_download", requestedIDs, len(records))

	switch format {
	case "cliproxyapi", "cliproxy", "cli-proxy-api":
		s.writeCompatibleAccountExport(w, records, explicitSelection, "cliproxyapi")
		return
	case "auth", "auth.json", "codex-auth", "codex_auth":
		s.writeCompatibleAccountExport(w, records, explicitSelection, "codex-auth")
		return
	}

	type rec struct {
		ID           string `json:"id"`
		Email        string `json:"email"`
		Label        string `json:"label"`
		Group        string `json:"group_name"`
		Provider     string `json:"provider"`
		Status       string `json:"status"`
		AccessToken  string `json:"access_token,omitempty"`
		RefreshToken string `json:"refresh_token,omitempty"`
		OpenAIAPIKey string `json:"openai_api_key,omitempty"`
	}
	recs := make([]rec, 0, len(records))
	for _, record := range records {
		a, tok := record.Account, record.Token
		recs = append(recs, rec{ID: a.ID, Email: a.Email, Label: a.Label, Group: a.GroupName,
			Provider: a.Provider, Status: a.Status, AccessToken: tok.AccessToken,
			RefreshToken: tok.RefreshToken, OpenAIAPIKey: tok.OpenAIAPIKey})
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	switch format {
	case "csv":
		var buf bytes.Buffer
		cw := csv.NewWriter(&buf)
		_ = cw.Write([]string{"id", "email", "label", "group", "provider", "status", "access_token", "refresh_token", "openai_api_key"})
		for _, row := range recs {
			_ = cw.Write([]string{row.ID, row.Email, row.Label, row.Group, row.Provider, row.Status, row.AccessToken, row.RefreshToken, row.OpenAIAPIKey})
		}
		cw.Flush()
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", contentDispositionAttachment("accounts.csv"))
		_, _ = w.Write(buf.Bytes())
	case "at", "tokens":
		var b strings.Builder
		for _, row := range recs {
			if row.AccessToken != "" {
				b.WriteString(row.AccessToken)
				b.WriteByte('\n')
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", contentDispositionAttachment("access_tokens.txt"))
		_, _ = w.Write([]byte(b.String()))
	default:
		w.Header().Set("Content-Disposition", contentDispositionAttachment("accounts.json"))
		writeJSON(w, http.StatusOK, recs)
	}
}

func (s *Server) accountExportRecords(r *http.Request) ([]accountExportRecord, bool, error) {
	requested := requestedAccountBackupIDs(r)
	var accounts []storage.Account
	if len(requested) == 0 {
		items, err := s.store.ListAccounts(r.Context())
		if err != nil {
			return nil, false, err
		}
		accounts = items
		sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	} else {
		byID, err := s.listAccountArchiveAccountsByIDs(r, requested)
		if err != nil {
			return nil, true, err
		}
		accounts = make([]storage.Account, 0, len(requested))
		for _, id := range requested {
			account, ok := byID[id]
			if !ok {
				return nil, true, fmt.Errorf("account %q not found", id)
			}
			accounts = append(accounts, account)
		}
	}
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	tokens, err := s.store.ListTokensByAccountIDs(r.Context(), ids)
	if err != nil {
		return nil, len(requested) > 0, err
	}
	records := make([]accountExportRecord, 0, len(accounts))
	for _, account := range accounts {
		token, ok := tokens[account.ID]
		if !ok {
			if len(requested) > 0 {
				return nil, true, fmt.Errorf("account %q has no credential", account.ID)
			}
			continue
		}
		records = append(records, accountExportRecord{Account: account, Token: token})
	}
	return records, len(requested) > 0, nil
}

func (s *Server) writeCompatibleAccountExport(w http.ResponseWriter, records []accountExportRecord, strict bool, format string) {
	type exportFile struct {
		name string
		body []byte
	}
	files := make([]exportFile, 0, len(records))
	skipped := 0
	exportedAt := time.Now().UTC()
	usedNames := make(map[string]int, len(records))
	for index, record := range records {
		provider := accountprovider.EffectiveProvider(record.Account.Provider, record.Token, true)
		if provider != "codex" {
			if strict {
				writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("account %q uses provider %q; %s only supports Codex credentials", record.Account.ID, provider, format))
				return
			}
			skipped++
			continue
		}
		var document any
		var name string
		var err error
		if format == "cliproxyapi" {
			document, err = cliProxyDocument(record)
			name = cliProxyCredentialFilename(record)
		} else {
			document, err = officialCodexAuthDocument(record)
			if len(records) == 1 {
				name = "auth.json"
			} else {
				name = codexMultiAuthFilename(record.Account, exportedAt, index)
			}
		}
		if err != nil {
			if strict {
				writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("account %q: %w", record.Account.ID, err))
				return
			}
			skipped++
			continue
		}
		body, marshalErr := json.MarshalIndent(document, "", "  ")
		if marshalErr != nil {
			writeError(w, http.StatusInternalServerError, marshalErr)
			return
		}
		body = append(body, '\n')
		name = uniqueCredentialFilename(name, usedNames)
		files = append(files, exportFile{name: name, body: body})
	}
	if len(files) == 0 {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("no accounts can be exported as %s", format))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accounts-Exported", strconv.Itoa(len(files)))
	w.Header().Set("X-Accounts-Skipped", strconv.Itoa(skipped))
	if len(files) == 1 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", contentDispositionAttachment(files[0].name))
		w.Header().Set("Content-Length", strconv.Itoa(len(files[0].body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(files[0].body)
		return
	}
	var archive bytes.Buffer
	bounded := &accountArchiveBoundedWriter{dst: &archive, remaining: accountArchiveMaxUploadBytes, err: errGeneratedAccountArchiveTooLarge}
	zw := zip.NewWriter(bounded)
	for _, file := range files {
		entry, err := zw.Create(file.name)
		if err == nil {
			_, err = entry.Write(file.body)
		}
		if err != nil {
			_ = zw.Close()
			status := http.StatusInternalServerError
			if errors.Is(err, errGeneratedAccountArchiveTooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			writeError(w, status, err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errGeneratedAccountArchiveTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, err)
		return
	}
	filename := format + "-" + exportedAt.Format("20060102-150405") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDispositionAttachment(filename))
	w.Header().Set("Content-Length", strconv.Itoa(archive.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(archive.Bytes())
}

func officialCodexAuthDocument(record accountExportRecord) (codexAuthDocument, error) {
	token := record.Token
	if accountprovider.UsesAPIKey("codex", token) {
		key := accountprovider.Credential("codex", token)
		if key == "" {
			return codexAuthDocument{}, errors.New("API key is empty")
		}
		return codexAuthDocument{AuthMode: "apikey", OpenAIAPIKey: key}, nil
	}
	if strings.TrimSpace(token.IDTokenRaw) == "" || strings.TrimSpace(token.AccessToken) == "" {
		return codexAuthDocument{}, errors.New("official ChatGPT auth.json requires both id_token and access_token")
	}
	if syntheticCodexIDToken(token.IDTokenRaw) {
		return codexAuthDocument{}, errors.New("official ChatGPT auth.json cannot export a local metadata-only synthetic id_token")
	}
	lastRefresh := ""
	if token.LastRefresh > 0 {
		lastRefresh = time.Unix(token.LastRefresh, 0).UTC().Format(time.RFC3339)
	}
	return codexAuthDocument{
		AuthMode: "chatgpt", OpenAIAPIKey: nil, LastRefresh: lastRefresh,
		Tokens: &codexAuthTokens{IDToken: token.IDTokenRaw, AccessToken: token.AccessToken,
			RefreshToken: token.RefreshToken, AccountID: strings.TrimSpace(record.Account.UpstreamAccountID)},
	}, nil
}

func cliProxyDocument(record accountExportRecord) (cliProxyCodexDocument, error) {
	if accountprovider.UsesAPIKey("codex", record.Token) {
		return cliProxyCodexDocument{}, errors.New("CLIProxyAPI Codex auth files require OAuth credentials; configure API keys in CLIProxyAPI's codex-api-key setting")
	}
	if strings.TrimSpace(record.Token.AccessToken) == "" {
		return cliProxyCodexDocument{}, errors.New("access_token is empty")
	}
	if strings.TrimSpace(record.Token.IDTokenRaw) == "" || syntheticCodexIDToken(record.Token.IDTokenRaw) {
		return cliProxyCodexDocument{}, errors.New("CLIProxyAPI export requires a real id_token")
	}
	lastRefresh, expired := "", ""
	if record.Token.LastRefresh > 0 {
		lastRefresh = time.Unix(record.Token.LastRefresh, 0).UTC().Format(time.RFC3339)
	}
	if record.Token.ExpiresAt > 0 {
		expired = time.Unix(record.Token.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	return cliProxyCodexDocument{IDToken: record.Token.IDTokenRaw, AccessToken: record.Token.AccessToken,
		RefreshToken: record.Token.RefreshToken, AccountID: strings.TrimSpace(record.Account.UpstreamAccountID),
		LastRefresh: lastRefresh, Email: record.Account.Email, Type: "codex", Expired: expired}, nil
}

func cliProxyCredentialFilename(record accountExportRecord) string {
	account := record.Account
	email := strings.TrimSpace(account.Email)
	hashID := strings.TrimSpace(account.UpstreamAccountID)
	if hashID != "" {
		digest := sha256.Sum256([]byte(hashID))
		hashID = hex.EncodeToString(digest[:])[:8]
	}
	plan := normalizeCredentialFilenamePart(account.PlanType)
	parts := []string{"codex"}
	if hashID != "" {
		parts = append(parts, hashID)
	}
	parts = append(parts, email)
	if plan != "" {
		parts = append(parts, plan)
	}
	return safeCredentialFilename(strings.Join(parts, "-") + ".json")
}

func normalizeCredentialFilenamePart(value string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(value), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for index := range parts {
		parts[index] = strings.ToLower(strings.TrimSpace(parts[index]))
	}
	return strings.Join(parts, "-")
}

func codexMultiAuthFilename(account storage.Account, at time.Time, index int) string {
	identity := strings.TrimSpace(account.Email)
	if identity == "" {
		identity = firstNonEmpty(account.Label, account.ID, "account-"+strconv.Itoa(index+1))
	}
	return safeCredentialFilename(identity) + "-" + at.Format("20060102-150405") + ".json"
}

func safeCredentialFilename(value string) string {
	var out strings.Builder
	for _, r := range strings.TrimSpace(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' || r == '@' || r == '+' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
		if out.Len() >= 160 {
			break
		}
	}
	safe := strings.Trim(out.String(), ".-_")
	if safe == "" {
		return "account"
	}
	return safe
}

func uniqueCredentialFilename(name string, used map[string]int) string {
	key := strings.ToLower(name)
	used[key]++
	if used[key] == 1 {
		return name
	}
	base := strings.TrimSuffix(name, ".json")
	return fmt.Sprintf("%s-%d.json", base, used[key])
}
