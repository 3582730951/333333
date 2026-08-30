package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/accountprovider"
	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/storage"

	"github.com/google/uuid"
)

const hubVerificationPendingPrefix = "sub2api_hub_verification_pending:"

type sub2APIHubCodexImportRequest struct {
	Content                 interface{}            `json:"content"`
	Contents                []interface{}          `json:"contents"`
	Name                    string                 `json:"name"`
	Notes                   *string                `json:"notes"`
	GroupIDs                []int64                `json:"group_ids"`
	ProxyID                 *int64                 `json:"proxy_id"`
	Concurrency             *int                   `json:"concurrency"`
	Priority                *int                   `json:"priority"`
	RateMultiplier          *float64               `json:"rate_multiplier"`
	LoadFactor              *int                   `json:"load_factor"`
	ExpiresAt               *int64                 `json:"expires_at"`
	AutoPauseOnExpired      *bool                  `json:"auto_pause_on_expired"`
	CredentialExtras        map[string]interface{} `json:"credential_extras"`
	Extra                   map[string]interface{} `json:"extra"`
	UpdateExisting          *bool                  `json:"update_existing"`
	SkipDefaultGroupBind    *bool                  `json:"skip_default_group_bind"`
	ConfirmMixedChannelRisk *bool                  `json:"confirm_mixed_channel_risk"`
}

type sub2APIHubCreateAccountRequest struct {
	Name                 string                 `json:"name"`
	Notes                *string                `json:"notes"`
	Platform             string                 `json:"platform"`
	Type                 string                 `json:"type"`
	Credentials          map[string]interface{} `json:"credentials"`
	Extra                map[string]interface{} `json:"extra"`
	ProxyID              *int64                 `json:"proxy_id"`
	Concurrency          int                    `json:"concurrency"`
	Priority             int                    `json:"priority"`
	RateMultiplier       *float64               `json:"rate_multiplier"`
	LoadFactor           *int                   `json:"load_factor"`
	GroupIDs             []int64                `json:"group_ids"`
	ExpiresAt            *int64                 `json:"expires_at"`
	AutoPauseOnExpired   *bool                  `json:"auto_pause_on_expired"`
	Status               string                 `json:"status"`
	Schedulable          *bool                  `json:"schedulable"`
	SkipDefaultGroupBind *bool                  `json:"skip_default_group_bind"`
}

type sub2APIHubImportItem struct {
	Index             int    `json:"index"`
	Name              string `json:"name,omitempty"`
	Action            string `json:"action"`
	AccountID         int64  `json:"account_id,omitempty"`
	Message           string `json:"message,omitempty"`
	Schedulable       bool   `json:"schedulable"`
	VerificationState string `json:"verification_state"`
}

type sub2APIHubImportMessage struct {
	Index   int    `json:"index"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

type sub2APIHubImportResult struct {
	Total    int                       `json:"total"`
	Created  int                       `json:"created"`
	Updated  int                       `json:"updated"`
	Skipped  int                       `json:"skipped"`
	Failed   int                       `json:"failed"`
	Items    []sub2APIHubImportItem    `json:"items"`
	Warnings []sub2APIHubImportMessage `json:"warnings,omitempty"`
	Errors   []sub2APIHubImportMessage `json:"errors,omitempty"`
}

type sub2APIHubDataImportResult struct {
	ProxyCreated   int                       `json:"proxy_created"`
	ProxyReused    int                       `json:"proxy_reused"`
	ProxyFailed    int                       `json:"proxy_failed"`
	AccountCreated int                       `json:"account_created"`
	AccountUpdated int                       `json:"account_updated,omitempty"`
	AccountSkipped int                       `json:"account_skipped,omitempty"`
	AccountFailed  int                       `json:"account_failed"`
	Errors         []map[string]interface{}  `json:"errors,omitempty"`
	Items          []sub2APIHubImportItem    `json:"items,omitempty"`
	Warnings       []sub2APIHubImportMessage `json:"warnings,omitempty"`
}

type sub2APIHubImportOptions struct {
	Name                 string
	EgressID             string
	ProxyByKey           map[string]string
	UpdateExisting       bool
	SkipDefaultGroupBind bool
	SourceRequest        string
	ConnectionSize       *int
}

type sub2APIHubWriteResult struct {
	Status  int
	Data    interface{}
	Message string
	Reason  string
	Counts  storage.Sub2APIHubImportRun
}

func canonicalHubRequestFingerprint(raw []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value interface{}
	canonical := bytes.TrimSpace(raw)
	if decoder.Decode(&value) == nil {
		if encoded, err := json.Marshal(value); err == nil {
			canonical = encoded
		}
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func hubIdempotencyKey(r *http.Request, connectionID, fingerprint string, now int64) (string, error) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key != "" {
		if len(key) > 200 || strings.ContainsAny(key, "\r\n\x00") {
			return "", errors.New("Idempotency-Key is invalid")
		}
		return "explicit:" + key, nil
	}
	// A bounded ten-minute bucket absorbs automatic connector retries while still
	// allowing an operator to intentionally replay the same update later. Credential
	// identity is a second, durable duplicate barrier across bucket boundaries.
	seed := connectionID + "\x00" + r.URL.Path + "\x00" + fingerprint + "\x00" + strconv.FormatInt(now/600, 10)
	digest := sha256.Sum256([]byte(seed))
	return "auto:" + hex.EncodeToString(digest[:]), nil
}

func (s *Server) executeSub2APIHubWrite(
	w http.ResponseWriter,
	r *http.Request,
	connection storage.Sub2APIHubConnection,
	raw []byte,
	execute func(runID string) sub2APIHubWriteResult,
) {
	now := storage.Now()
	fingerprint := canonicalHubRequestFingerprint(raw)
	idempotencyKey, err := hubIdempotencyKey(r, connection.ID, fingerprint, now)
	if err != nil {
		writeSub2APIHubError(w, http.StatusBadRequest, err.Error(), "idempotency_key_invalid", nil)
		return
	}
	run := storage.Sub2APIHubImportRun{
		ID: uuid.NewString(), ConnectionID: connection.ID, ProtocolRoute: r.URL.Path,
		IdempotencyKey: idempotencyKey, RequestFingerprint: fingerprint, StartedAt: now,
	}
	claimed, existing, err := s.store.ClaimSub2APIHubImportRun(r.Context(), run)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrSub2APIHubIdempotencyReuse):
			writeSub2APIHubError(w, http.StatusConflict, err.Error(), "idempotency_key_reused", nil)
		case errors.Is(err, storage.ErrSub2APIHubIdempotencyBusy):
			w.Header().Set("Retry-After", "1")
			writeSub2APIHubError(w, http.StatusConflict, err.Error(), "idempotency_in_progress", map[string]string{"retry_after": "1"})
		default:
			writeSub2APIHubError(w, http.StatusInternalServerError, "Idempotency storage unavailable", "storage_unavailable", nil)
		}
		return
	}
	if !claimed {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Idempotent-Replay", "true")
		w.WriteHeader(existing.ResponseStatus)
		_, _ = w.Write([]byte(existing.ResponseRedactedJSON))
		return
	}
	result := execute(run.ID)
	if result.Status == 0 {
		result.Status = http.StatusOK
	}
	var envelope sub2APIEnvelope
	if result.Status >= 200 && result.Status < 300 {
		envelope = sub2APIEnvelope{Code: 0, Message: "success", Data: result.Data}
	} else {
		envelope = sub2APIEnvelope{Code: result.Status, Message: result.Message, Reason: result.Reason}
	}
	encoded, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		writeSub2APIHubError(w, http.StatusInternalServerError, "Response encoding failed", "internal_error", nil)
		return
	}
	run.Total = result.Counts.Total
	run.CreatedCount = result.Counts.CreatedCount
	run.UpdatedCount = result.Counts.UpdatedCount
	run.SkippedCount = result.Counts.SkippedCount
	run.FailedCount = result.Counts.FailedCount
	run.ResponseStatus = result.Status
	run.ResponseRedactedJSON = string(encoded)
	run.FinishedAt = storage.Now()
	if err = s.store.FinishSub2APIHubImportRun(r.Context(), run); err != nil {
		writeSub2APIHubError(w, http.StatusInternalServerError, "Import result could not be settled", "idempotency_settlement_failed", nil)
		return
	}
	s.enqueueAudit(storage.AuditLogRow{Action: "sub2api_hub_import", State: "settled", Reason: "idempotent",
		Detail: fmt.Sprintf("connection_id=%s route=%s total=%d created=%d updated=%d skipped=%d failed=%d request_hash=%s",
			connection.ID, r.URL.Path, run.Total, run.CreatedCount, run.UpdatedCount, run.SkippedCount, run.FailedCount, fingerprint[:16]), CreatedAt: storage.Now()})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(result.Status)
	_, _ = w.Write(encoded)
}

func hubContentString(value interface{}) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "", nil
	case string:
		return typed, nil
	default:
		raw, err := json.Marshal(typed)
		return string(raw), err
	}
}

func parseSub2APIHubImportChunk(raw string) ([]authparse.ImportEntry, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("import content is empty")
	}
	if len(trimmed) > 1 && trimmed[0] == '"' {
		var decoded string
		if json.Unmarshal([]byte(trimmed), &decoded) == nil {
			trimmed = strings.TrimSpace(decoded)
		}
	}
	if doc, err := authparse.ParseImportDocument([]byte(trimmed)); err == nil {
		return doc.Entries, nil
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > 1 {
		entries := []authparse.ImportEntry{}
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			doc, err := authparse.ParseImportDocument([]byte(strings.TrimSpace(line)))
			if err != nil {
				encoded, _ := json.Marshal(map[string]string{"access_token": strings.TrimSpace(line)})
				doc, err = authparse.ParseImportDocument(encoded)
			}
			if err != nil {
				return nil, err
			}
			entries = append(entries, doc.Entries...)
		}
		if len(entries) > 0 {
			return entries, nil
		}
	}
	encoded, _ := json.Marshal(map[string]string{"access_token": trimmed})
	doc, err := authparse.ParseImportDocument(encoded)
	if err != nil {
		return nil, err
	}
	return doc.Entries, nil
}

func parseSub2APIHubCodexEntries(req sub2APIHubCodexImportRequest, max int) ([]authparse.ImportEntry, error) {
	values := make([]interface{}, 0, len(req.Contents)+1)
	if req.Content != nil {
		values = append(values, req.Content)
	}
	values = append(values, req.Contents...)
	entries := []authparse.ImportEntry{}
	for _, value := range values {
		raw, err := hubContentString(value)
		if err != nil {
			return nil, err
		}
		parsed, err := parseSub2APIHubImportChunk(raw)
		if err != nil {
			return nil, err
		}
		for _, entry := range parsed {
			entry.Index = len(entries) + 1
			entries = append(entries, entry)
			if len(entries) > max {
				return nil, fmt.Errorf("import contains more than %d accounts", max)
			}
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("content or contents must contain at least one account")
	}
	return entries, nil
}

func (s *Server) sub2APIHubCredentialFingerprint(label, material string) (string, error) {
	if len(s.identitySecretCached) < 16 || strings.TrimSpace(material) == "" {
		return "", errors.New("credential identity unavailable")
	}
	mac := hmac.New(sha256.New, s.identitySecretCached)
	_, _ = mac.Write([]byte("codex-pool/sub2api-hub-credential/v1\x00" + label + "\x00"))
	_, _ = mac.Write([]byte(material))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func hubCredentialMaterial(parsed authparse.ParsedAuth) (string, bool) {
	for _, candidate := range []struct{ kind, value string }{
		{"refresh_token", parsed.RefreshToken},
		{"access_token", parsed.AccessToken},
		{"api_key", parsed.OpenAIAPIKey},
		{"agent_private_key", parsed.AgentPrivateKey},
	} {
		if strings.TrimSpace(candidate.value) != "" {
			return candidate.kind + "\x00" + strings.TrimSpace(candidate.value), true
		}
	}
	return "", false
}

func mergeHubToken(current, incoming storage.AccountToken) storage.AccountToken {
	if incoming.AccessToken == "" {
		incoming.AccessToken = current.AccessToken
	}
	if incoming.RefreshToken == "" {
		incoming.RefreshToken = current.RefreshToken
	}
	if incoming.OpenAIAPIKey == "" {
		incoming.OpenAIAPIKey = current.OpenAIAPIKey
	}
	if incoming.IDTokenRaw == "" {
		incoming.IDTokenRaw = current.IDTokenRaw
	}
	if incoming.AgentRuntimeID == "" {
		incoming.AgentRuntimeID = current.AgentRuntimeID
	}
	if incoming.AgentPrivateKey == "" {
		incoming.AgentPrivateKey = current.AgentPrivateKey
	}
	if incoming.AgentTaskID == "" {
		incoming.AgentTaskID = current.AgentTaskID
	}
	if incoming.ExpiresAt == 0 {
		incoming.ExpiresAt = current.ExpiresAt
	}
	if incoming.Scopes == "" {
		incoming.Scopes = current.Scopes
	}
	if incoming.OAuthRateLimitTier == "" {
		incoming.OAuthRateLimitTier = current.OAuthRateLimitTier
	}
	incoming.CreatedAt = current.CreatedAt
	return incoming
}

func (s *Server) resolveSub2APIHubProxyID(ctx context.Context, connection storage.Sub2APIHubConnection, wireID *int64) (string, error) {
	if wireID == nil || *wireID == 0 {
		return "", nil
	}
	for _, id := range connection.AllowedProxyIDs {
		if hubNumericID("sub2api-proxy", id) == *wireID {
			if _, err := s.store.GetEgressProfile(ctx, id); err != nil {
				return "", errors.New("configured proxy is unavailable")
			}
			return id, nil
		}
	}
	return "", errors.New("proxy_id is outside this Hub connection's allowed proxy scope")
}

func (s *Server) importSub2APIHubEntries(
	ctx context.Context,
	connection storage.Sub2APIHubConnection,
	entries []authparse.ImportEntry,
	options sub2APIHubImportOptions,
) sub2APIHubImportResult {
	result := sub2APIHubImportResult{Total: len(entries), Items: make([]sub2APIHubImportItem, 0, len(entries))}
	connectionSize := 0
	if options.ConnectionSize != nil {
		connectionSize = *options.ConnectionSize
	} else {
		connectionSize, _ = s.store.CountSub2APIHubAccounts(ctx, connection.ID)
	}
	for offset, entry := range entries {
		if entry.Index <= 0 {
			entry.Index = offset + 1
		}
		item := sub2APIHubImportItem{Index: entry.Index, Name: entry.Name, Action: "failed", VerificationState: "rejected"}
		fail := func(err error) {
			item.Message = err.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			result.Errors = append(result.Errors, sub2APIHubImportMessage{Index: item.Index, Name: item.Name, Message: item.Message})
		}
		if entry.Err != nil {
			fail(entry.Err)
			continue
		}
		// Resolve every proxy reference before the first account/token write. A
		// rejected or out-of-scope proxy must never leave a partially imported
		// account behind. Hub data proxies are mappings to administrator-approved
		// local egress profiles only; the inbound payload is never dialed or used to
		// create a new network destination.
		egressID := options.EgressID
		if entry.ProxyKey != "" {
			mapped, found := options.ProxyByKey[entry.ProxyKey]
			if !found {
				fail(errors.New("referenced proxy is not allowed or was not imported"))
				continue
			}
			egressID = mapped
		}
		parsed := entry.Parsed
		token := accountTokenFromParsed(parsed, parsed.RefreshToken)
		provider := accountprovider.EffectiveProvider(parsed.Provider, token, true)
		if provider != "codex" {
			fail(fmt.Errorf("provider %q is outside this connection's allowlist", provider))
			continue
		}
		if accountprovider.UsesAPIKey(provider, token) {
			fail(errors.New("OpenAI API-key accounts require the cost-confirmed native importer and are not accepted by Hub v1"))
			continue
		}
		material, ok := hubCredentialMaterial(parsed)
		if !ok {
			fail(errors.New("account has no strong credential identity"))
			continue
		}
		credentialFingerprint, err := s.sub2APIHubCredentialFingerprint("global", material)
		if err != nil {
			fail(err)
			continue
		}
		externalIdentity, err := s.sub2APIHubCredentialFingerprint(connection.ID, material)
		if err != nil {
			fail(err)
			continue
		}
		owned, ownedFound, err := s.store.FindSub2APIHubAccountByExternalIdentity(ctx, connection.ID, externalIdentity)
		if err != nil {
			fail(err)
			continue
		}
		if !ownedFound {
			cross, crossErr := s.store.FindSub2APIHubAccountsByCredential(ctx, credentialFingerprint)
			if crossErr != nil {
				fail(crossErr)
				continue
			}
			crossConnection := false
			for _, candidate := range cross {
				if candidate.ConnectionID != connection.ID {
					crossConnection = true
					break
				}
			}
			if crossConnection {
				fail(errors.New("credential is already owned by another Hub connection"))
				continue
			}
		}
		accountID := parsed.AccountID
		var account storage.Account
		existing := false
		if ownedFound {
			accountID = owned.LocalAccountID
			account, err = s.store.GetAccount(ctx, accountID)
			existing = err == nil
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				fail(err)
				continue
			}
		} else if accountID != "" {
			account, err = s.store.GetAccount(ctx, accountID)
			existing = err == nil
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				fail(err)
				continue
			}
			if existing && connection.DuplicatePolicy != "reuse_unowned_local" && account.QuarantineReason != hubVerificationPendingPrefix+connection.ID {
				fail(errors.New("matching local account is not owned by this Hub connection"))
				continue
			}
		}
		if existing && !options.UpdateExisting {
			item.Action, item.Message = "skipped", "matching account already exists"
			item.AccountID = hubNumericID("sub2api-account", account.ID)
			item.Schedulable = account.Status == "active" && account.QuarantineUntil <= storage.Now()
			item.VerificationState = map[bool]string{true: "active", false: "quarantine"}[item.Schedulable]
			result.Skipped++
			result.Items = append(result.Items, item)
			continue
		}
		if !existing && connectionSize >= connection.MaxAccounts {
			fail(errors.New("Hub connection account capacity reached"))
			continue
		}
		if strings.TrimSpace(accountID) == "" {
			fail(errors.New("account identity could not be derived from credentials"))
			continue
		}
		label := strings.TrimSpace(entry.Name)
		if label == "" {
			label = strings.TrimSpace(options.Name)
		}
		if label == "" {
			label = firstNonEmpty(parsed.Name, "Hub "+accountExportCode(accountID))
		}
		if existing {
			current, tokenErr := s.store.GetToken(ctx, account.ID)
			if tokenErr != nil {
				fail(tokenErr)
				continue
			}
			token = mergeHubToken(current, token)
			account.Label = label
			if parsed.UpstreamAccountID != "" {
				account.UpstreamAccountID = parsed.UpstreamAccountID
			}
			if parsed.ChatGPTUserID != "" {
				account.ChatGPTUserID = parsed.ChatGPTUserID
			}
			if parsed.Email != "" {
				account.Email = parsed.Email
			}
			if parsed.PlanType != "" {
				account.PlanType = parsed.PlanType
			}
		} else {
			account = storage.Account{ID: accountID, Label: label, UpstreamAccountID: parsed.UpstreamAccountID,
				ChatGPTUserID: parsed.ChatGPTUserID, Email: parsed.Email, PlanType: parsed.PlanType,
				Provider: "codex", Status: "active", IsFedramp: parsed.IsFedramp}
		}
		account.GroupName = connection.TargetGroupID
		account.Provider = "codex"
		account.Status = "active"
		account.QuarantineUntil = kiroSuspensionQuarantineUntil
		account.QuarantineReason = hubVerificationPendingPrefix + connection.ID
		token.AuthMethod = accountprovider.EffectiveAuthMethod("codex", token)
		if err = s.store.UpsertAccount(ctx, account, token); err != nil {
			fail(err)
			continue
		}
		if !options.SkipDefaultGroupBind {
			if err = s.store.SetAccountGroup(ctx, account.ID, connection.TargetGroupID); err != nil {
				fail(err)
				continue
			}
		}
		if err = s.bindImportedAccountPrimaryEgress(ctx, account.ID, egressID); err != nil {
			fail(err)
			continue
		}
		if err = s.store.SetAccountQuarantine(ctx, account.ID, kiroSuspensionQuarantineUntil, hubVerificationPendingPrefix+connection.ID); err != nil {
			fail(err)
			continue
		}
		if err = s.seedImportedAccountCapabilities(ctx, account); err != nil {
			fail(err)
			continue
		}
		if err = s.store.UpsertSub2APIHubAccount(ctx, storage.Sub2APIHubAccount{
			ConnectionID: connection.ID, LocalAccountID: account.ID, ExternalIdentityHash: externalIdentity,
			CredentialFingerprint: credentialFingerprint, SourceRequestID: options.SourceRequest, State: "quarantine",
		}); err != nil {
			fail(err)
			continue
		}
		item.AccountID = hubNumericID("sub2api-account", account.ID)
		item.Schedulable = false
		item.VerificationState = "pending"
		if existing {
			item.Action = "updated"
			result.Updated++
		} else {
			item.Action = "created"
			result.Created++
			connectionSize++
		}
		for _, warning := range entry.Warnings {
			result.Warnings = append(result.Warnings, sub2APIHubImportMessage{Index: item.Index, Name: item.Name, Message: warning})
		}
		result.Items = append(result.Items, item)
		s.verifySub2APIHubAccountAsync(connection, account)
	}
	if options.ConnectionSize != nil {
		*options.ConnectionSize = connectionSize
	}
	if s.scheduler != nil && (result.Created > 0 || result.Updated > 0) {
		s.scheduler.InvalidateAccountCache()
	}
	return result
}

func (s *Server) verifySub2APIHubAccountAsync(connection storage.Sub2APIHubConnection, account storage.Account) {
	s.launchRuntimeTask("sub2api-hub-verify", 2*s.cfg.RequestTimeout(), func(ctx context.Context) {
		caps, err := s.probeAccountModels(ctx, account)
		if err != nil || len(caps) == 0 {
			_ = s.store.SetSub2APIHubAccountState(ctx, connection.ID, account.ID, "verification_failed")
			_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{AccountID: account.ID, AccountLabel: account.Label,
				Action: "sub2api_hub_verify", State: "quarantined", Reason: "model_discovery_failed", Detail: "connection_id=" + connection.ID})
			return
		}
		current, getErr := s.store.GetAccount(ctx, account.ID)
		if getErr != nil || current.QuarantineReason != hubVerificationPendingPrefix+connection.ID {
			return
		}
		if err = s.store.SetAccountQuarantine(ctx, account.ID, 0, ""); err != nil {
			return
		}
		_ = s.store.SetAccountStatus(ctx, account.ID, "active")
		_ = s.store.SetSub2APIHubAccountState(ctx, connection.ID, account.ID, "active")
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{AccountID: account.ID, AccountLabel: account.Label,
			Action: "sub2api_hub_verify", State: "active", Reason: "model_discovery_succeeded", Detail: "connection_id=" + connection.ID})
		if s.scheduler != nil {
			s.scheduler.NotifyStateChanged()
		}
	})
}

func (s *Server) sub2APIHubAccounts(w http.ResponseWriter, r *http.Request) {
	connection, done, ok := s.authorizeSub2APIHub(w, r)
	if !ok {
		return
	}
	defer done()
	switch r.Method {
	case http.MethodGet:
		s.sub2APIHubListAccounts(w, r, connection)
	case http.MethodPost:
		raw, err := requestBodyBytes(r, sub2APIHubBodyLimit)
		if err != nil {
			writeSub2APIHubError(w, http.StatusBadRequest, err.Error(), "invalid_request", nil)
			return
		}
		s.executeSub2APIHubWrite(w, r, connection, raw, func(runID string) sub2APIHubWriteResult {
			var req sub2APIHubCreateAccountRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return sub2APIHubWriteResult{Status: http.StatusBadRequest, Message: "Invalid request: " + err.Error(), Reason: "invalid_request"}
			}
			entry, entryErr := hubCreateRequestEntry(req)
			if entryErr != nil {
				return sub2APIHubWriteResult{Status: http.StatusBadRequest, Message: entryErr.Error(), Reason: "invalid_account"}
			}
			egressID, proxyErr := s.resolveSub2APIHubProxyID(r.Context(), connection, req.ProxyID)
			if proxyErr != nil {
				return sub2APIHubWriteResult{Status: http.StatusForbidden, Message: proxyErr.Error(), Reason: "proxy_scope_denied"}
			}
			result := s.importSub2APIHubEntries(r.Context(), connection, []authparse.ImportEntry{entry}, sub2APIHubImportOptions{
				Name: req.Name, EgressID: egressID, UpdateExisting: false,
				SkipDefaultGroupBind: req.SkipDefaultGroupBind != nil && *req.SkipDefaultGroupBind, SourceRequest: runID,
			})
			counts := hubRunCounts(result)
			if result.Failed > 0 {
				return sub2APIHubWriteResult{Status: http.StatusUnprocessableEntity, Message: result.Errors[0].Message, Reason: "account_import_failed", Counts: counts}
			}
			if result.Skipped > 0 {
				return sub2APIHubWriteResult{Status: http.StatusConflict, Message: "matching account already exists", Reason: "account_duplicate", Counts: counts}
			}
			account, found, _ := s.findSub2APIHubAccountByWireID(r.Context(), connection, result.Items[0].AccountID, true)
			if !found {
				return sub2APIHubWriteResult{Status: http.StatusInternalServerError, Message: "created account could not be read back", Reason: "storage_unavailable", Counts: counts}
			}
			return sub2APIHubWriteResult{Status: http.StatusCreated, Data: s.sub2APIHubAccountDTO(r.Context(), connection, account), Counts: counts}
		})
	default:
		writeSub2APIHubMethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func hubCreateRequestEntry(req sub2APIHubCreateAccountRequest) (authparse.ImportEntry, error) {
	if strings.TrimSpace(req.Name) == "" {
		return authparse.ImportEntry{}, errors.New("name is required")
	}
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if platform != "openai" && platform != "codex" {
		return authparse.ImportEntry{}, errors.New("platform must be openai")
	}
	if !strings.EqualFold(strings.TrimSpace(req.Type), "oauth") {
		return authparse.ImportEntry{}, errors.New("Hub v1 currently accepts only oauth accounts")
	}
	if len(req.Credentials) == 0 {
		return authparse.ImportEntry{}, errors.New("credentials are required")
	}
	raw, _ := json.Marshal(req.Credentials)
	parsed, err := authparse.ParseAuthJSON(raw)
	return authparse.ImportEntry{Index: 1, Name: strings.TrimSpace(req.Name), Parsed: parsed, Err: err}, err
}

func hubRunCounts(result sub2APIHubImportResult) storage.Sub2APIHubImportRun {
	return storage.Sub2APIHubImportRun{Total: result.Total, CreatedCount: result.Created, UpdatedCount: result.Updated, SkippedCount: result.Skipped, FailedCount: result.Failed}
}

func (s *Server) sub2APIHubAccountAction(w http.ResponseWriter, r *http.Request) {
	connection, done, ok := s.authorizeSub2APIHub(w, r)
	if !ok {
		return
	}
	defer done()
	remainder := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/accounts/"), "/")
	if remainder == "import/codex-session" {
		s.sub2APIHubImportCodexSession(w, r, connection)
		return
	}
	if remainder == "data" {
		s.sub2APIHubImportData(w, r, connection)
		return
	}
	parts := strings.Split(remainder, "/")
	wireID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || wireID <= 0 {
		writeSub2APIHubError(w, http.StatusBadRequest, "Invalid account ID", "invalid_account_id", nil)
		return
	}
	account, found, err := s.findSub2APIHubAccountByWireID(r.Context(), connection, wireID, true)
	if err != nil {
		writeSub2APIHubError(w, http.StatusInternalServerError, "Account lookup failed", "storage_unavailable", nil)
		return
	}
	if !found {
		writeSub2APIHubError(w, http.StatusNotFound, "Account not found", "account_not_found", nil)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			writeSub2APIHubSuccess(w, http.StatusOK, s.sub2APIHubAccountDTO(r.Context(), connection, account))
		case http.MethodPut:
			s.sub2APIHubUpdateAccount(w, r, connection, account)
		default:
			writeSub2APIHubMethodNotAllowed(w, http.MethodGet, http.MethodPut)
		}
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		writeSub2APIHubMethodNotAllowed(w, http.MethodPost)
		return
	}
	switch parts[1] {
	case "refresh":
		_ = s.store.SetAccountStatus(r.Context(), account.ID, "active")
		_ = s.store.SetAccountQuarantine(r.Context(), account.ID, kiroSuspensionQuarantineUntil, hubVerificationPendingPrefix+connection.ID)
		_ = s.store.SetSub2APIHubAccountState(r.Context(), connection.ID, account.ID, "quarantine")
		s.verifySub2APIHubAccountAsync(connection, account)
		writeSub2APIHubSuccess(w, http.StatusAccepted, map[string]interface{}{"id": wireID, "schedulable": false, "verification_state": "pending"})
	case "test":
		token, tokenErr := s.store.GetToken(r.Context(), account.ID)
		if tokenErr != nil {
			writeSub2APIHubError(w, http.StatusInternalServerError, "Credential unavailable", "storage_unavailable", nil)
			return
		}
		probe := s.probeAccountLiveness(r.Context(), account, token)
		if probe.Alive && probe.Ready {
			_ = s.store.SetAccountQuarantine(r.Context(), account.ID, 0, "")
			_ = s.store.SetAccountStatus(r.Context(), account.ID, "active")
			_ = s.store.SetSub2APIHubAccountState(r.Context(), connection.ID, account.ID, "active")
		}
		writeSub2APIHubSuccess(w, http.StatusOK, map[string]interface{}{
			"id": wireID, "alive": probe.Alive, "ready": probe.Ready, "state": probe.Verdict.State,
			"reason": probe.Verdict.Reason, "http_status": probe.Status, "schedulable": probe.Alive && probe.Ready,
		})
	case "schedulable":
		var req struct {
			Schedulable *bool `json:"schedulable"`
		}
		if err = decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil || req.Schedulable == nil {
			writeSub2APIHubError(w, http.StatusBadRequest, "schedulable is required", "invalid_request", nil)
			return
		}
		if !*req.Schedulable {
			_ = s.store.SetAccountStatus(r.Context(), account.ID, "disabled")
			_ = s.store.SetSub2APIHubAccountState(r.Context(), connection.ID, account.ID, "disabled")
			writeSub2APIHubSuccess(w, http.StatusOK, map[string]interface{}{"id": wireID, "schedulable": false, "verification_state": "disabled"})
			return
		}
		_ = s.store.SetAccountStatus(r.Context(), account.ID, "active")
		_ = s.store.SetAccountQuarantine(r.Context(), account.ID, kiroSuspensionQuarantineUntil, hubVerificationPendingPrefix+connection.ID)
		_ = s.store.SetSub2APIHubAccountState(r.Context(), connection.ID, account.ID, "quarantine")
		s.verifySub2APIHubAccountAsync(connection, account)
		writeSub2APIHubSuccess(w, http.StatusAccepted, map[string]interface{}{"id": wireID, "schedulable": false, "verification_state": "pending"})
	default:
		writeSub2APIHubError(w, http.StatusNotFound, "Account action not found", "route_not_found", nil)
	}
}

func (s *Server) sub2APIHubUpdateAccount(w http.ResponseWriter, r *http.Request, connection storage.Sub2APIHubConnection, account storage.Account) {
	raw, err := requestBodyBytes(r, sub2APIHubBodyLimit)
	if err != nil {
		writeSub2APIHubError(w, http.StatusBadRequest, err.Error(), "invalid_request", nil)
		return
	}
	s.executeSub2APIHubWrite(w, r, connection, raw, func(runID string) sub2APIHubWriteResult {
		var req sub2APIHubCreateAccountRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return sub2APIHubWriteResult{Status: http.StatusBadRequest, Message: "Invalid request: " + err.Error(), Reason: "invalid_request"}
		}
		if strings.TrimSpace(req.Name) != "" {
			account.Label = strings.TrimSpace(req.Name)
		}
		token, tokenErr := s.store.GetToken(r.Context(), account.ID)
		if tokenErr != nil {
			return sub2APIHubWriteResult{Status: http.StatusInternalServerError, Message: "Credential unavailable", Reason: "storage_unavailable"}
		}
		mapping, mappingFound, mappingErr := s.store.GetSub2APIHubAccount(r.Context(), connection.ID, account.ID)
		if mappingErr != nil || !mappingFound {
			return sub2APIHubWriteResult{Status: http.StatusForbidden, Message: "Account is not owned by this Hub connection", Reason: "account_scope_denied"}
		}
		credentialFingerprint := mapping.CredentialFingerprint
		externalIdentity := mapping.ExternalIdentityHash
		if len(req.Credentials) > 0 {
			encoded, _ := json.Marshal(req.Credentials)
			parsed, parseErr := authparse.ParseAuthJSON(encoded)
			if parseErr != nil {
				return sub2APIHubWriteResult{Status: http.StatusBadRequest, Message: parseErr.Error(), Reason: "invalid_credentials"}
			}
			incoming := accountTokenFromParsed(parsed, parsed.RefreshToken)
			if accountprovider.UsesAPIKey("codex", incoming) {
				return sub2APIHubWriteResult{Status: http.StatusBadRequest, Message: "OpenAI API-key accounts are not accepted by Hub v1", Reason: "cost_confirmation_required"}
			}
			token = mergeHubToken(token, incoming)
			material := ""
			switch {
			case strings.TrimSpace(token.RefreshToken) != "":
				material = "refresh_token\x00" + strings.TrimSpace(token.RefreshToken)
			case strings.TrimSpace(token.AccessToken) != "":
				material = "access_token\x00" + strings.TrimSpace(token.AccessToken)
			case strings.TrimSpace(token.OpenAIAPIKey) != "":
				material = "api_key\x00" + strings.TrimSpace(token.OpenAIAPIKey)
			case strings.TrimSpace(token.AgentPrivateKey) != "":
				material = "agent_private_key\x00" + strings.TrimSpace(token.AgentPrivateKey)
			}
			if material == "" {
				return sub2APIHubWriteResult{Status: http.StatusBadRequest, Message: "Updated credentials have no strong identity", Reason: "invalid_credentials"}
			}
			credentialFingerprint, tokenErr = s.sub2APIHubCredentialFingerprint("global", material)
			if tokenErr != nil {
				return sub2APIHubWriteResult{Status: http.StatusServiceUnavailable, Message: "Credential identity unavailable", Reason: "storage_unavailable"}
			}
			externalIdentity, tokenErr = s.sub2APIHubCredentialFingerprint(connection.ID, material)
			if tokenErr != nil {
				return sub2APIHubWriteResult{Status: http.StatusServiceUnavailable, Message: "Credential identity unavailable", Reason: "storage_unavailable"}
			}
			matches, lookupErr := s.store.FindSub2APIHubAccountsByCredential(r.Context(), credentialFingerprint)
			if lookupErr != nil {
				return sub2APIHubWriteResult{Status: http.StatusInternalServerError, Message: "Credential ownership lookup failed", Reason: "storage_unavailable"}
			}
			for _, match := range matches {
				if match.ConnectionID != connection.ID || match.LocalAccountID != account.ID {
					return sub2APIHubWriteResult{Status: http.StatusConflict, Message: "Credential is already owned by another Hub account", Reason: "credential_scope_conflict"}
				}
			}
			if owner, found, lookupErr := s.store.FindSub2APIHubAccountByExternalIdentity(r.Context(), connection.ID, externalIdentity); lookupErr != nil {
				return sub2APIHubWriteResult{Status: http.StatusInternalServerError, Message: "Credential ownership lookup failed", Reason: "storage_unavailable"}
			} else if found && owner.LocalAccountID != account.ID {
				return sub2APIHubWriteResult{Status: http.StatusConflict, Message: "Credential identity belongs to another account in this connection", Reason: "credential_scope_conflict"}
			}
			if parsed.Email != "" {
				account.Email = parsed.Email
			}
			if parsed.PlanType != "" {
				account.PlanType = parsed.PlanType
			}
			if parsed.UpstreamAccountID != "" {
				account.UpstreamAccountID = parsed.UpstreamAccountID
			}
		}
		egressID, proxyErr := s.resolveSub2APIHubProxyID(r.Context(), connection, req.ProxyID)
		if proxyErr != nil {
			return sub2APIHubWriteResult{Status: http.StatusForbidden, Message: proxyErr.Error(), Reason: "proxy_scope_denied"}
		}
		account.GroupName = connection.TargetGroupID
		account.Status = "active"
		if strings.EqualFold(req.Status, "inactive") || (req.Schedulable != nil && !*req.Schedulable) {
			account.Status = "disabled"
		}
		if err := s.store.UpsertAccount(r.Context(), account, token); err != nil {
			return sub2APIHubWriteResult{Status: http.StatusInternalServerError, Message: "Account update failed", Reason: "storage_unavailable"}
		}
		_ = s.store.SetAccountGroup(r.Context(), account.ID, connection.TargetGroupID)
		if req.ProxyID != nil {
			_ = s.bindImportedAccountPrimaryEgress(r.Context(), account.ID, egressID)
		}
		state := "disabled"
		if account.Status == "active" {
			_ = s.store.SetAccountQuarantine(r.Context(), account.ID, kiroSuspensionQuarantineUntil, hubVerificationPendingPrefix+connection.ID)
			state = "quarantine"
			s.verifySub2APIHubAccountAsync(connection, account)
		}
		mapping.ExternalIdentityHash = externalIdentity
		mapping.CredentialFingerprint = credentialFingerprint
		mapping.SourceRequestID = runID
		mapping.State = state
		if err := s.store.UpsertSub2APIHubAccount(r.Context(), mapping); err != nil {
			return sub2APIHubWriteResult{Status: http.StatusConflict, Message: "Credential ownership could not be updated", Reason: "credential_scope_conflict"}
		}
		updated, _ := s.store.GetAccount(r.Context(), account.ID)
		counts := storage.Sub2APIHubImportRun{Total: 1, UpdatedCount: 1}
		_ = runID
		return sub2APIHubWriteResult{Status: http.StatusOK, Data: s.sub2APIHubAccountDTO(r.Context(), connection, updated), Counts: counts}
	})
}

func (s *Server) sub2APIHubImportCodexSession(w http.ResponseWriter, r *http.Request, connection storage.Sub2APIHubConnection) {
	if r.Method != http.MethodPost {
		writeSub2APIHubMethodNotAllowed(w, http.MethodPost)
		return
	}
	raw, err := requestBodyBytes(r, sub2APIHubBodyLimit)
	if err != nil {
		writeSub2APIHubError(w, http.StatusBadRequest, err.Error(), "invalid_request", nil)
		return
	}
	s.executeSub2APIHubWrite(w, r, connection, raw, func(runID string) sub2APIHubWriteResult {
		var req sub2APIHubCodexImportRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return sub2APIHubWriteResult{Status: http.StatusBadRequest, Message: "Invalid request: " + err.Error(), Reason: "invalid_request"}
		}
		entries, parseErr := parseSub2APIHubCodexEntries(req, connection.MaxImportBatch)
		if parseErr != nil {
			return sub2APIHubWriteResult{Status: http.StatusBadRequest, Message: parseErr.Error(), Reason: "invalid_content"}
		}
		egressID, proxyErr := s.resolveSub2APIHubProxyID(r.Context(), connection, req.ProxyID)
		if proxyErr != nil {
			return sub2APIHubWriteResult{Status: http.StatusForbidden, Message: proxyErr.Error(), Reason: "proxy_scope_denied"}
		}
		updateExisting := true
		if req.UpdateExisting != nil {
			updateExisting = *req.UpdateExisting
		}
		result := s.importSub2APIHubEntries(r.Context(), connection, entries, sub2APIHubImportOptions{
			Name: req.Name, EgressID: egressID, UpdateExisting: updateExisting,
			SkipDefaultGroupBind: req.SkipDefaultGroupBind != nil && *req.SkipDefaultGroupBind, SourceRequest: runID,
		})
		return sub2APIHubWriteResult{Status: http.StatusOK, Data: result, Counts: hubRunCounts(result)}
	})
}

func (s *Server) allowedSub2APIHubDataProxies(ctx context.Context, connection storage.Sub2APIHubConnection, proxies []authparse.Sub2APIProxy) (map[string]string, int, []map[string]interface{}) {
	allowedProfiles := map[string]storage.EgressProfile{}
	for _, id := range connection.AllowedProxyIDs {
		if profile, err := s.store.GetEgressProfile(ctx, id); err == nil {
			allowedProfiles[strings.ToLower(profile.Type)+"\x00"+profile.Endpoint] = profile
		}
	}
	resolved := map[string]string{}
	errorsOut := []map[string]interface{}{}
	reused := 0
	for _, proxy := range proxies {
		key := sub2APIProxyKey(proxy)
		profile, err := sub2APIEgressProfile(proxy, key)
		if err != nil {
			errorsOut = append(errorsOut, map[string]interface{}{"kind": "proxy", "name": proxy.Name, "proxy_key": safeSub2APIProxyKey(key), "message": err.Error()})
			continue
		}
		allowed, ok := allowedProfiles[strings.ToLower(profile.Type)+"\x00"+profile.Endpoint]
		if !ok {
			errorsOut = append(errorsOut, map[string]interface{}{"kind": "proxy", "name": proxy.Name, "proxy_key": safeSub2APIProxyKey(key), "message": "proxy is outside this Hub connection's allowed proxy scope"})
			continue
		}
		resolved[key] = allowed.ID
		reused++
	}
	return resolved, reused, errorsOut
}

func (s *Server) sub2APIHubImportData(w http.ResponseWriter, r *http.Request, connection storage.Sub2APIHubConnection) {
	if r.Method != http.MethodPost {
		writeSub2APIHubMethodNotAllowed(w, http.MethodPost)
		return
	}
	raw, err := requestBodyBytes(r, sub2APIHubBodyLimit)
	if err != nil {
		writeSub2APIHubError(w, http.StatusBadRequest, err.Error(), "invalid_request", nil)
		return
	}
	s.executeSub2APIHubWrite(w, r, connection, raw, func(runID string) sub2APIHubWriteResult {
		var req struct {
			Data                 json.RawMessage `json:"data"`
			SkipDefaultGroupBind *bool           `json:"skip_default_group_bind"`
		}
		if err := json.Unmarshal(raw, &req); err != nil || len(req.Data) == 0 {
			return sub2APIHubWriteResult{Status: http.StatusBadRequest, Message: "Invalid request: data is required", Reason: "invalid_request"}
		}
		doc, parseErr := authparse.ParseImportDocument(req.Data)
		if parseErr != nil {
			return sub2APIHubWriteResult{Status: http.StatusBadRequest, Message: parseErr.Error(), Reason: "invalid_sub2api_data"}
		}
		if doc.Format != authparse.ImportFormatSub2API {
			return sub2APIHubWriteResult{Status: http.StatusBadRequest, Message: "data must use Sub2API data v1", Reason: "invalid_sub2api_data"}
		}
		if len(doc.Entries) > connection.MaxImportBatch {
			return sub2APIHubWriteResult{Status: http.StatusRequestEntityTooLarge, Message: fmt.Sprintf("import contains more than %d accounts", connection.MaxImportBatch), Reason: "batch_too_large"}
		}
		proxyMap, reused, proxyErrors := s.allowedSub2APIHubDataProxies(r.Context(), connection, doc.Proxies)
		result := s.importSub2APIHubEntries(r.Context(), connection, doc.Entries, sub2APIHubImportOptions{
			ProxyByKey: proxyMap, UpdateExisting: true,
			SkipDefaultGroupBind: req.SkipDefaultGroupBind != nil && *req.SkipDefaultGroupBind, SourceRequest: runID,
		})
		data := sub2APIHubDataImportResult{
			ProxyCreated: 0, ProxyReused: reused, ProxyFailed: len(proxyErrors), AccountCreated: result.Created,
			AccountUpdated: result.Updated, AccountSkipped: result.Skipped, AccountFailed: result.Failed,
			Errors: proxyErrors, Items: result.Items, Warnings: result.Warnings,
		}
		for _, item := range result.Errors {
			data.Errors = append(data.Errors, map[string]interface{}{"kind": "account", "name": item.Name, "message": item.Message})
		}
		counts := hubRunCounts(result)
		counts.FailedCount += len(proxyErrors)
		return sub2APIHubWriteResult{Status: http.StatusOK, Data: data, Counts: counts}
	})
}

func (s *Server) sub2APIHubInventory(ctx context.Context, connection storage.Sub2APIHubConnection) ([]storage.Account, error) {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	if connection.InventoryScope == "target_group" {
		out := make([]storage.Account, 0, len(accounts))
		for _, account := range accounts {
			if account.GroupName == connection.TargetGroupID {
				out = append(out, account)
			}
		}
		return out, nil
	}
	ids, err := s.store.ListSub2APIHubAccountIDs(ctx, connection.ID)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		allowed[id] = struct{}{}
	}
	out := make([]storage.Account, 0, len(ids))
	for _, account := range accounts {
		if _, ok := allowed[account.ID]; ok {
			out = append(out, account)
		}
	}
	return out, nil
}

func (s *Server) findSub2APIHubAccountByWireID(ctx context.Context, connection storage.Sub2APIHubConnection, wireID int64, write bool) (storage.Account, bool, error) {
	accounts, err := s.sub2APIHubInventory(ctx, connection)
	if err != nil {
		return storage.Account{}, false, err
	}
	for _, account := range accounts {
		if hubNumericID("sub2api-account", account.ID) != wireID {
			continue
		}
		if write {
			if _, owned, err := s.store.GetSub2APIHubAccount(ctx, connection.ID, account.ID); err != nil || !owned {
				return storage.Account{}, false, err
			}
		}
		return account, true, nil
	}
	return storage.Account{}, false, nil
}

func (s *Server) sub2APIHubAccountDTO(ctx context.Context, connection storage.Sub2APIHubConnection, account storage.Account) map[string]interface{} {
	now := storage.Now()
	token, _ := s.store.GetToken(ctx, account.ID)
	expired := token.ExpiresAt > 0 && token.ExpiresAt <= now
	quarantined := account.QuarantineUntil > now
	schedulable := account.Status == "active" && !quarantined && !expired
	status := "active"
	if account.Status == "disabled" {
		status = "inactive"
	} else if quarantined && account.QuarantineUntil >= kiroSuspensionQuarantineUntil {
		status = "error"
	}
	accountType := "oauth"
	if accountprovider.UsesAPIKey("codex", token) {
		accountType = "apikey"
	}
	groupID := hubNumericID("sub2api-group", connection.TargetGroupID)
	var proxyID *int64
	if binding, err := s.store.GetEgressBinding(ctx, account.ID); err == nil {
		for _, allowed := range connection.AllowedProxyIDs {
			if binding.PrimaryEgressID == allowed {
				value := hubNumericID("sub2api-proxy", allowed)
				proxyID = &value
				break
			}
		}
	}
	verification := "active"
	if mapping, found, _ := s.store.GetSub2APIHubAccount(ctx, connection.ID, account.ID); found {
		verification = mapping.State
	} else if !schedulable {
		verification = "not_owned"
	}
	var expires interface{}
	if token.ExpiresAt > 0 {
		expires = token.ExpiresAt
	}
	var tempUntil interface{}
	if quarantined && account.QuarantineUntil < kiroSuspensionQuarantineUntil {
		tempUntil = time.Unix(account.QuarantineUntil, 0).UTC().Format(time.RFC3339)
	}
	return map[string]interface{}{
		"id": hubNumericID("sub2api-account", account.ID), "name": firstNonEmpty(account.Label, accountExportCode(account.ID)),
		"notes": nil, "platform": "openai", "type": accountType, "credentials": map[string]interface{}{},
		"credentials_status": map[string]bool{"has_access_token": token.AccessToken != "", "has_refresh_token": token.RefreshToken != "", "has_id_token": token.IDTokenRaw != ""},
		"extra":              map[string]interface{}{"verification_state": verification, "source": "micliproxy_sub2api_hub"},
		"proxy_id":           proxyID, "concurrency": connection.DefaultConcurrency, "priority": connection.DefaultPriority,
		"rate_multiplier": 1, "status": status, "error_message": map[bool]string{true: account.QuarantineReason, false: ""}[quarantined],
		"expires_at": expires, "auto_pause_on_expired": token.RefreshToken == "", "schedulable": schedulable,
		"temp_unschedulable_until": tempUntil, "temp_unschedulable_reason": map[bool]string{true: account.QuarantineReason, false: ""}[quarantined],
		"created_at": time.Unix(account.CreatedAt, 0).UTC().Format(time.RFC3339), "updated_at": time.Unix(account.UpdatedAt, 0).UTC().Format(time.RFC3339),
		"group_ids": []int64{groupID}, "groups": []map[string]interface{}{{"id": groupID, "name": connection.TargetGroupID, "platform": "openai", "status": "active"}},
	}
}

func (s *Server) sub2APIHubListAccounts(w http.ResponseWriter, r *http.Request, connection storage.Sub2APIHubConnection) {
	accounts, err := s.sub2APIHubInventory(r.Context(), connection)
	if err != nil {
		writeSub2APIHubError(w, http.StatusInternalServerError, "Inventory unavailable", "storage_unavailable", nil)
		return
	}
	query := r.URL.Query()
	search := strings.ToLower(strings.TrimSpace(query.Get("search")))
	statusFilter := strings.ToLower(strings.TrimSpace(query.Get("status")))
	// The upstream Sub2API API has used both `type` and `account_type` for
	// this filter across releases.  Accept both spellings, preferring the
	// explicit `type` value when callers send both, so pagination/retry URLs
	// remain wire-compatible with either generation.
	typeFilter := strings.ToLower(strings.TrimSpace(firstNonEmpty(query.Get("type"), query.Get("account_type"))))
	platformFilter := strings.ToLower(strings.TrimSpace(query.Get("platform")))
	groupFilter := strings.TrimSpace(query.Get("group"))
	filtered := make([]storage.Account, 0, len(accounts))
	for _, account := range accounts {
		if platformFilter != "" && platformFilter != "openai" && platformFilter != "codex" {
			continue
		}
		if groupFilter != "" && groupFilter != connection.TargetGroupID && groupFilter != strconv.FormatInt(hubNumericID("sub2api-group", connection.TargetGroupID), 10) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(account.ID+" "+account.Label+" "+account.Email), search) {
			continue
		}
		dto := s.sub2APIHubAccountDTO(r.Context(), connection, account)
		if statusFilter != "" && statusFilter != strings.ToLower(fmt.Sprint(dto["status"])) {
			continue
		}
		if typeFilter != "" && typeFilter != strings.ToLower(fmt.Sprint(dto["type"])) {
			continue
		}
		filtered = append(filtered, account)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].CreatedAt == filtered[j].CreatedAt {
			return filtered[i].ID < filtered[j].ID
		}
		return filtered[i].CreatedAt > filtered[j].CreatedAt
	})
	page, _ := strconv.Atoi(query.Get("page"))
	pageSize, _ := strconv.Atoi(firstNonEmpty(query.Get("page_size"), query.Get("limit")))
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 1000 {
		pageSize = 1000
	}
	start := (page - 1) * pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	items := make([]map[string]interface{}, 0, end-start)
	for _, account := range filtered[start:end] {
		items = append(items, s.sub2APIHubAccountDTO(r.Context(), connection, account))
	}
	pages := int(math.Ceil(float64(len(filtered)) / float64(pageSize)))
	if pages < 1 {
		pages = 1
	}
	writeSub2APIHubSuccess(w, http.StatusOK, map[string]interface{}{"items": items, "total": len(filtered), "page": page, "page_size": pageSize, "pages": pages})
}
