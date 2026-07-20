// admin_resources.go holds the admin console's egress-profile, tenant/user/project,
// and observability (CF events, usage, usage-timeseries, quota) REST handlers.
// Extracted verbatim from server.go (no behavior change). Imports via goimports.
package api

import (
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/proxyparse"
	cliproxyproxy "codex-account-pool/internal/registration/provider/proxy"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) adminEgressBinding(w http.ResponseWriter, r *http.Request, accountID string) {
	switch r.Method {
	case http.MethodGet:
		binding, err := s.store.GetEgressBinding(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, binding)
	case http.MethodPost:
		var req map[string]json.RawMessage
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var primary string
		if raw, ok := req["primary_egress_id"]; ok {
			if err := json.Unmarshal(raw, &primary); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("primary_egress_id must be a string"))
				return
			}
		}
		primary = strings.TrimSpace(primary)
		if primary == "" {
			writeError(w, http.StatusBadRequest, errors.New("primary_egress_id required"))
			return
		}
		if _, err := s.store.GetAccount(r.Context(), accountID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, err)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if _, err := s.store.GetEgressProfile(r.Context(), primary); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("egress %q not found", primary))
			return
		}
		binding, err := s.store.GetEgressBinding(r.Context(), accountID)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			binding = storage.AccountEgressBinding{AccountID: accountID}
		}
		standby := binding.StandbyIDs()
		if raw, ok := req["standby_egress_ids"]; ok {
			ids, err := parseStandbyEgressIDs(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			standby = ids
		}
		standby = normalizeStandbyEgressIDs(primary, standby)
		for _, id := range standby {
			if _, err := s.store.GetEgressProfile(r.Context(), id); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("egress %q not found", id))
				return
			}
		}
		sidecarID := binding.SidecarEgressID
		if raw, ok := req["sidecar_egress_id"]; ok {
			if strings.TrimSpace(string(raw)) == "null" {
				sidecarID = ""
			} else if err := json.Unmarshal(raw, &sidecarID); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("sidecar_egress_id must be a string or null"))
				return
			}
		}
		sidecarID = strings.TrimSpace(sidecarID)
		if sidecarID != "" {
			sidecar, err := s.store.GetEgressProfile(r.Context(), sidecarID)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("sidecar egress %q not found", sidecarID))
				return
			}
			if !storage.IsSidecarEgress(sidecar) {
				writeError(w, http.StatusBadRequest, fmt.Errorf("sidecar egress %q must have type %s", sidecarID, storage.CurlCFFISidecarEgressType))
				return
			}
			if strings.TrimSpace(sidecar.Endpoint) == "" {
				writeError(w, http.StatusBadRequest, fmt.Errorf("sidecar egress %q endpoint required", sidecarID))
				return
			}
		}
		binding.AccountID = accountID
		binding.PrimaryEgressID = primary
		binding.StandbyEgressIDs = strings.Join(standby, ",")
		binding.SidecarEgressID = sidecarID
		binding.CookieJarKey = accountID + ":" + primary
		if err := s.store.UpsertEgressBinding(r.Context(), binding); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		got, err := s.store.GetEgressBinding(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if s.scheduler != nil {
			s.scheduler.InvalidateAccountCache()
		}
		writeJSON(w, http.StatusOK, got)
	default:
		methodNotAllowed(w)
	}
}

func parseStandbyEgressIDs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err == nil {
		return ids, nil
	}
	var csv string
	if err := json.Unmarshal(raw, &csv); err != nil {
		return nil, fmt.Errorf("standby_egress_ids must be a string or array of strings")
	}
	return strings.Split(csv, ","), nil
}

func normalizeStandbyEgressIDs(primary string, ids []string) []string {
	seen := map[string]bool{primary: true}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (s *Server) adminBrowserRepair(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		EgressID     string `json:"egress_id"`
		UpstreamHost string `json:"upstream_host"`
		CookieHeader string `json:"cookie_header"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.CookieHeader == "" {
		writeError(w, http.StatusBadRequest, errors.New("cookie_header required"))
		return
	}
	binding, err := s.store.GetEgressBinding(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	egressID := firstNonEmpty(req.EgressID, binding.PrimaryEgressID)
	if err := s.upstream.ImportCookies(accountID, egressID, req.UpstreamHost, accountID+":"+egressID, req.CookieHeader); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	_ = s.store.SetBindingCooldown(r.Context(), accountID, 0)
	s.scheduler.NotifyStateChanged()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_id": accountID,
		"egress_id":  egressID,
		"repaired":   true,
	})
}

func (s *Server) adminEgressProfiles(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		profiles, err := s.store.ListEgressProfiles(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, profiles)
	case http.MethodPost:
		// Accept either a full EgressProfile {type,endpoint,...} or the four-field
		// proxy form {type,host,port,username,password}; build the endpoint URL from
		// the fields when no explicit endpoint is given.
		var req egressProfileRequest
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		profile, err := req.profile()
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(profile.Endpoint) == "" && strings.TrimSpace(req.Host) != "" {
			d, err := proxyparse.FromFields(req.Host, req.Port, req.Username, req.Password)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			profile.Endpoint = d.Endpoint(profile.Type)
		}
		if strings.TrimSpace(profile.ID) == "" {
			profile.ID = generatedID("egress")
		}
		if strings.TrimSpace(profile.Name) == "" {
			profile.Name = profile.ID
		}
		// Auto-detect region/exit-IP on create (best-effort; off for direct unless
		// explicitly requested) so the operator immediately sees where it exits.
		detect := profile.Type != "" && profile.Type != "direct"
		if req.DetectRegion != nil {
			detect = *req.DetectRegion
		}
		if detect {
			if pr, perr := s.upstream.ProbeEgress(r.Context(), profile); perr == nil {
				profile.Region = firstNonEmpty(pr.Country, profile.Region)
				profile.ExitIP = pr.IP
				profile.LatencyMillis = pr.LatencyMS
			}
		}
		if err := s.store.UpsertEgressProfile(r.Context(), profile); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, profile)
	default:
		methodNotAllowed(w)
	}
}

type egressProfileRequest struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	Type              string          `json:"type"`
	Endpoint          string          `json:"endpoint"`
	ChainProxy        string          `json:"chain_proxy"`
	Region            string          `json:"region"`
	ExitIP            string          `json:"exit_ip"`
	StreamCapable     *bool           `json:"stream_capable"`
	Health            string          `json:"health"`
	LatencyMillis     int64           `json:"latency_millis"`
	CFScore           int64           `json:"cf_score"`
	LastCFRay         string          `json:"last_cf_ray"`
	CooldownUntil     int64           `json:"cooldown_until"`
	MaxConcurrency    int             `json:"max_concurrency"`
	ProxyAuthMode     string          `json:"proxy_auth_mode"`
	ProxyAPIKey       string          `json:"proxy_api_key"`
	IPMode            string          `json:"ip_mode"`
	ProviderKey       string          `json:"provider_key"`
	DynamicConfigJSON json.RawMessage `json:"dynamic_config_json"`
	Host              string          `json:"host"`
	Port              string          `json:"port"`
	Username          string          `json:"username"`
	Password          string          `json:"password"`
	DetectRegion      *bool           `json:"detect_region"`
	APIBase           string          `json:"api_base"`
	APINum            int             `json:"api_num"`
	APITime           int             `json:"api_time"`
}

type egressProfileTestRequest struct {
	EgressID string                `json:"egress_id"`
	Profile  *egressProfileRequest `json:"profile"`
	ProbeURL string                `json:"probe_url"`
}

func (r egressProfileRequest) profile() (storage.EgressProfile, error) {
	dynamicJSON, err := normalizeDynamicConfigJSON(r.DynamicConfigJSON)
	if err != nil {
		return storage.EgressProfile{}, err
	}
	dynamicJSON, err = mergeEgressDynamicConfig(dynamicJSON, r)
	if err != nil {
		return storage.EgressProfile{}, err
	}
	stream := true
	if r.StreamCapable != nil {
		stream = *r.StreamCapable
	}
	return storage.EgressProfile{
		ID:                strings.TrimSpace(r.ID),
		Name:              strings.TrimSpace(r.Name),
		Type:              strings.TrimSpace(r.Type),
		Endpoint:          strings.TrimSpace(r.Endpoint),
		ChainProxy:        strings.TrimSpace(r.ChainProxy),
		Region:            strings.TrimSpace(r.Region),
		ExitIP:            strings.TrimSpace(r.ExitIP),
		StreamCapable:     stream,
		Health:            strings.TrimSpace(r.Health),
		LatencyMillis:     r.LatencyMillis,
		CFScore:           r.CFScore,
		LastCFRay:         strings.TrimSpace(r.LastCFRay),
		CooldownUntil:     r.CooldownUntil,
		MaxConcurrency:    r.MaxConcurrency,
		ProxyAuthMode:     strings.TrimSpace(r.ProxyAuthMode),
		ProxyAPIKey:       strings.TrimSpace(r.ProxyAPIKey),
		IPMode:            strings.TrimSpace(r.IPMode),
		ProviderKey:       strings.TrimSpace(r.ProviderKey),
		DynamicConfigJSON: dynamicJSON,
	}, nil
}

func normalizeDynamicConfigJSON(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}", nil
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("dynamic_config_json: %w", err)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return "{}", nil
		}
		if json.Valid([]byte(s)) {
			return s, nil
		}
		encoded, _ := json.Marshal(map[string]string{"value": s})
		return string(encoded), nil
	}
	if !json.Valid(raw) {
		return "", errors.New("dynamic_config_json must be valid JSON")
	}
	return trimmed, nil
}

func mergeEgressDynamicConfig(dynamicJSON string, req egressProfileRequest) (string, error) {
	if strings.TrimSpace(req.APIBase) == "" && req.APINum <= 0 && req.APITime <= 0 {
		return dynamicJSON, nil
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(dynamicJSON), &obj); err != nil {
		return dynamicJSON, nil
	}
	if obj == nil {
		obj = map[string]interface{}{}
	}
	if v := strings.TrimSpace(req.APIBase); v != "" {
		obj["api_base"] = v
	}
	if req.APINum > 0 {
		obj["api_num"] = req.APINum
	}
	if req.APITime > 0 {
		obj["api_time"] = req.APITime
	}
	encoded, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("dynamic_config_json: %w", err)
	}
	return string(encoded), nil
}

func (s *Server) adminEgressPools(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		pools, err := s.store.ListEgressPools(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		out := make([]storage.EgressPool, 0, len(pools))
		for _, pool := range pools {
			if !isRegistrationEgressPoolPurpose(pool.Purpose) {
				continue
			}
			out = append(out, normalizeRegistrationEgressPool(pool))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var pool storage.EgressPool
		if err := decodeJSONRequestBody(r.Body, &pool, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		pool.Purpose = strings.TrimSpace(pool.Purpose)
		if pool.Purpose == "" {
			pool.Purpose = "registration"
		}
		if !isRegistrationEgressPoolPurpose(pool.Purpose) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("only registration egress pools are supported"))
			return
		}
		if strings.TrimSpace(pool.ID) == "" {
			pool.ID = generatedID("egress_pool")
		}
		if err := s.store.UpsertEgressPool(r.Context(), pool); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		got, err := s.store.GetEgressPool(r.Context(), pool.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, normalizeRegistrationEgressPool(got))
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminEgressPoolAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/egress-pools/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	poolID, action := parts[0], parts[1]
	switch action {
	case "members":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if _, err := s.getRegistrationEgressPool(r.Context(), poolID); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var req struct {
			EgressID string `json:"egress_id"`
			Enabled  *bool  `json:"enabled"`
			Capacity int    `json:"capacity"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.EgressID = strings.TrimSpace(req.EgressID)
		if req.EgressID == "" {
			writeError(w, http.StatusBadRequest, errors.New("egress_id required"))
			return
		}
		if _, err := s.store.GetEgressProfile(r.Context(), req.EgressID); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("egress %q not found", req.EgressID))
			return
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		if err := s.store.UpsertEgressPoolMember(r.Context(), storage.EgressPoolMember{PoolID: poolID, EgressID: req.EgressID, Enabled: enabled, Capacity: req.Capacity}); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		pool, err := s.store.GetEgressPool(r.Context(), poolID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, normalizeRegistrationEgressPool(pool))
	case "rebalance":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		pool, err := s.getRegistrationEgressPool(r.Context(), poolID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"pool_id": pool.ID, "members": len(pool.Members), "strategy": pool.AssignmentStrategy})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) adminEgressProfileAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/egress-profiles/"), "/")
	if rest == "test" {
		s.adminEgressProfileTest(w, r)
		return
	}
	// Collection-level action: bulk import of "host:port:user:pass" lines.
	if rest == "import" {
		s.adminEgressImport(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, action := parts[0], parts[1]
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	switch action {
	case "health-check":
		profile, err := s.store.GetEgressProfile(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		profile.Health = "healthy"
		profile.CooldownUntil = 0
		profile.LatencyMillis = 0
		if err := s.store.UpsertEgressProfile(r.Context(), profile); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"egress_id": id, "health": "healthy"})
	case "detect-region":
		// Probe the geo endpoint THROUGH this egress; persist the discovered exit
		// region/IP/latency. Doubles as a real connectivity check.
		profile, err := s.store.GetEgressProfile(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		pr, perr := s.upstream.ProbeEgress(r.Context(), profile)
		if perr != nil {
			writeError(w, http.StatusBadGateway, perr)
			return
		}
		profile.Region = firstNonEmpty(pr.Country, profile.Region)
		profile.ExitIP = pr.IP
		profile.LatencyMillis = pr.LatencyMS
		profile.Health = "healthy"
		if err := s.store.UpsertEgressProfile(r.Context(), profile); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"egress_id": id, "region": profile.Region, "exit_ip": profile.ExitIP,
			"city": pr.City, "latency_ms": pr.LatencyMS,
		})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) adminEgressProfileTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req egressProfileTestRequest
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	profile, err := s.egressProfileForTest(r.Context(), req)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	probeURL := strings.TrimSpace(req.ProbeURL)
	if probeURL == "" {
		probeURL = strings.TrimSpace(s.cfg.GeoProbeURL)
	}
	if probeURL == "" {
		probeURL = config.DefaultGeoProbeURL
	}
	result, err := s.probeEgressProfileURL(r.Context(), profile, probeURL)
	if err != nil {
		status := http.StatusBadGateway
		if isEgressProfileInputError(err) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":             true,
		"egress_id":      strings.TrimSpace(req.EgressID),
		"exit_ip":        result.IP,
		"region":         result.Country,
		"country":        result.Country,
		"region_name":    result.Region,
		"city":           result.City,
		"latency_ms":     result.LatencyMS,
		"latency_millis": result.LatencyMS,
		"probe_url":      probeURL,
		"warnings":       egressProfileProbeWarnings(profile, result),
	})
}

func (s *Server) egressProfileForTest(ctx context.Context, req egressProfileTestRequest) (storage.EgressProfile, error) {
	if id := strings.TrimSpace(req.EgressID); id != "" {
		return s.store.GetEgressProfile(ctx, id)
	}
	if req.Profile == nil {
		return storage.EgressProfile{}, errors.New("egress_id or profile required")
	}
	profile, err := req.Profile.profile()
	if err != nil {
		return storage.EgressProfile{}, err
	}
	if strings.TrimSpace(profile.Type) == "" {
		profile.Type = "direct"
	}
	if strings.TrimSpace(profile.Endpoint) == "" && strings.TrimSpace(req.Profile.Host) != "" {
		d, err := proxyparse.FromFields(req.Profile.Host, req.Profile.Port, req.Profile.Username, req.Profile.Password)
		if err != nil {
			return storage.EgressProfile{}, err
		}
		profile.Endpoint = d.Endpoint(profile.Type)
	}
	if strings.EqualFold(strings.TrimSpace(profile.ProxyAuthMode), "api_whitelist") && strings.TrimSpace(profile.Endpoint) == "" {
		return s.cliproxyAPIWhitelistProbeProfile(ctx, profile, req.Profile)
	}
	return profile, nil
}

func (s *Server) cliproxyAPIWhitelistProbeProfile(ctx context.Context, profile storage.EgressProfile, req *egressProfileRequest) (storage.EgressProfile, error) {
	base := "https://api.cliproxy.io"
	if s.cfg.CliproxyAPIBase != "" {
		base = strings.TrimSpace(s.cfg.CliproxyAPIBase)
	}
	if req != nil && strings.TrimSpace(req.APIBase) != "" {
		base = strings.TrimSpace(req.APIBase)
	}
	apiKey := strings.TrimSpace(profile.ProxyAPIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(s.cfg.CliproxyAPIKey)
	}
	region := strings.TrimSpace(profile.Region)
	if region == "" {
		region = "Rand"
	}
	num := 1
	if req != nil && req.APINum > 0 {
		num = req.APINum
	}
	timeMin := 10
	if req != nil && req.APITime > 0 {
		timeMin = req.APITime
	}
	extractor := &cliproxyproxy.CliproxyAPIExtractor{
		BaseURL: base,
		APIKey:  apiKey,
		HC:      &http.Client{Timeout: 15 * time.Second},
	}
	ips, err := extractor.ExtractIPs(ctx, region, num, timeMin)
	if err != nil {
		return storage.EgressProfile{}, err
	}
	if len(ips) == 0 || strings.TrimSpace(ips[0]) == "" {
		return storage.EgressProfile{}, errors.New("cliproxy api: no ips in response")
	}
	endpoint := strings.TrimSpace(ips[0])
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	profile.Type = "http_proxy"
	profile.Endpoint = endpoint
	return profile, nil
}

func (s *Server) probeEgressProfileURL(ctx context.Context, profile storage.EgressProfile, probeURL string) (upstream.ProbeResult, error) {
	client, err := s.upstream.EgressHTTPClient(profile)
	if err != nil {
		return upstream.ProbeResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return upstream.ProbeResult{}, err
	}
	req.Header.Set("User-Agent", "curl/8.4.0")
	req.Header.Set("Accept", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return upstream.ProbeResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	latency := time.Since(start).Milliseconds()
	if resp.StatusCode >= 400 {
		return upstream.ProbeResult{}, fmt.Errorf("geo probe status %d", resp.StatusCode)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return upstream.ProbeResult{}, fmt.Errorf("geo probe parse: %w", err)
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	return upstream.ProbeResult{
		IP:        pick("ip", "query", "ip_addr", "YourFuckingIPAddress"),
		Country:   pick("country_code", "countryCode", "country"),
		Region:    pick("region", "region_name", "regionName", "region_code"),
		City:      pick("city"),
		LatencyMS: latency,
	}, nil
}

func egressProfileProbeWarnings(profile storage.EgressProfile, result upstream.ProbeResult) []string {
	warnings := []string{}
	wantRegion := strings.ToUpper(strings.TrimSpace(profile.Region))
	gotRegion := strings.ToUpper(strings.TrimSpace(result.Country))
	if wantRegion != "" && wantRegion != "RAND" && gotRegion != "" && wantRegion != gotRegion {
		warnings = append(warnings, fmt.Sprintf("配置地区 %s 与实际出口地区 %s 不一致", wantRegion, gotRegion))
	}
	if strings.TrimSpace(result.IP) == "" {
		warnings = append(warnings, "探测响应未返回出口 IP")
	}
	return warnings
}

func isEgressProfileInputError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, part := range []string{
		"endpoint required",
		"unsupported egress type",
		"invalid url",
		"invalid url escape",
		"missing protocol scheme",
		"first path segment in url cannot contain colon",
	} {
		if strings.Contains(msg, part) {
			return true
		}
	}
	return false
}

// adminEgressImport bulk-creates proxy egress profiles from newline-separated
// "host:port:username:password" lines (the batch-import format). Region detection
// is off by default (one outbound probe per proxy would be slow on large lists);
// the operator can detect per-row afterwards.
func (s *Server) adminEgressImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Type         string `json:"type"`
		Lines        string `json:"lines"`
		DetectRegion *bool  `json:"detect_region"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Type) == "" {
		req.Type = "socks5h_proxy"
	}
	drafts, parseErrs := proxyparse.ParseLines(req.Lines)
	detect := req.DetectRegion != nil && *req.DetectRegion
	created := make([]storage.EgressProfile, 0, len(drafts))
	for _, d := range drafts {
		p := storage.EgressProfile{
			ID:             generatedID("egress"),
			Name:           firstNonEmpty(d.Host, "proxy"),
			Type:           req.Type,
			Endpoint:       d.Endpoint(req.Type),
			StreamCapable:  true,
			Health:         "healthy",
			MaxConcurrency: 16,
		}
		if detect {
			if pr, perr := s.upstream.ProbeEgress(r.Context(), p); perr == nil {
				p.Region = pr.Country
				p.ExitIP = pr.IP
				p.LatencyMillis = pr.LatencyMS
			}
		}
		if err := s.store.UpsertEgressProfile(r.Context(), p); err == nil {
			created = append(created, p)
		}
	}
	errStrs := make([]string, 0, len(parseErrs))
	for _, e := range parseErrs {
		errStrs = append(errStrs, e.Error())
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count": len(created), "created": created, "errors": errStrs,
	})
}

func (s *Server) adminTenants(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.store.ListTenants(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var item storage.Tenant
		if err := decodeJSONRequestBody(r.Body, &item, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if item.ID == "" {
			item.ID = generatedID("tenant")
		}
		if item.Name == "" {
			writeError(w, http.StatusBadRequest, errors.New("name required"))
			return
		}
		if err := s.store.UpsertTenant(r.Context(), item); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, item)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.store.ListUsers(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var req struct {
			Email    string `json:"email"`
			Name     string `json:"name"`
			Role     string `json:"role"`
			Status   string `json:"status"`
			Password string `json:"password"`
			TenantID string `json:"tenant_id"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		email := strings.TrimSpace(strings.ToLower(req.Email))
		if email == "" {
			writeError(w, http.StatusBadRequest, errors.New("email required"))
			return
		}
		if _, exists, err := s.store.GetUserByEmail(r.Context(), email); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		} else if exists {
			writeError(w, http.StatusConflict, errors.New("email already registered"))
			return
		}
		item := storage.User{ID: generatedID("usr"), Email: email, Name: strings.TrimSpace(req.Name), TenantID: strings.TrimSpace(req.TenantID), Role: "user", Status: "active"}
		if req.Role == "admin" {
			item.Role = "admin"
		}
		if req.Status == "disabled" {
			item.Status = "disabled"
		}
		if strings.TrimSpace(req.Password) != "" {
			if len(req.Password) < minPasswordLen {
				writeError(w, http.StatusBadRequest, errors.New("password too short"))
				return
			}
			ph, err := hashPassword(req.Password)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			item.PasswordHash = ph
		}
		if err := s.store.UpsertUser(r.Context(), item); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, item)
	default:
		methodNotAllowed(w)
	}
}

// adminUserAction handles PATCH/DELETE /admin/users/{id}: change role/status/name,
// reset password, or delete a portal user. Guards against an admin locking
// themselves out (demoting/disabling/deleting their own account). A status change to
// disabled, a password reset, or a delete invalidates the target's sessions.
func (s *Server) adminUserAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/users/"), "/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	u, ok, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	cur, _ := s.currentUser(r) // empty when acting via admin_token
	switch r.Method {
	case http.MethodDelete:
		if cur.ID == u.ID {
			writeError(w, http.StatusBadRequest, errors.New("cannot delete your own account"))
			return
		}
		if err := s.store.DeleteUser(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": id})
	case http.MethodPatch:
		var req struct {
			Role     *string `json:"role"`
			Status   *string `json:"status"`
			Name     *string `json:"name"`
			Password *string `json:"password"`
		}
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		invalidate := false
		if req.Role != nil && (*req.Role == "admin" || *req.Role == "user") {
			u.Role = *req.Role
		}
		if req.Status != nil && (*req.Status == "active" || *req.Status == "disabled") {
			if *req.Status == "disabled" && u.Status != "disabled" {
				invalidate = true
			}
			u.Status = *req.Status
		}
		if req.Name != nil {
			u.Name = strings.TrimSpace(*req.Name)
		}
		if req.Password != nil && *req.Password != "" {
			if len(*req.Password) < minPasswordLen {
				writeError(w, http.StatusBadRequest, errors.New("password too short"))
				return
			}
			ph, err := hashPassword(*req.Password)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			u.PasswordHash = ph
			invalidate = true
		}
		// Self-lockout guard: an admin cannot demote or disable their own account.
		if cur.ID == u.ID && (u.Role != "admin" || u.Status != "active") {
			writeError(w, http.StatusBadRequest, errors.New("cannot demote or disable your own account"))
			return
		}
		if err := s.store.UpsertUser(r.Context(), u); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if invalidate {
			_ = s.store.DeleteUserSessionsForUser(r.Context(), u.ID)
		}
		writeJSON(w, http.StatusOK, u)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminProjects(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.store.ListProjects(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var item storage.Project
		if err := decodeJSONRequestBody(r.Body, &item, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if item.ID == "" {
			item.ID = generatedID("project")
		}
		if item.TenantID == "" || item.Name == "" {
			writeError(w, http.StatusBadRequest, errors.New("tenant_id and name required"))
			return
		}
		if err := s.store.UpsertProject(r.Context(), item); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, item)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminCFEvents(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.ListCFEvents(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if events == nil {
		events = []storage.CFEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}

// adminUsage returns per-account token usage aggregates for the billing view.
func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	now := time.Now()
	completeness, err := s.currentUsageCompleteness(r.Context(), now.Unix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.EnsureUsageDailyResetAudit(r.Context(), now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	win, err := s.resolveAdminUsageWindow(r, now, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	summary, err := s.store.UsageSummaryWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if summary == nil {
		summary = []storage.UsageSummaryRow{}
	}
	cutover := s.store.UsageAccuracyCutover(r.Context())
	body := mergeWindowFields(map[string]interface{}{"rows": summary, "accuracy_cutover_at": cutover, "legacy_unverified": win.EffectiveStartAt < cutover}, win)
	writeJSON(w, http.StatusOK, mergeCompletenessFields(body, completeness))
}

// adminUsageTimeseries returns usage aggregated into fixed-width time buckets for
// the usage-over-time chart. Query params: since (epoch seconds, default last 24h)
// and bucket (seconds per bucket, default 1h).
func (s *Server) adminUsageTimeseries(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	now := time.Now()
	completeness, err := s.currentUsageCompleteness(r.Context(), now.Unix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.EnsureUsageDailyResetAudit(r.Context(), now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	win, err := s.resolveAdminUsageWindow(r, now, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	bucket, _ := strconv.ParseInt(r.URL.Query().Get("bucket"), 10, 64)
	if bucket <= 0 {
		bucket = 3600
	}
	buckets, err := s.store.UsageTimeseriesWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt(), bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if buckets == nil {
		buckets = []storage.UsageBucket{}
	}
	markUsageBucketsPartial(buckets, bucket, completeness.UsageCompleteThroughAt)
	body := map[string]interface{}{
		"since": win.EffectiveStartAt, "bucket": bucket, "now": win.EffectiveUntilAt, "buckets": buckets,
	}
	cutover := s.store.UsageAccuracyCutover(r.Context())
	body["accuracy_cutover_at"] = cutover
	body["legacy_unverified"] = win.EffectiveStartAt < cutover
	if strings.TrimSpace(r.URL.Query().Get("series_dimension")) == "model" {
		limit := 6
		if raw := strings.TrimSpace(r.URL.Query().Get("series_limit")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 || n > 20 {
				writeError(w, http.StatusBadRequest, errors.New("series_limit must be an integer from 1 to 20"))
				return
			}
			limit = n
		}
		series, rows, err := s.store.UsageModelSeriesWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt(), bucket, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		body["series_dimension"] = "model"
		body["series"] = series
		body["model_series"] = rows
	}
	writeJSON(w, http.StatusOK, mergeCompletenessFields(mergeWindowFields(body, win), completeness))
}

// adminQuota returns the latest captured rate-limit / remaining-quota snapshot for
// every account, joined with the account label and provider, for the quota gauges.
// Each snapshot includes both the 5h (primary) and 7d (secondary) window data.
func (s *Server) adminQuota(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if truthyQuery(r.URL.Query().Get("include_missing")) {
		page, pageSize, err := parseQuotaPageParams(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		search := strings.TrimSpace(r.URL.Query().Get("search"))
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		accounts, total, err := s.store.ListAccountsPageDesc(r.Context(), pageSize, (page-1)*pageSize, search, status)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		rows, err := s.quotaViewsForAccounts(r.Context(), accounts)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"rows": rows, "total": total, "page": page, "pageSize": pageSize,
		})
		return
	}
	snaps, err := s.store.ListAccountRateLimits(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	accountIDs := uniqueQuotaAccountIDs(snaps)
	accountsByID, err := s.store.ListAccountsByIDs(r.Context(), accountIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	accounts := make([]storage.Account, 0, len(accountIDs))
	for _, id := range accountIDs {
		if account, ok := accountsByID[id]; ok {
			accounts = append(accounts, account)
		}
	}
	rows, err := s.quotaViewsForAccounts(r.Context(), accounts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

type quotaView struct {
	storage.AccountRateLimit
	Label              string       `json:"label"`
	PlanType           string       `json:"plan_type,omitempty"`
	OAuthRateLimitTier string       `json:"oauth_rate_limit_tier,omitempty"`
	Secondary7d        *QuotaWindow `json:"secondary_7d,omitempty"`
	Secondary7dUsed    float64      `json:"secondary_7d_used_pct"`
	QuotaSummary       QuotaSummary `json:"quota_summary"`
}

func (s *Server) quotaViewsForAccounts(ctx context.Context, accounts []storage.Account) ([]quotaView, error) {
	accountIDs := make([]string, 0, len(accounts))
	for _, account := range accounts {
		accountIDs = append(accountIDs, account.ID)
	}
	labels, err := s.store.AccountLabelsByID(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	tokensByID, err := s.store.ListTokensByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	snapsByID, err := s.store.ListAccountRateLimitsByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	now := storage.Now()
	out := make([]quotaView, 0, len(accounts))
	for _, account := range accounts {
		var token *storage.AccountToken
		if t, ok := tokensByID[account.ID]; ok {
			token = &t
		}
		snaps := snapsByID[account.ID]
		summary := BuildQuotaSummary(account, token, snaps, now)
		base := quotaBaseSnapshot(account, summary, snaps)
		qv := quotaView{
			AccountRateLimit: base,
			Label:            labels[account.ID],
			PlanType:         account.PlanType,
			QuotaSummary:     summary,
			Secondary7d:      summary.Secondary,
		}
		if token != nil {
			qv.OAuthRateLimitTier = token.OAuthRateLimitTier
		}
		if summary.Secondary != nil {
			qv.Secondary7dUsed = summary.Secondary.UsedPercent
		}
		out = append(out, qv)
	}
	return out, nil
}

func quotaBaseSnapshot(account storage.Account, summary QuotaSummary, snaps []storage.AccountRateLimit) storage.AccountRateLimit {
	provider := strings.TrimSpace(summary.Provider)
	if provider == "" {
		provider = strings.TrimSpace(account.Provider)
	}
	if primary := selectQuotaPrimary(provider, snaps); primary != nil {
		return *primary
	}
	if len(snaps) > 0 {
		return snaps[0]
	}
	return storage.AccountRateLimit{
		AccountID:         account.ID,
		Provider:          provider,
		UsedPercent:       -1,
		LimitTokens:       -1,
		RemainingTokens:   -1,
		LimitRequests:     -1,
		RemainingRequests: -1,
	}
}

func uniqueQuotaAccountIDs(snaps []storage.AccountRateLimit) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, snap := range snaps {
		id := strings.TrimSpace(snap.AccountID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func parseQuotaPageParams(r *http.Request) (int, int, error) {
	page, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page")))
	if err != nil || page <= 0 {
		return 0, 0, errors.New("page must be a positive integer")
	}
	pageSize, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("pageSize")))
	if err != nil || pageSize <= 0 || pageSize > 500 {
		return 0, 0, errors.New("pageSize must be an integer from 1 to 500")
	}
	return page, pageSize, nil
}

func truthyQuery(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// adminIdentity exposes the synthetic, account-bound virtual identity the relay
// presents upstream. All fields are generated (never the real client's), so the
// view is safe to surface and lets operators confirm each account looks like a
// single consistent machine.
