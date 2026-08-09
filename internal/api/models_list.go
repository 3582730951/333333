package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/storage"
)

// Capabilities is populated on the admin endpoint only. It is omitempty so the user
// endpoint's response stays byte-identical, and /admin/models consumers that read just
// Models (Groups.jsx) are unaffected.
type modelListResponse struct {
	Models       []string                 `json:"models"`
	Capabilities []modelCapabilitySummary `json:"capabilities,omitempty"`
	GeneratedAt  int64                    `json:"generated_at"`
}

// One row per distinct model, aggregated across the accounts that report it. The three
// availability counts and the three context-1M counts each partition Accounts exactly,
// so a reader can render either as a share of a known total.
type modelCapabilitySummary struct {
	Model                string `json:"model"`
	Accounts             int    `json:"accounts"`
	Verified             int    `json:"verified"`
	Unverified           int    `json:"unverified"`
	Unsupported          int    `json:"unsupported"`
	Context1MSupported   int    `json:"context_1m_supported"`
	Context1MUnsupported int    `json:"context_1m_unsupported"`
	Context1MUnknown     int    `json:"context_1m_unknown"`
	MaxContextWindow     int64  `json:"max_context_window"`
	LastProbeAt          int64  `json:"last_probe_at"`
}

func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	caps, err := s.store.ListCapabilities(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	display := filterDisplayCapabilities(caps, accounts)
	writeJSON(w, http.StatusOK, modelListResponse{
		Models:       sortedCapabilityModels(display, nil),
		Capabilities: summarizeCapabilityModels(display),
		GeneratedAt:  storage.Now(),
	})
}

func (s *Server) handleUserModels(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	keys, err := s.store.ListAPIKeysByUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	now := storage.Now()
	groups := map[string]bool{}
	for _, key := range keys {
		if key.Enabled && (key.ExpiresAt == 0 || key.ExpiresAt > now) && normalizeAPIKeyType(key.KeyType) == "downstream" {
			group := strings.TrimSpace(key.GroupName)
			if group == "" {
				group = s.cfg.DefaultGroup
			}
			groups[group] = true
		}
	}
	var caps []storage.ModelCapability
	for group := range groups {
		rows, listErr := s.store.ListRoutableCapabilities(r.Context(), group)
		if listErr != nil {
			writeError(w, http.StatusInternalServerError, listErr)
			return
		}
		caps = append(caps, rows...)
	}
	writeJSON(w, http.StatusOK, modelListResponse{Models: sortedCapabilityModels(caps, nil), GeneratedAt: now})
}

func filterDisplayCapabilities(caps []storage.ModelCapability, accounts []storage.Account) []storage.ModelCapability {
	byID := map[string]storage.Account{}
	for _, account := range accounts {
		byID[account.ID] = account
	}
	out := make([]storage.ModelCapability, 0, len(caps))
	for _, capRow := range caps {
		visibility := strings.ToLower(strings.TrimSpace(capRow.Visibility))
		if visibility == "hide" || visibility == "hidden" || visibility == "internal" {
			continue
		}
		if capRow.Source == "kiro_static_unknown" && !capability.KiroPlanAllowsBootstrap(byID[capRow.AccountID].PlanType, capRow.ModelSlug) {
			continue
		}
		out = append(out, capRow)
	}
	return out
}

func sortedCapabilityModels(caps []storage.ModelCapability, allowedAccounts map[string]bool) []string {
	seen := map[string]string{}
	for _, cap := range caps {
		if allowedAccounts != nil && !allowedAccounts[cap.AccountID] {
			continue
		}
		model := strings.TrimSpace(cap.ModelSlug)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, exists := seen[key]; !exists {
			seen[key] = model
		}
	}
	models := make([]string, 0, len(seen))
	for _, model := range seen {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return naturalModelLess(models[i], models[j]) })
	return models
}

// summarizeCapabilityModels aggregates rows per distinct model. It keys on the lowercased
// slug and keeps the first spelling encountered, which is the rule sortedCapabilityModels
// uses; the two therefore always describe the same set under the same names, in the same
// natural order.
func summarizeCapabilityModels(caps []storage.ModelCapability) []modelCapabilitySummary {
	order := make([]string, 0, len(caps))
	byKey := map[string]*modelCapabilitySummary{}
	for _, capRow := range caps {
		model := strings.TrimSpace(capRow.ModelSlug)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		row := byKey[key]
		if row == nil {
			row = &modelCapabilitySummary{Model: model}
			byKey[key] = row
			order = append(order, key)
		}
		row.Accounts++
		switch capRow.AvailabilityState {
		case capability.AvailabilityVerified:
			row.Verified++
		case capability.AvailabilityUnsupported:
			row.Unsupported++
		default:
			row.Unverified++
		}
		switch capRow.Context1MState {
		case capability.Context1MSupported:
			row.Context1MSupported++
		case capability.Context1MUnsupported:
			row.Context1MUnsupported++
		default:
			row.Context1MUnknown++
		}
		if window := capRow.NativeMaxContextWindow; window > row.MaxContextWindow {
			row.MaxContextWindow = window
		}
		if capRow.NativeContextWindow > row.MaxContextWindow {
			row.MaxContextWindow = capRow.NativeContextWindow
		}
		if capRow.LastProbeAt > row.LastProbeAt {
			row.LastProbeAt = capRow.LastProbeAt
		}
	}
	out := make([]modelCapabilitySummary, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	sort.Slice(out, func(i, j int) bool { return naturalModelLess(out[i].Model, out[j].Model) })
	return out
}

func naturalModelLess(a, b string) bool {
	ra, rb := []rune(strings.ToLower(a)), []rune(strings.ToLower(b))
	for ia, ib := 0, 0; ia < len(ra) && ib < len(rb); {
		if unicode.IsDigit(ra[ia]) && unicode.IsDigit(rb[ib]) {
			ja, jb := ia, ib
			for ja < len(ra) && unicode.IsDigit(ra[ja]) {
				ja++
			}
			for jb < len(rb) && unicode.IsDigit(rb[jb]) {
				jb++
			}
			na, _ := strconv.ParseUint(string(ra[ia:ja]), 10, 64)
			nb, _ := strconv.ParseUint(string(rb[ib:jb]), 10, 64)
			if na != nb {
				return na < nb
			}
			if ja-ia != jb-ib {
				return ja-ia < jb-ib
			}
			ia, ib = ja, jb
			continue
		}
		if ra[ia] != rb[ib] {
			return ra[ia] < rb[ib]
		}
		ia++
		ib++
	}
	return len(ra) < len(rb)
}
