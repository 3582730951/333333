package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/entitlement"
	"codex-account-pool/internal/pricing"
	"codex-account-pool/internal/storage"
)

const pricingCatalogBodyLimit = 2 << 20

const (
	businessStandardFromPlusFactorMilli = int64(1250)
	capacityPriorMaxAgeSeconds          = int64(30 * 24 * 60 * 60)
)

type businessStandardFiveHourEstimate struct {
	ModelFamily       string `json:"model_family"`
	ServiceTier       string `json:"service_tier"`
	LimitMicroUSD     int64  `json:"limit_micro_usd"`
	LowerMicroUSD     *int64 `json:"lower_micro_usd,omitempty"`
	UpperMicroUSD     *int64 `json:"upper_micro_usd,omitempty"`
	SourceAccountID   string `json:"source_account_id"`
	SourceSampleCount int64  `json:"source_sample_count"`
	SourceConfidence  string `json:"source_confidence"`
	SourceUpdatedAt   int64  `json:"source_updated_at"`
}

type businessStandardFiveHourPrior struct {
	Status      string                             `json:"status"`
	Method      string                             `json:"method"`
	FactorMilli int64                              `json:"factor_milli"`
	Role        string                             `json:"role"`
	Estimates   []businessStandardFiveHourEstimate `json:"estimates"`
}

func scalePlusRemainingToBusinessStandard(amount *int64, usedRatioPPM *int64) (*int64, bool) {
	// Near exhaustion, reconstructing a full window amplifies a tiny remaining
	// value and poll jitter too aggressively. Wait for a healthier Plus sample.
	if amount == nil || usedRatioPPM == nil || *amount < 0 || *usedRatioPPM < 0 || *usedRatioPPM >= 950_000 {
		return nil, false
	}
	// Standard full-window capacity = Plus remaining / remaining_ratio * 1.25.
	// Use integer arithmetic throughout so a capacity prior never introduces a
	// float rounding path into the fixed-point money model.
	numerator := new(big.Int).SetInt64(*amount)
	numerator.Mul(numerator, big.NewInt(1_000_000))
	numerator.Mul(numerator, big.NewInt(businessStandardFromPlusFactorMilli))
	denominator := big.NewInt((1_000_000 - *usedRatioPPM) * 1000)
	// Round to the nearest micro-dollar instead of silently biasing every prior
	// downward. denominator is positive because of the guard above.
	numerator.Add(numerator, new(big.Int).Quo(new(big.Int).Set(denominator), big.NewInt(2)))
	numerator.Quo(numerator, denominator)
	if !numerator.IsInt64() {
		return nil, false
	}
	value := numerator.Int64()
	return &value, true
}

func deriveBusinessStandardFiveHourPrior(rows []storage.AccountCapacityEstimatePlan) businessStandardFiveHourPrior {
	prior := businessStandardFiveHourPrior{
		Status: "waiting_for_plus_empirical_5h_baseline", Method: "plus_empirical_5h_x_1_25",
		FactorMilli: businessStandardFromPlusFactorMilli, Role: "capacity_prior_not_entitlement",
		Estimates: []businessStandardFiveHourEstimate{},
	}
	seen := map[string]struct{}{}
	for _, row := range rows {
		if entitlement.NormalizePlanFamily(row.PlanType) != entitlement.PlanPlus ||
			(row.Confidence != "high" && row.Confidence != "medium") || row.SampleCount <= 0 ||
			row.UnitKind != "api_list_price_equivalent_micro_usd" ||
			!strings.HasPrefix(row.Method, "empirical_quota_window_") ||
			strings.TrimSpace(row.ModelFamily) == "" || row.ModelFamily == "mixed_or_unknown" ||
			(row.ServiceTier != pricing.TierDefault && row.ServiceTier != pricing.TierFast) {
			continue
		}
		center, ok := scalePlusRemainingToBusinessStandard(row.USDEquivalentMicro, row.UsedRatioPPM)
		if !ok || center == nil || *center <= 0 {
			continue
		}
		key := row.ModelFamily + "\x00" + row.ServiceTier
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		lower, _ := scalePlusRemainingToBusinessStandard(row.LowerBoundUnits, row.UsedRatioPPM)
		upper, _ := scalePlusRemainingToBusinessStandard(row.UpperBoundUnits, row.UsedRatioPPM)
		prior.Estimates = append(prior.Estimates, businessStandardFiveHourEstimate{
			ModelFamily: row.ModelFamily, ServiceTier: row.ServiceTier, LimitMicroUSD: *center,
			LowerMicroUSD: lower, UpperMicroUSD: upper, SourceAccountID: row.AccountID,
			SourceSampleCount: row.SampleCount, SourceConfidence: row.Confidence, SourceUpdatedAt: row.UpdatedAt,
		})
	}
	if len(prior.Estimates) > 0 {
		prior.Status = "derived_from_plus_empirical_5h"
	}
	return prior
}

func premiumFixtureStatus(current *storage.AccountEntitlementEvidence, conflict bool) string {
	if conflict {
		return "reviewed_mapping_active_evidence_conflict"
	}
	if hasReviewedBusinessPremiumEvidence(current) {
		return "recognized_reviewed_5x_fixture_v1"
	}
	return "reviewed_mapping_active_waiting_for_match"
}

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
	valuationFrom := now - 30*24*60*60
	valuation, err := s.store.AccountValuationTotals(r.Context(), accountID, "", valuationFrom, now)
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
	baselineRows, err := s.store.ListRecentFiveHourCapacityBaselines(r.Context(), now-capacityPriorMaxAgeSeconds, 512)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	standardFiveHourPrior := deriveBusinessStandardFiveHourPrior(baselineRows)
	planOnly := entitlement.FromPlanLabel(account.PlanType)
	premiumStatus := premiumFixtureStatus(currentEvidence, entitlementConflict)
	// A Premium 5x seat has no five-hour entitlement; suppress any Plus-derived
	// Standard prior so capacity evidence cannot accidentally display a 5h budget.
	if hasReviewedBusinessPremiumEvidence(currentEvidence) {
		standardFiveHourPrior = businessStandardFiveHourPrior{
			Status: "suppressed_for_premium_5x", Method: "premium_5x_no_five_hour_window",
			FactorMilli: businessStandardFromPlusFactorMilli, Role: "capacity_prior_not_entitlement",
			Estimates: []businessStandardFiveHourEstimate{},
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_id":                 accountID,
		"capacity_estimates":         estimates,
		"upstream_quota_windows":     rateLimits,
		"last_30d_valuation":         valuation,
		"valuation_window":           map[string]int64{"from_at": valuationFrom, "to_at": now},
		"business_standard_5h_prior": standardFiveHourPrior,
		"entitlement": map[string]interface{}{
			"plan_only": planOnly, "current_evidence": currentEvidence, "evidence": evidence,
			"conflict":                        entitlementConflict,
			"evidence_freshness":              storage.EntitlementEvidenceFreshness(evidence, now),
			"premium_fixture_status":          premiumStatus,
			"premium_fixture_mapping_version": entitlement.BusinessPremiumMappingVersion,
		},
		"plan_presentation": accountPlanPresentationFor(account.PlanType, currentEvidence, entitlementConflict),
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
