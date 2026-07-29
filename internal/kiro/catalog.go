package kiro

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/storage"
)

const (
	kiroCatalogTTL      = 6 * time.Hour
	kiroCatalogMaxPages = 100
)

// CatalogProbeError intentionally carries only a stable classification and status.
// Upstream response bodies, credential identifiers, URLs, and network addresses are
// never included in its public Error string.
type CatalogProbeError struct {
	Class      string
	StatusCode int
}

type catalogFlight struct {
	done   chan struct{}
	models []storage.KiroModelDescriptor
	err    error
}

func (e *CatalogProbeError) Error() string {
	if e == nil {
		return "Kiro model catalog probe failed"
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("Kiro model catalog probe failed (%s, status %d)", e.Class, e.StatusCode)
	}
	return fmt.Sprintf("Kiro model catalog probe failed (%s)", e.Class)
}

// KiroCapabilityKey prevents observations made against one endpoint, region, or
// governance profile from granting capabilities in another scope. The profile ARN
// is hashed before persistence.
func KiroCapabilityKey(endpointHash, region, governance string) (capabilityKey, governanceKey string) {
	governanceSum := sha256.Sum256([]byte("kiro-governance\x00" + strings.TrimSpace(governance)))
	governanceKey = hex.EncodeToString(governanceSum[:])
	scopeSum := sha256.Sum256([]byte(strings.TrimSpace(endpointHash) + "\x00" +
		strings.ToLower(strings.TrimSpace(region)) + "\x00" + governanceKey))
	return hex.EncodeToString(scopeSum[:]), governanceKey
}

// RefreshModelCatalog calls Kiro's paginated ListAvailableModels operation and
// atomically replaces the account-scoped last-good catalog only after every page
// has been validated. Concurrent refreshes of the same scope are singleflight.
func (m *Manager) RefreshModelCatalog(ctx context.Context, account storage.Account, cred storage.KiroCredentials, bearer string, egress storage.EgressProfile) ([]storage.KiroModelDescriptor, error) {
	cfg := m.Config()
	region := first(cred.APIRegion, cfg.KiroDefaultAPIRegion, "us-east-1")
	base, err := ValidateEndpoint(cred.Endpoint, region, cfg.KiroEndpointAllowlist)
	if err != nil {
		return nil, err
	}
	base = trimKiroOperation(base)
	endpointHash, err := EndpointHash(base, region, cfg.KiroEndpointAllowlist)
	if err != nil {
		return nil, err
	}
	capabilityKey, governanceKey := KiroCapabilityKey(endpointHash, region, cred.ProfileARN)
	flightKey := account.ID + "\x00" + capabilityKey
	flight, leader := m.beginCatalogFlight(flightKey)
	if !leader {
		select {
		case <-flight.done:
			return append([]storage.KiroModelDescriptor(nil), flight.models...), flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var (
		result    []storage.KiroModelDescriptor
		resultErr error
	)
	defer func() {
		m.completeCatalogFlight(flightKey, flight, result, resultErr)
	}()

	now := storage.Now()
	state := storage.KiroProbeState{
		AccountID:     account.ID,
		CapabilityKey: capabilityKey,
		Region:        strings.ToLower(strings.TrimSpace(region)),
		EndpointHash:  endpointHash,
		GovernanceKey: governanceKey,
		Source:        "kiro_live_catalog",
		Generation:    now,
		ExpiresAt:     now + int64(kiroCatalogTTL/time.Second),
	}
	models, defaultID, pages, err := m.fetchCompleteModelCatalog(ctx, account, cred, bearer, egress, base, state)
	if err != nil {
		var probeErr *CatalogProbeError
		if errors.As(err, &probeErr) {
			state.LastErrorClass = probeErr.Class
		} else {
			state.LastErrorClass = "internal"
		}
		_ = m.store.RecordKiroProbeError(ctx, state)
		resultErr = err
		return nil, resultErr
	}
	state.PageCount = pages
	state.Complete = true
	for index := range models {
		models[index].AccountID = account.ID
		models[index].CapabilityKey = capabilityKey
		models[index].Region = state.Region
		models[index].Source = state.Source
		models[index].Generation = state.Generation
		models[index].ObservedAt = now
		models[index].ExpiresAt = state.ExpiresAt
		models[index].Complete = true
		if defaultID != "" && (strings.EqualFold(models[index].UpstreamID, defaultID) ||
			strings.EqualFold(models[index].PublicID, defaultID)) {
			models[index].Default = true
		}
	}
	if !catalogHasDefault(models) {
		for index := range models {
			if strings.EqualFold(models[index].PublicID, "auto") {
				models[index].Default = true
				break
			}
		}
	}
	if err := m.store.ReplaceKiroModelCatalog(ctx, state, models); err != nil {
		resultErr = err
		return nil, resultErr
	}
	result = models
	return result, nil
}

func (m *Manager) beginCatalogFlight(key string) (*catalogFlight, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.catalogFlights[key]; existing != nil {
		return existing, false
	}
	flight := &catalogFlight{done: make(chan struct{})}
	m.catalogFlights[key] = flight
	return flight, true
}

func (m *Manager) completeCatalogFlight(key string, flight *catalogFlight, models []storage.KiroModelDescriptor, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.catalogFlights[key]; current != flight {
		return
	}
	flight.models = append([]storage.KiroModelDescriptor(nil), models...)
	flight.err = err
	delete(m.catalogFlights, key)
	close(flight.done)
}

func trimKiroOperation(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	for _, suffix := range []string{"/generateAssistantResponse", "/getUsageLimits", "/listAvailableModels"} {
		if strings.HasSuffix(base, suffix) {
			return strings.TrimSuffix(base, suffix)
		}
	}
	return base
}

func (m *Manager) fetchCompleteModelCatalog(ctx context.Context, account storage.Account, cred storage.KiroCredentials, bearer string, egress storage.EgressProfile, base string, state storage.KiroProbeState) ([]storage.KiroModelDescriptor, string, int, error) {
	var (
		all       []storage.KiroModelDescriptor
		defaultID string
		nextToken string
		seen      = map[string]struct{}{"": {}}
	)
	for page := 1; page <= kiroCatalogMaxPages; page++ {
		target, err := url.Parse(base + "/listAvailableModels")
		if err != nil {
			return nil, "", page - 1, &CatalogProbeError{Class: "internal"}
		}
		query := target.Query()
		query.Set("origin", "AI_EDITOR")
		if cred.ProfileARN != "" {
			query.Set("profileArn", cred.ProfileARN)
		}
		if nextToken != "" {
			query.Set("nextToken", nextToken)
		}
		target.RawQuery = query.Encode()
		headers := Headers(m.Config(), cred, bearer, false)
		headers.Set("Accept", "application/json")
		status, _, raw, requestErr := m.do(ctx, egress, http.MethodGet, target.String(), headers, nil, account.ID+":"+egress.ID)
		if requestErr != nil {
			return nil, "", page - 1, &CatalogProbeError{Class: "network"}
		}
		if status < 200 || status >= 300 {
			return nil, "", page - 1, &CatalogProbeError{Class: classifyCatalogStatus(status), StatusCode: status}
		}
		pageModels, pageDefault, token, parseErr := parseKiroCatalogPage(raw)
		if parseErr != nil {
			return nil, "", page - 1, &CatalogProbeError{Class: "invalid_response"}
		}
		if len(pageModels) == 0 {
			return nil, "", page, &CatalogProbeError{Class: "empty_catalog"}
		}
		if pageDefault != "" {
			defaultID = pageDefault
		}
		all = append(all, pageModels...)
		nextToken = strings.TrimSpace(token)
		if nextToken == "" {
			all = deduplicateCatalog(all)
			if len(all) == 0 {
				return nil, "", page, &CatalogProbeError{Class: "empty_catalog"}
			}
			return all, defaultID, page, nil
		}
		if _, duplicate := seen[nextToken]; duplicate {
			return nil, "", page, &CatalogProbeError{Class: "pagination"}
		}
		seen[nextToken] = struct{}{}
	}
	return nil, "", kiroCatalogMaxPages, &CatalogProbeError{Class: "pagination"}
}

func classifyCatalogStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "auth"
	case status == http.StatusForbidden:
		return "governance"
	case status == http.StatusTooManyRequests:
		return "rate_limit"
	case status >= 500:
		return "upstream"
	default:
		return "invalid_response"
	}
}

func parseKiroCatalogPage(raw []byte) ([]storage.KiroModelDescriptor, string, string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, "", "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, "", "", errors.New("multiple JSON values")
	}
	items := firstObjectArray(root, "models", "availableModels", "modelSummaries", "items")
	if items == nil {
		if nested, ok := root["result"].(map[string]any); ok {
			items = firstObjectArray(nested, "models", "availableModels", "modelSummaries", "items")
		}
	}
	models := make([]storage.KiroModelDescriptor, 0, len(items))
	for _, item := range items {
		model, ok := parseKiroModelDescriptor(item)
		if ok {
			models = append(models, model)
		}
	}
	defaultID := firstString(root, "defaultModel", "defaultModelId", "defaultModelID")
	if object, ok := root["defaultModel"].(map[string]any); ok {
		defaultID = firstString(object, "modelId", "modelID", "id", "publicId")
	}
	nextToken := firstString(root, "nextToken", "nextPageToken", "continuationToken")
	if nested, ok := root["result"].(map[string]any); ok {
		if defaultID == "" {
			defaultID = firstString(nested, "defaultModel", "defaultModelId", "defaultModelID")
		}
		if nextToken == "" {
			nextToken = firstString(nested, "nextToken", "nextPageToken", "continuationToken")
		}
	}
	return models, defaultID, nextToken, nil
}

func parseKiroModelDescriptor(item map[string]any) (storage.KiroModelDescriptor, bool) {
	upstreamID := firstString(item, "modelId", "modelID", "id", "upstreamId", "upstreamID")
	if upstreamID == "" {
		return storage.KiroModelDescriptor{}, false
	}
	publicID := firstString(item, "publicId", "publicID", "modelName", "slug")
	if publicID == "" {
		if canonical, ok := capability.KiroCanonicalModel(upstreamID); ok {
			publicID = canonical
		} else if strings.EqualFold(upstreamID, "auto") {
			publicID = "auto"
		} else {
			publicID = upstreamID
		}
	}
	aliases := stringSlice(item["aliases"])
	if alias := firstString(item, "alias"); alias != "" {
		aliases = append(aliases, alias)
	}
	aliases = normalizedStrings(aliases)
	thinking := firstJSON(item, "thinking", "thinkingConfig", "supportedThinking")
	effort := firstJSON(item, "effort", "effortConfig", "supportedEfforts", "outputConfig")
	raw, _ := json.Marshal(item)
	hash := sha256.Sum256(raw)
	return storage.KiroModelDescriptor{
		UpstreamID:      strings.TrimSpace(upstreamID),
		PublicID:        strings.TrimSpace(publicID),
		Aliases:         aliases,
		Default:         firstBool(item, "default", "isDefault"),
		MaxInputTokens:  firstInt64(item, "maxInputTokens", "maxContextWindow", "contextWindow", "maxTokens"),
		MaxOutputTokens: firstInt64(item, "maxOutputTokens", "outputTokenLimit", "maxTokensOut"),
		ThinkingJSON:    thinking,
		EffortJSON:      effort,
		RawJSONHash:     hex.EncodeToString(hash[:]),
	}, true
}

func firstObjectArray(root map[string]any, keys ...string) []map[string]any {
	for _, key := range keys {
		values, ok := root[key].([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(values))
		for _, value := range values {
			if object, ok := value.(map[string]any); ok {
				out = append(out, object)
			}
		}
		return out
	}
	return nil
}

func firstString(root map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := root[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		case json.Number:
			return value.String()
		}
	}
	return ""
}

func firstInt64(root map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch value := root[key].(type) {
		case json.Number:
			if result, err := value.Int64(); err == nil {
				return result
			}
			if result, err := strconv.ParseFloat(value.String(), 64); err == nil {
				return int64(result)
			}
		case float64:
			return int64(value)
		case int64:
			return value
		case string:
			if result, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				return result
			}
		}
	}
	return 0
}

func firstBool(root map[string]any, keys ...string) bool {
	for _, key := range keys {
		switch value := root[key].(type) {
		case bool:
			return value
		case string:
			result, _ := strconv.ParseBool(strings.TrimSpace(value))
			return result
		}
	}
	return false
}

func firstJSON(root map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, exists := root[key]; exists && value != nil {
			raw, err := json.Marshal(value)
			if err == nil && string(raw) != "null" {
				return string(raw)
			}
		}
	}
	return "{}"
}

func stringSlice(value any) []string {
	switch values := value.(type) {
	case []any:
		out := make([]string, 0, len(values))
		for _, item := range values {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	case []string:
		return append([]string(nil), values...)
	case string:
		if strings.TrimSpace(values) != "" {
			return []string{values}
		}
	}
	return nil
}

func normalizedStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

func deduplicateCatalog(models []storage.KiroModelDescriptor) []storage.KiroModelDescriptor {
	byID := make(map[string]storage.KiroModelDescriptor, len(models))
	for _, model := range models {
		key := strings.ToLower(strings.TrimSpace(model.UpstreamID))
		if key == "" {
			continue
		}
		if previous, exists := byID[key]; exists {
			model.Aliases = normalizedStrings(append(previous.Aliases, model.Aliases...))
			model.Default = model.Default || previous.Default
			if model.MaxInputTokens == 0 {
				model.MaxInputTokens = previous.MaxInputTokens
			}
			if model.MaxOutputTokens == 0 {
				model.MaxOutputTokens = previous.MaxOutputTokens
			}
		}
		byID[key] = model
	}
	out := make([]storage.KiroModelDescriptor, 0, len(byID))
	for _, model := range byID {
		out = append(out, model)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		if out[i].PublicID != out[j].PublicID {
			return out[i].PublicID < out[j].PublicID
		}
		return out[i].UpstreamID < out[j].UpstreamID
	})
	return out
}

func catalogHasDefault(models []storage.KiroModelDescriptor) bool {
	for _, model := range models {
		if model.Default {
			return true
		}
	}
	return false
}

// CatalogAdaptiveThinking returns live-catalog evidence without inferring support
// from a model-name substring. Empty metadata is unknown and allows the versioned
// fallback to remain in control.
func CatalogAdaptiveThinking(model storage.KiroModelDescriptor) (supported, known bool) {
	raw := strings.TrimSpace(model.ThinkingJSON)
	if raw == "" || raw == "{}" || raw == "null" {
		return false, false
	}
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return false, false
	}
	var (
		hasPositive bool
		hasNegative bool
		walk        func(any)
	)
	walk = func(current any) {
		switch typed := current.(type) {
		case bool:
			if typed {
				hasPositive = true
			} else {
				hasNegative = true
			}
		case string:
			switch strings.ToLower(strings.TrimSpace(typed)) {
			case "adaptive", "enabled", "supported", "true":
				hasPositive = true
			case "disabled", "unsupported", "none", "false":
				hasNegative = true
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(value)
	if hasPositive {
		return true, true
	}
	if hasNegative {
		return false, true
	}
	return false, false
}

// CatalogMaximumEffort selects only from explicit live-catalog effort values.
func CatalogMaximumEffort(model storage.KiroModelDescriptor) (string, bool) {
	raw := strings.TrimSpace(model.EffortJSON)
	if raw == "" || raw == "{}" || raw == "null" {
		return "", false
	}
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return "", false
	}
	rank := map[string]int{"low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5}
	best, bestRank := "", 0
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case string:
			normalized := strings.ToLower(strings.TrimSpace(typed))
			if rank[normalized] > bestRank {
				best, bestRank = normalized, rank[normalized]
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(value)
	return best, best != ""
}
