package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/storage"

	"github.com/google/uuid"
)

const (
	sub2APIHubBodyLimit      = int64(8 << 20)
	sub2APIHubKeyGrace       = 15 * time.Minute
	sub2APIHubDefaultBatch   = 100
	sub2APIHubMaxBatch       = 500
	sub2APIHubDefaultRPM     = 120
	sub2APIHubDefaultFlights = 4
)

type sub2APIHubRateState struct {
	Minute   int64
	Count    int
	InFlight int
	LastSeen int64
}

type sub2APIEnvelope struct {
	Code     int               `json:"code"`
	Message  string            `json:"message"`
	Reason   string            `json:"reason,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Data     interface{}       `json:"data,omitempty"`
}

func writeSub2APIHubSuccess(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, sub2APIEnvelope{Code: 0, Message: "success", Data: data})
}

func writeSub2APIHubError(w http.ResponseWriter, status int, message, reason string, metadata map[string]string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, sub2APIEnvelope{Code: status, Message: message, Reason: reason, Metadata: metadata})
}

func writeSub2APIHubMethodNotAllowed(w http.ResponseWriter, allowed ...string) {
	if len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
	}
	writeSub2APIHubError(w, http.StatusMethodNotAllowed, "Method not allowed", "method_not_allowed", nil)
}

func (s *Server) sub2APIHubKeyHash(plaintext string) (string, error) {
	if len(s.identitySecretCached) < 16 {
		return "", errors.New("Hub key verifier unavailable")
	}
	mac := hmac.New(sha256.New, s.identitySecretCached)
	_, _ = mac.Write([]byte("codex-pool/sub2api-hub-key/v1\x00"))
	_, _ = mac.Write([]byte(strings.TrimSpace(plaintext)))
	return "hmac-sha256-v1:" + hex.EncodeToString(mac.Sum(nil)), nil
}

func newSub2APIHubKey() (plaintext, prefix string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	plaintext = "hub_sk_" + base64.RawURLEncoding.EncodeToString(raw)
	prefix = plaintext
	if len(prefix) > 15 {
		prefix = prefix[:15]
	}
	return plaintext, prefix, nil
}

func hubNumericID(namespace, value string) int64 {
	digest := sha256.Sum256([]byte(namespace + "\x00" + strings.TrimSpace(value)))
	value64 := binary.BigEndian.Uint64(digest[:8]) & ((1 << 63) - 1)
	if value64 == 0 {
		value64 = 1
	}
	return int64(value64)
}

func normalizeHubStringSet(values []string, aliases map[string]string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.ToLower(strings.TrimSpace(raw))
		if alias := aliases[value]; alias != "" {
			value = alias
		}
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func normalizeHubConnection(item *storage.Sub2APIHubConnection) error {
	item.Name = strings.TrimSpace(item.Name)
	item.TargetGroupID = strings.TrimSpace(item.TargetGroupID)
	item.InventoryScope = strings.TrimSpace(item.InventoryScope)
	item.DuplicatePolicy = strings.TrimSpace(item.DuplicatePolicy)
	item.ActivationPolicy = strings.TrimSpace(item.ActivationPolicy)
	if item.Name == "" || len(item.Name) > 120 {
		return errors.New("name is required and must not exceed 120 characters")
	}
	if item.TargetGroupID == "" {
		return errors.New("target_group_id is required")
	}
	if item.InventoryScope == "" {
		item.InventoryScope = "connection_only"
	}
	if item.InventoryScope != "connection_only" && item.InventoryScope != "target_group" {
		return errors.New("inventory_scope must be connection_only or target_group")
	}
	item.ProviderAllowlist = normalizeHubStringSet(item.ProviderAllowlist, map[string]string{"openai": "codex", "chatgpt": "codex"})
	if len(item.ProviderAllowlist) == 0 {
		item.ProviderAllowlist = []string{"codex"}
	}
	for _, provider := range item.ProviderAllowlist {
		if provider != "codex" {
			return fmt.Errorf("provider %q is not supported by Sub2API Hub v1", provider)
		}
	}
	item.AllowedProxyIDs = normalizeHubStringSet(item.AllowedProxyIDs, nil)
	item.AllowedCIDRs = normalizeHubStringSet(item.AllowedCIDRs, nil)
	for _, raw := range item.AllowedCIDRs {
		if ip := net.ParseIP(raw); ip != nil {
			continue
		}
		if _, _, err := net.ParseCIDR(raw); err != nil {
			return fmt.Errorf("invalid allowed CIDR %q", raw)
		}
	}
	if item.DefaultConcurrency <= 0 {
		item.DefaultConcurrency = 3
	}
	if item.DefaultConcurrency > 1000 {
		return errors.New("default_concurrency must be between 1 and 1000")
	}
	if item.DefaultPriority <= 0 {
		item.DefaultPriority = 50
	}
	if item.DefaultPriority > 10000 {
		return errors.New("default_priority must be between 1 and 10000")
	}
	if item.MaxAccounts <= 0 {
		item.MaxAccounts = 1000
	}
	if item.MaxAccounts > 1_000_000 {
		return errors.New("max_accounts must be between 1 and 1000000")
	}
	if item.MaxImportBatch <= 0 {
		item.MaxImportBatch = sub2APIHubDefaultBatch
	}
	if item.MaxImportBatch > sub2APIHubMaxBatch {
		return fmt.Errorf("max_import_batch must be between 1 and %d", sub2APIHubMaxBatch)
	}
	if item.RequestsPerMinute <= 0 {
		item.RequestsPerMinute = sub2APIHubDefaultRPM
	}
	if item.RequestsPerMinute > 10000 {
		return errors.New("requests_per_minute must be between 1 and 10000")
	}
	if item.MaxConcurrentRequests <= 0 {
		item.MaxConcurrentRequests = sub2APIHubDefaultFlights
	}
	if item.MaxConcurrentRequests > 100 {
		return errors.New("max_concurrent_requests must be between 1 and 100")
	}
	if item.DuplicatePolicy == "" {
		item.DuplicatePolicy = "reject_cross_connection"
	}
	if item.DuplicatePolicy != "reject_cross_connection" && item.DuplicatePolicy != "reuse_unowned_local" {
		return errors.New("duplicate_policy must be reject_cross_connection or reuse_unowned_local")
	}
	if item.ActivationPolicy == "" {
		item.ActivationPolicy = "verify_then_activate"
	}
	if item.ActivationPolicy != "verify_then_activate" {
		return errors.New("activation_policy must be verify_then_activate")
	}
	return nil
}

func (s *Server) validateHubConnectionReferences(ctx context.Context, item storage.Sub2APIHubConnection) error {
	if _, err := s.store.GetGroup(ctx, item.TargetGroupID); err != nil {
		return fmt.Errorf("target group %q does not exist", item.TargetGroupID)
	}
	for _, id := range item.AllowedProxyIDs {
		if _, err := s.store.GetEgressProfile(ctx, id); err != nil {
			return fmt.Errorf("allowed proxy %q does not exist", id)
		}
	}
	return nil
}

func (s *Server) hubConnectionActor(r *http.Request) string {
	if user, ok := s.currentUser(r); ok {
		return "user:" + user.ID
	}
	return "admin_token"
}

func (s *Server) hubConnectionPublic(r *http.Request, item storage.Sub2APIHubConnection) map[string]interface{} {
	return map[string]interface{}{
		"connection":     item,
		"base_url":       s.externalOrigin(r) + "/api/v1",
		"target_group":   map[string]interface{}{"id": hubNumericID("sub2api-group", item.TargetGroupID), "name": item.TargetGroupID},
		"global_enabled": s.flagEnabled(r.Context(), "sub2api_hub_compat_v1", false),
	}
}

func (s *Server) adminSub2APIHubConnections(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.store.ListSub2APIHubConnections(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		out := make([]map[string]interface{}, 0, len(items))
		for _, item := range items {
			view := s.hubConnectionPublic(r, item)
			if count, countErr := s.store.CountSub2APIHubAccounts(r.Context(), item.ID); countErr == nil {
				view["account_count"] = count
			}
			out = append(out, view)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"connections": out, "global_enabled": s.flagEnabled(r.Context(), "sub2api_hub_compat_v1", false)})
	case http.MethodPost:
		var req storage.Sub2APIHubConnection
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := normalizeHubConnection(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.validateHubConnectionReferences(r.Context(), req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		plaintext, prefix, err := newSub2APIHubKey()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		req.KeyHash, err = s.sub2APIHubKeyHash(plaintext)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		req.ID = "hub_" + uuid.NewString()
		req.KeyPrefix = prefix
		req.CreatedBy = s.hubConnectionActor(r)
		if err = s.store.CreateSub2APIHubConnection(r.Context(), req); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.enqueueAudit(storage.AuditLogRow{Action: "sub2api_hub_connection_create", State: "created", Reason: "least_privilege_key", Detail: "connection_id=" + req.ID, CreatedAt: storage.Now()})
		response := s.hubConnectionPublic(r, req)
		response["api_key"] = plaintext
		response["api_key_display"] = "shown_once"
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, response)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminSub2APIHubConnectionAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	remainder := strings.TrimPrefix(r.URL.Path, "/admin/sub2api-hub/connections/")
	parts := strings.Split(strings.Trim(remainder, "/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		writeError(w, http.StatusNotFound, errors.New("connection not found"))
		return
	}
	id := strings.TrimSpace(parts[0])
	item, err := s.store.GetSub2APIHubConnection(r.Context(), id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, storage.ErrSub2APIHubConnectionNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, s.hubConnectionPublic(r, item))
		case http.MethodPut, http.MethodPatch:
			var req storage.Sub2APIHubConnection
			// PATCH is a true partial update. Decoding into the existing value keeps
			// omitted policy fields intact; PUT retains replacement semantics and is
			// normalized below. Secret and audit fields are restored explicitly in
			// both cases and can never be changed by JSON input.
			if r.Method == http.MethodPatch {
				req = item
			}
			if err = decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			req.ID, req.KeyHash, req.KeyPrefix, req.PreviousKeyHash = item.ID, item.KeyHash, item.KeyPrefix, item.PreviousKeyHash
			req.PreviousKeyExpiresAt, req.CreatedBy, req.CreatedAt, req.LastSeenAt = item.PreviousKeyExpiresAt, item.CreatedBy, item.CreatedAt, item.LastSeenAt
			if err = normalizeHubConnection(&req); err == nil {
				err = s.validateHubConnectionReferences(r.Context(), req)
			}
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			if err = s.store.UpdateSub2APIHubConnection(r.Context(), req); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			s.enqueueAudit(storage.AuditLogRow{Action: "sub2api_hub_connection_update", State: "updated", Reason: "admin", Detail: "connection_id=" + id, CreatedAt: storage.Now()})
			writeJSON(w, http.StatusOK, s.hubConnectionPublic(r, req))
		case http.MethodDelete:
			if err = s.store.RevokeSub2APIHubConnection(r.Context(), id); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			s.enqueueAudit(storage.AuditLogRow{Action: "sub2api_hub_connection_revoke", State: "revoked", Reason: "admin", Detail: "connection_id=" + id, CreatedAt: storage.Now()})
			w.WriteHeader(http.StatusNoContent)
		default:
			methodNotAllowed(w)
		}
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	switch parts[1] {
	case "rotate-key":
		plaintext, prefix, createErr := newSub2APIHubKey()
		if createErr != nil {
			writeError(w, http.StatusInternalServerError, createErr)
			return
		}
		keyHash, hashErr := s.sub2APIHubKeyHash(plaintext)
		if hashErr != nil {
			writeError(w, http.StatusInternalServerError, hashErr)
			return
		}
		graceUntil := time.Now().Add(sub2APIHubKeyGrace).Unix()
		if err = s.store.RotateSub2APIHubConnectionKey(r.Context(), id, keyHash, prefix, graceUntil); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.enqueueAudit(storage.AuditLogRow{Action: "sub2api_hub_key_rotate", State: "rotated", Reason: "admin", Detail: "connection_id=" + id, CreatedAt: storage.Now()})
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]interface{}{"api_key": plaintext, "api_key_display": "shown_once", "previous_key_expires_at": graceUntil})
	case "test":
		count, countErr := s.store.CountSub2APIHubAccounts(r.Context(), id)
		if countErr != nil {
			writeError(w, http.StatusInternalServerError, countErr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"connection_id": id, "global_enabled": s.flagEnabled(r.Context(), "sub2api_hub_compat_v1", false),
			"enabled": item.Enabled, "account_count": count, "checks": []string{"groups", "accounts", "idempotency", "scope"},
			"private_connector_fixtures": "blocked_until_three_real_fixtures",
		})
	default:
		writeError(w, http.StatusNotFound, errors.New("connection action not found"))
	}
}

func (s *Server) beginSub2APIHubRequest(connection storage.Sub2APIHubConnection, now int64) (func(), int, bool) {
	s.sub2APIHubLimiterMu.Lock()
	defer s.sub2APIHubLimiterMu.Unlock()
	if s.sub2APIHubLimiters == nil {
		s.sub2APIHubLimiters = map[string]*sub2APIHubRateState{}
	}
	minute := now / 60
	state := s.sub2APIHubLimiters[connection.ID]
	if state == nil || state.Minute != minute {
		state = &sub2APIHubRateState{Minute: minute}
		s.sub2APIHubLimiters[connection.ID] = state
	}
	if state.Count >= connection.RequestsPerMinute {
		return nil, int(60 - now%60), false
	}
	if state.InFlight >= connection.MaxConcurrentRequests {
		return nil, 1, false
	}
	state.Count++
	state.InFlight++
	state.LastSeen = now
	if len(s.sub2APIHubLimiters) > 2048 {
		for id, candidate := range s.sub2APIHubLimiters {
			if candidate.InFlight == 0 && candidate.LastSeen < now-600 {
				delete(s.sub2APIHubLimiters, id)
			}
		}
	}
	return func() {
		s.sub2APIHubLimiterMu.Lock()
		if current := s.sub2APIHubLimiters[connection.ID]; current != nil && current.InFlight > 0 {
			current.InFlight--
		}
		s.sub2APIHubLimiterMu.Unlock()
	}, 0, true
}

func hubClientAllowed(client string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(client))
	if ip == nil {
		return false
	}
	for _, raw := range allowed {
		if allowedIP := net.ParseIP(raw); allowedIP != nil && allowedIP.Equal(ip) {
			return true
		}
		_, network, err := net.ParseCIDR(raw)
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) authorizeSub2APIHub(w http.ResponseWriter, r *http.Request) (storage.Sub2APIHubConnection, func(), bool) {
	if !s.flagEnabled(r.Context(), "sub2api_hub_compat_v1", false) {
		writeSub2APIHubError(w, http.StatusNotFound, "Sub2API Hub compatibility is disabled", "feature_disabled", nil)
		return storage.Sub2APIHubConnection{}, nil, false
	}
	plaintext := strings.TrimSpace(r.Header.Get("x-api-key"))
	if plaintext == "" {
		plaintext = strings.TrimSpace(adminBearerToken(r))
	}
	if plaintext == "" || len(plaintext) > 256 {
		writeSub2APIHubError(w, http.StatusUnauthorized, "Invalid API key", "hub_key_required", nil)
		return storage.Sub2APIHubConnection{}, nil, false
	}
	keyHash, err := s.sub2APIHubKeyHash(plaintext)
	if err != nil {
		writeSub2APIHubError(w, http.StatusServiceUnavailable, "Hub authentication unavailable", "storage_unavailable", nil)
		return storage.Sub2APIHubConnection{}, nil, false
	}
	now := storage.Now()
	connection, err := s.store.FindSub2APIHubConnectionByKeyHash(r.Context(), keyHash, now)
	if err != nil || !connection.Enabled || (connection.ExpiresAt > 0 && connection.ExpiresAt < now) {
		writeSub2APIHubError(w, http.StatusUnauthorized, "Invalid or revoked API key", "hub_key_invalid", nil)
		return storage.Sub2APIHubConnection{}, nil, false
	}
	if !hubClientAllowed(s.clientIP(r), connection.AllowedCIDRs) {
		writeSub2APIHubError(w, http.StatusForbidden, "Client IP is not allowed", "cidr_denied", nil)
		return storage.Sub2APIHubConnection{}, nil, false
	}
	done, retryAfter, ok := s.beginSub2APIHubRequest(connection, now)
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeSub2APIHubError(w, http.StatusTooManyRequests, "Hub connection rate limit exceeded", "hub_rate_limited", map[string]string{"retry_after": strconv.Itoa(retryAfter)})
		return storage.Sub2APIHubConnection{}, nil, false
	}
	_ = s.store.TouchSub2APIHubConnection(r.Context(), connection.ID, now)
	return connection, done, true
}

func (s *Server) sub2APIHubGroupsAll(w http.ResponseWriter, r *http.Request) {
	connection, done, ok := s.authorizeSub2APIHub(w, r)
	if !ok {
		return
	}
	defer done()
	if r.Method != http.MethodGet {
		writeSub2APIHubMethodNotAllowed(w, http.MethodGet)
		return
	}
	group, err := s.store.GetGroup(r.Context(), connection.TargetGroupID)
	if err != nil {
		writeSub2APIHubError(w, http.StatusInternalServerError, "Target group unavailable", "target_group_unavailable", nil)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	writeSub2APIHubSuccess(w, http.StatusOK, []map[string]interface{}{{
		"id": hubNumericID("sub2api-group", group.Name), "name": group.Name, "description": "Hub connection target group",
		"platform": "openai", "rate_multiplier": 1, "is_exclusive": false, "status": "active",
		"subscription_type": "", "rpm_limit": 0, "created_at": time.Unix(group.CreatedAt, 0).UTC().Format(time.RFC3339),
		"updated_at": now,
	}})
}

func hubProxyWire(profile storage.EgressProfile) (protocol, host string, port int, username string, ok bool) {
	protocol = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(profile.Type)), "_proxy")
	if protocol != "http" && protocol != "https" && protocol != "socks5" && protocol != "socks5h" {
		return "", "", 0, "", false
	}
	raw := strings.TrimSpace(profile.Endpoint)
	if !strings.Contains(raw, "://") {
		raw = protocol + "://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return "", "", 0, "", false
	}
	port, err = strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", "", 0, "", false
	}
	if parsed.User != nil {
		username = parsed.User.Username()
	}
	return protocol, parsed.Hostname(), port, username, true
}

func (s *Server) sub2APIHubProxiesAll(w http.ResponseWriter, r *http.Request) {
	connection, done, ok := s.authorizeSub2APIHub(w, r)
	if !ok {
		return
	}
	defer done()
	if r.Method != http.MethodGet {
		writeSub2APIHubMethodNotAllowed(w, http.MethodGet)
		return
	}
	items := make([]map[string]interface{}, 0, len(connection.AllowedProxyIDs))
	for _, id := range connection.AllowedProxyIDs {
		profile, err := s.store.GetEgressProfile(r.Context(), id)
		if err != nil {
			continue
		}
		protocol, host, port, username, representable := hubProxyWire(profile)
		if !representable {
			continue
		}
		items = append(items, map[string]interface{}{
			"id": hubNumericID("sub2api-proxy", id), "name": profile.Name, "protocol": protocol, "host": host,
			"port": port, "username": username, "status": map[bool]string{true: "active", false: "inactive"}[profile.Health != "disabled" && profile.Health != "unhealthy"],
			"created_at": time.Unix(profile.CreatedAt, 0).UTC().Format(time.RFC3339), "updated_at": time.Unix(profile.UpdatedAt, 0).UTC().Format(time.RFC3339),
			"expires_at": nil, "fallback_mode": "none", "backup_proxy_id": nil, "expiry_warn_days": 0,
		})
	}
	writeSub2APIHubSuccess(w, http.StatusOK, items)
}
