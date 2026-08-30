package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/entitlement"
	"codex-account-pool/internal/pricing"
	"codex-account-pool/internal/storage"
)

const pricingCatalogBodyLimit = 2 << 20

func (s *Server) adminPricingCatalogs(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	versions, err := s.store.ListPricingCatalogVersions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if versions == nil {
		versions = []storage.PricingCatalogVersion{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"catalogs": versions,
		"amount_units": map[string]interface{}{
			"api_usd_equivalent": map[string]int64{"unit_scale": pricing.MicroUSDScale},
			"chatgpt_credits":    map[string]int64{"unit_scale": pricing.MilliCreditScale},
		},
	})
}

func (s *Server) adminAccountCapacity(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	account, err := s.store.GetAccount(r.Context(), accountID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	estimates, err := s.store.ListAccountCapacityEstimates(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	rateLimitsByAccount, err := s.store.ListAccountRateLimitsByAccountIDs(r.Context(), []string{accountID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	rateLimits := rateLimitsByAccount[accountID]
	for index := range rateLimits {
		rateLimits[index].Raw = ""
	}
	now := time.Now().Unix()
	valuation, err := s.store.AccountValuationTotals(r.Context(), accountID, "", now-30*24*60*60, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	evidence, err := s.store.ListAccountEntitlementEvidence(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	currentEvidence, entitlementConflict, err := s.store.CurrentAccountEntitlementEvidence(r.Context(), accountID, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	planOnly := entitlement.FromPlanLabel(account.PlanType)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_id":             accountID,
		"capacity_estimates":     estimates,
		"upstream_quota_windows": rateLimits,
		"last_30d_valuation":     valuation,
		"entitlement": map[string]interface{}{
			"plan_only": planOnly, "current_evidence": currentEvidence, "evidence": evidence,
			"conflict":               entitlementConflict,
			"premium_fixture_status": "blocked_until_reviewed_real_fixture",
		},
		"labels": map[string]string{
			"usd":     "API list-price equivalent; not an account cash balance",
			"credits": "ChatGPT Credits estimate only when subscription evidence is available",
		},
		"updated_at": now,
	})
}

func (s *Server) adminPricingCatalogAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/pricing/catalogs/"), "/")
	if path == "validate" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var catalog pricing.Catalog
		if err := decodeJSONRequestBody(r.Body, &catalog, pricingCatalogBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := pricing.ValidateCatalog(catalog); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{"valid": false, "error": err.Error()})
			return
		}
		if err := s.store.StagePricingCatalog(r.Context(), catalog); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{Action: "pricing_catalog_validate", State: "draft", Reason: "official_source_validated", Detail: "catalog_id=" + catalog.ID + " sha256=" + catalog.SHA256})
		writeJSON(w, http.StatusOK, map[string]interface{}{"valid": true, "catalog_id": catalog.ID, "sha256": catalog.SHA256, "status": "draft"})
		return
	}
	if strings.HasSuffix(path, "/activate") {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		id := strings.Trim(strings.TrimSuffix(path, "/activate"), "/")
		if id == "" || strings.Contains(id, "/") {
			writeError(w, http.StatusBadRequest, errors.New("pricing catalog id is invalid"))
			return
		}
		if err := s.store.ActivatePricingCatalog(r.Context(), id); err != nil {
			status := http.StatusConflict
			if errors.Is(err, sql.ErrNoRows) {
				status = http.StatusNotFound
			}
			writeError(w, status, err)
			return
		}
		_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{Action: "pricing_catalog_activate", State: "active", Reason: "admin_review", Detail: "catalog_id=" + id})
		writeJSON(w, http.StatusOK, map[string]interface{}{"catalog_id": id, "status": "active"})
		return
	}
	methodNotAllowed(w)
}

func (s *Server) adminUsageEventValuation(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/usage/events/"), "/")
	if !strings.HasSuffix(path, "/valuation") {
		http.NotFound(w, r)
		return
	}
	eventID := strings.Trim(strings.TrimSuffix(path, "/valuation"), "/")
	if eventID == "" || strings.Contains(eventID, "/") {
		http.NotFound(w, r)
		return
	}
	component, err := s.store.GetUsageComponent(r.Context(), eventID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	valuations, err := s.store.ListUsageValuations(r.Context(), eventID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if valuations == nil {
		valuations = []storage.UsageValuationRow{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"usage": component, "valuations": valuations})
}

// adminUsageReconcile replays a bounded batch of usage components whose
// valuation was unavailable at ingest time.  It never changes raw token
// components or historical catalog rows.
func (s *Server) adminUsageReconcile(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Limit int `json:"limit"`
	}
	if r.Body != nil && r.Body != http.NoBody {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, errors.New("invalid reconcile request"))
			return
		}
	}
	result, err := s.store.ReconcileUsageValuations(r.Context(), req.Limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{Action: "usage_valuation_reconcile", State: "completed",
		Reason: "bounded_admin_replay", Detail: "scanned=" + fmt.Sprint(result.Scanned) + " priced=" + fmt.Sprint(result.Priced)})
	writeJSON(w, http.StatusOK, result)
}
