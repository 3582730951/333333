package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"codex-account-pool/internal/pricing"
)

var ErrNoActivePricingCatalog = errors.New("no active pricing catalog is effective for the usage event")

// ensurePricingCatalogExpiryColumn is an additive SQLite upgrade. The original
// pricing v1 table is already deployed in the wild, so changing its CREATE
// statement alone would leave existing databases without the validity bound.
// Keep this check idempotent and avoid SQLite's version-dependent ADD COLUMN IF
// NOT EXISTS syntax.
func (s *Store) ensurePricingCatalogExpiryColumn(ctx context.Context) error {
	if s == nil || s.driver == "postgres" {
		return nil
	}
	rows, err := s.rdb.QueryContext(ctx, `PRAGMA table_info(pricing_catalog_versions)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if strings.EqualFold(name, "expires_at") {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE pricing_catalog_versions ADD COLUMN expires_at BIGINT NOT NULL DEFAULT 0`)
	return err
}

// ensureAccountCapacitySourcePlanColumn keeps the Plus baseline fail-closed on
// upgraded SQLite databases. A current accounts.plan_type join cannot prove
// what plan produced a historical estimate after an account changes plans, so
// only newly sampled rows carry immutable source-plan provenance.
func (s *Store) ensureAccountCapacitySourcePlanColumn(ctx context.Context) error {
	if s == nil || s.driver == "postgres" {
		return nil
	}
	rows, err := s.rdb.QueryContext(ctx, `PRAGMA table_info(account_capacity_estimates)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if strings.EqualFold(name, "source_plan_type") {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !found {
		if _, err = s.db.ExecContext(ctx, `ALTER TABLE account_capacity_estimates ADD COLUMN source_plan_type TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_capacity_limiter_updated ON account_capacity_estimates(limiter_kind,updated_at)`)
	return err
}

// StagePricingCatalog validates and persists a new immutable draft. Reusing an
// ID with different content is always rejected, including while still a draft.
func (s *Store) StagePricingCatalog(ctx context.Context, catalog pricing.Catalog) error {
	if err := pricing.ValidateCatalog(catalog); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existingSHA string
	err = tx.QueryRowContext(ctx, `SELECT sha256 FROM pricing_catalog_versions WHERE id=?`, catalog.ID).Scan(&existingSHA)
	if err == nil {
		if !strings.EqualFold(existingSHA, catalog.SHA256) {
			return fmt.Errorf("immutable pricing catalog id already has different content")
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO pricing_catalog_versions(id,catalog_kind,source_url,effective_at,expires_at,fetched_at,currency,sha256,status,raw_snapshot_ref)
VALUES(?,?,?,?,?,?,?,?,?,?)`, catalog.ID, catalog.CatalogKind, catalog.SourceURL, catalog.EffectiveAt, catalog.ExpiresAt, catalog.FetchedAt,
		catalog.Currency, catalog.SHA256, "draft", catalog.SnapshotRef); err != nil {
		return err
	}
	for _, rate := range catalog.Rates {
		if _, err = tx.ExecContext(ctx, `INSERT INTO pricing_rates(catalog_id,product_surface,provider,model_pattern,service_tier,context_band,unit_kind,
input_rate_units,cached_read_rate_units,cache_write_rate_units,output_rate_units,per_request_rate_units,multiplier_milli,source_line_ref)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, catalog.ID, string(rate.ProductSurface), rate.Provider, rate.Model, rate.ServiceTier,
			rate.ContextBand, string(rate.UnitKind), rate.InputRateUnits, rate.CachedRateUnits, nullableInt64(rate.CacheWriteRate),
			rate.OutputRateUnits, rate.PerRequestUnits, rate.MultiplierMilli, rate.SourceLineRef); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ActivatePricingCatalog(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("pricing catalog id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM pricing_catalog_versions WHERE id=?`, id).Scan(&status); err != nil {
		return err
	}
	if status == "active" {
		return tx.Commit()
	}
	if status != "draft" {
		return fmt.Errorf("pricing catalog status %q cannot be activated", status)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE pricing_catalog_versions SET status='active' WHERE id=? AND status='draft'`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetPricingCatalog(ctx context.Context, id string) (pricing.Catalog, error) {
	var catalog pricing.Catalog
	err := s.rdb.QueryRowContext(ctx, `SELECT id,catalog_kind,source_url,effective_at,expires_at,fetched_at,currency,status,raw_snapshot_ref,sha256
FROM pricing_catalog_versions WHERE id=?`, strings.TrimSpace(id)).Scan(&catalog.ID, &catalog.CatalogKind, &catalog.SourceURL,
		&catalog.EffectiveAt, &catalog.ExpiresAt, &catalog.FetchedAt, &catalog.Currency, &catalog.Status, &catalog.SnapshotRef, &catalog.SHA256)
	if err != nil {
		return pricing.Catalog{}, err
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT product_surface,provider,model_pattern,service_tier,context_band,unit_kind,
input_rate_units,cached_read_rate_units,cache_write_rate_units,output_rate_units,per_request_rate_units,multiplier_milli,source_line_ref
FROM pricing_rates WHERE catalog_id=? ORDER BY product_surface,model_pattern,service_tier,context_band,unit_kind`, catalog.ID)
	if err != nil {
		return pricing.Catalog{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var rate pricing.Rate
		var surface, unit string
		var cacheWrite sql.NullInt64
		if err := rows.Scan(&surface, &rate.Provider, &rate.Model, &rate.ServiceTier, &rate.ContextBand, &unit,
			&rate.InputRateUnits, &rate.CachedRateUnits, &cacheWrite, &rate.OutputRateUnits, &rate.PerRequestUnits,
			&rate.MultiplierMilli, &rate.SourceLineRef); err != nil {
			return pricing.Catalog{}, err
		}
		rate.ProductSurface = pricing.ProductSurface(surface)
		rate.UnitKind = pricing.UnitKind(unit)
		if cacheWrite.Valid {
			rate.CacheWriteRate = &cacheWrite.Int64
		}
		catalog.Rates = append(catalog.Rates, rate)
	}
	return catalog, rows.Err()
}

// ActivePricingCatalogAt resolves the immutable catalog that was effective when
// an event occurred. Published catalogs intentionally remain active: selecting
// the greatest effective_at not after the event preserves historical replay
// while allowing a newly activated version to price only subsequent events.
func (s *Store) ActivePricingCatalogAt(ctx context.Context, occurredAt int64) (pricing.Catalog, error) {
	if occurredAt <= 0 {
		occurredAt = Now()
	}
	var id string
	err := s.rdb.QueryRowContext(ctx, `SELECT id FROM pricing_catalog_versions
WHERE status='active' AND effective_at<=? AND (expires_at=0 OR expires_at>?)
ORDER BY effective_at DESC,id DESC LIMIT 1`, occurredAt, occurredAt).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return pricing.Catalog{}, ErrNoActivePricingCatalog
	}
	if err != nil {
		return pricing.Catalog{}, err
	}
	return s.GetPricingCatalog(ctx, id)
}

type UsageComponentRow struct {
	UsageEventID         string          `json:"usage_event_id"`
	AccountID            string          `json:"account_id"`
	UserID               string          `json:"user_id"`
	ProductSurface       string          `json:"product_surface"`
	RequestedModel       string          `json:"requested_model"`
	ResolvedModel        string          `json:"resolved_model"`
	UpstreamModel        string          `json:"upstream_model"`
	RequestedServiceTier string          `json:"requested_service_tier"`
	ForwardedServiceTier string          `json:"forwarded_service_tier"`
	ObservedServiceTier  string          `json:"observed_service_tier"`
	BilledServiceTier    string          `json:"billed_service_tier"`
	BilledTierReason     string          `json:"billed_tier_reason"`
	UsageSource          string          `json:"usage_source"`
	SettlementState      string          `json:"settlement_state"`
	FieldPresence        json.RawMessage `json:"field_presence"`
	InputTotal           *int64          `json:"input_total"`
	InputUncached        *int64          `json:"input_uncached"`
	CachedRead           *int64          `json:"cached_read"`
	CacheWrite           *int64          `json:"cache_write"`
	OutputTotal          *int64          `json:"output_total"`
	OutputReasoning      *int64          `json:"output_reasoning"`
	SettledAt            *int64          `json:"settled_at"`
	Estimated            int64           `json:"estimated"`
	CreatedAt            int64           `json:"created_at"`
	UpdatedAt            int64           `json:"updated_at"`
	IntegrityError       string          `json:"integrity_error,omitempty"`
}

func nullableScanPointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	copy := value.Int64
	return &copy
}

const usageComponentColumns = `usage_event_id,account_id,user_id,product_surface,requested_model,resolved_model,upstream_model,
requested_service_tier,forwarded_service_tier,observed_service_tier,billed_service_tier,billed_tier_reason,
input_total,input_uncached,cached_read,cache_write,output_total,output_reasoning,field_presence_json,usage_source,settlement_state,
estimated,integrity_error,settled_at,created_at,updated_at`

func scanUsageComponent(scan func(...interface{}) error) (UsageComponentRow, error) {
	var row UsageComponentRow
	var inputTotal, inputUncached, cachedRead, cacheWrite, outputTotal, outputReasoning, settledAt sql.NullInt64
	var fieldPresence string
	err := scan(
		&row.UsageEventID, &row.AccountID, &row.UserID, &row.ProductSurface, &row.RequestedModel, &row.ResolvedModel, &row.UpstreamModel,
		&row.RequestedServiceTier, &row.ForwardedServiceTier, &row.ObservedServiceTier, &row.BilledServiceTier, &row.BilledTierReason,
		&inputTotal, &inputUncached, &cachedRead, &cacheWrite, &outputTotal, &outputReasoning, &fieldPresence, &row.UsageSource,
		&row.SettlementState, &row.Estimated, &row.IntegrityError, &settledAt, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		return UsageComponentRow{}, err
	}
	row.InputTotal, row.InputUncached, row.CachedRead, row.CacheWrite = nullableScanPointer(inputTotal), nullableScanPointer(inputUncached), nullableScanPointer(cachedRead), nullableScanPointer(cacheWrite)
	row.OutputTotal, row.OutputReasoning, row.SettledAt = nullableScanPointer(outputTotal), nullableScanPointer(outputReasoning), nullableScanPointer(settledAt)
	row.FieldPresence = json.RawMessage(fieldPresence)
	return row, nil
}

func (s *Store) GetUsageComponent(ctx context.Context, eventID string) (UsageComponentRow, error) {
	return scanUsageComponent(s.rdb.QueryRowContext(ctx, `SELECT `+usageComponentColumns+` FROM usage_components WHERE usage_event_id=?`, strings.TrimSpace(eventID)).Scan)
}

type UserUsageComponentFilter struct {
	UserID, Model, ServiceTier string
	From, To                   int64
	CursorAt                   int64
	CursorEventID              string
	Limit                      int
}

func (s *Store) ListUserUsageComponents(ctx context.Context, filter UserUsageComponentFilter) ([]UsageComponentRow, bool, error) {
	userID := strings.TrimSpace(filter.UserID)
	if userID == "" {
		return nil, false, fmt.Errorf("user id is required")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	where := []string{"user_id=?"}
	args := []interface{}{userID}
	if filter.From > 0 {
		where, args = append(where, "created_at>=?"), append(args, filter.From)
	}
	if filter.To > 0 {
		where, args = append(where, "created_at<=?"), append(args, filter.To)
	}
	if model := strings.TrimSpace(filter.Model); model != "" {
		where, args = append(where, "(requested_model=? OR resolved_model=? OR upstream_model=?)"), append(args, model, model, model)
	}
	if tier := strings.TrimSpace(filter.ServiceTier); tier != "" {
		where, args = append(where, "billed_service_tier=?"), append(args, tier)
	}
	if filter.CursorAt > 0 && strings.TrimSpace(filter.CursorEventID) != "" {
		where, args = append(where, "(created_at<? OR (created_at=? AND usage_event_id<?))"), append(args, filter.CursorAt, filter.CursorAt, filter.CursorEventID)
	}
	args = append(args, limit+1)
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+usageComponentColumns+` FROM usage_components WHERE `+strings.Join(where, " AND ")+`
ORDER BY created_at DESC,usage_event_id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	result := make([]UsageComponentRow, 0, limit+1)
	for rows.Next() {
		row, scanErr := scanUsageComponent(rows.Scan)
		if scanErr != nil {
			return nil, false, scanErr
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(result) > limit
	if hasMore {
		result = result[:limit]
	}
	return result, hasMore, nil
}

func (s *Store) ListUsageValuationsByEventIDs(ctx context.Context, eventIDs []string) (map[string][]UsageValuationRow, error) {
	result := make(map[string][]UsageValuationRow, len(eventIDs))
	ids := make([]string, 0, len(eventIDs))
	seen := make(map[string]struct{}, len(eventIDs))
	for _, raw := range eventIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		result[id] = []UsageValuationRow{}
	}
	if len(ids) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for index, id := range ids {
		placeholders[index], args[index] = "?", id
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT v.usage_event_id,v.valuation_kind,v.catalog_id,p.effective_at,p.source_url,
v.amount_units,v.unit_scale,v.confidence,v.breakdown_json,v.computed_at
FROM usage_valuations v JOIN pricing_catalog_versions p ON p.id=v.catalog_id
WHERE v.usage_event_id IN (`+strings.Join(placeholders, ",")+`)
ORDER BY v.usage_event_id,v.valuation_kind,p.effective_at DESC,v.catalog_id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item UsageValuationRow
		var amount sql.NullInt64
		var breakdown string
		if err := rows.Scan(&item.UsageEventID, &item.ValuationKind, &item.CatalogID, &item.CatalogEffectiveAt, &item.CatalogSourceURL,
			&amount, &item.UnitScale, &item.Confidence, &breakdown, &item.ComputedAt); err != nil {
			return nil, err
		}
		if amount.Valid {
			item.AmountUnits = nullableScanPointer(amount)
		}
		item.BreakdownJSON = json.RawMessage(breakdown)
		result[item.UsageEventID] = append(result[item.UsageEventID], item)
	}
	return result, rows.Err()
}

type AccountValuationSummary struct {
	APIMicroUSDSettled             int64 `json:"api_micro_usd_settled"`
	APIMicroUSDProvisional         int64 `json:"api_micro_usd_provisional"`
	ChatGPTMilliCreditsSettled     int64 `json:"chatgpt_milli_credits_settled"`
	ChatGPTMilliCreditsProvisional int64 `json:"chatgpt_milli_credits_provisional"`
	SettledEvents                  int64 `json:"settled_events"`
	ProvisionalEvents              int64 `json:"provisional_events"`
	UnavailableEvents              int64 `json:"unavailable_events"`
	UpdatedAt                      int64 `json:"updated_at"`
}

func (s *Store) AccountValuationTotals(ctx context.Context, accountID, userID string, from, to int64) (AccountValuationSummary, error) {
	where := `c.created_at>=? AND c.created_at<=?`
	args := []interface{}{from, to}
	if strings.TrimSpace(accountID) != "" {
		where += ` AND c.account_id=?`
		args = append(args, strings.TrimSpace(accountID))
	}
	if strings.TrimSpace(userID) != "" {
		where += ` AND c.user_id=?`
		args = append(args, strings.TrimSpace(userID))
	}
	var result AccountValuationSummary
	err := s.rdb.QueryRowContext(ctx, `SELECT
COALESCE(SUM(CASE WHEN v.valuation_kind='api_usd_equivalent' AND v.confidence='settled' THEN v.amount_units ELSE 0 END),0),
COALESCE(SUM(CASE WHEN v.valuation_kind='api_usd_equivalent' AND v.confidence='provisional' THEN v.amount_units ELSE 0 END),0),
COALESCE(SUM(CASE WHEN v.valuation_kind='chatgpt_credits' AND v.confidence='settled' THEN v.amount_units ELSE 0 END),0),
COALESCE(SUM(CASE WHEN v.valuation_kind='chatgpt_credits' AND v.confidence='provisional' THEN v.amount_units ELSE 0 END),0),
COUNT(DISTINCT CASE WHEN v.confidence='settled' THEN c.usage_event_id END),
COUNT(DISTINCT CASE WHEN v.confidence='provisional' THEN c.usage_event_id END),
COUNT(DISTINCT CASE WHEN v.confidence='unavailable' AND
 (v.valuation_kind='api_usd_equivalent' OR (v.valuation_kind='chatgpt_credits' AND c.product_surface='chatgpt_subscription'))
 THEN c.usage_event_id END),
COALESCE(MAX(v.computed_at),0)
FROM usage_components c LEFT JOIN usage_valuations v ON v.usage_event_id=c.usage_event_id
 AND v.catalog_id=(SELECT v2.catalog_id FROM usage_valuations v2
  JOIN pricing_catalog_versions p2 ON p2.id=v2.catalog_id
  WHERE v2.usage_event_id=c.usage_event_id AND v2.valuation_kind=v.valuation_kind AND p2.effective_at<=c.created_at
  ORDER BY p2.effective_at DESC,v2.catalog_id DESC LIMIT 1)
WHERE `+where, args...).Scan(
		&result.APIMicroUSDSettled, &result.APIMicroUSDProvisional, &result.ChatGPTMilliCreditsSettled,
		&result.ChatGPTMilliCreditsProvisional, &result.SettledEvents, &result.ProvisionalEvents,
		&result.UnavailableEvents, &result.UpdatedAt)
	return result, err
}

type AccountUsageValuationWindow struct {
	TotalMicroUSD       int64
	ProvisionalMicroUSD int64
	TotalEvents         int64
	UnavailableEvents   int64
}

// AccountUsageValuationWindowSummary returns the versioned fixed-point API
// list-price equivalent for a quota window. It never substitutes zero for a
// valuation whose rate or token component is unavailable.
func (s *Store) AccountUsageValuationWindowSummary(ctx context.Context, accountID string, from, to int64) (AccountUsageValuationWindow, error) {
	var result AccountUsageValuationWindow
	err := s.rdb.QueryRowContext(ctx, `SELECT
COALESCE(SUM(CASE WHEN v.amount_units IS NOT NULL THEN v.amount_units ELSE 0 END),0),
COALESCE(SUM(CASE WHEN v.amount_units IS NOT NULL AND v.confidence<>'settled' THEN v.amount_units ELSE 0 END),0),
COUNT(DISTINCT c.usage_event_id),
COUNT(DISTINCT CASE WHEN v.amount_units IS NULL OR v.confidence='unavailable' THEN c.usage_event_id END)
FROM usage_components c
LEFT JOIN usage_valuations v ON v.usage_event_id=c.usage_event_id AND v.valuation_kind='api_usd_equivalent'
 AND v.catalog_id=(SELECT v2.catalog_id FROM usage_valuations v2
  JOIN pricing_catalog_versions p2 ON p2.id=v2.catalog_id
  WHERE v2.usage_event_id=c.usage_event_id AND v2.valuation_kind='api_usd_equivalent' AND p2.effective_at<=c.created_at
  ORDER BY p2.effective_at DESC,v2.catalog_id DESC LIMIT 1)
WHERE c.account_id=? AND c.created_at>=? AND c.created_at<=?`, strings.TrimSpace(accountID), from, to).Scan(
		&result.TotalMicroUSD, &result.ProvisionalMicroUSD, &result.TotalEvents, &result.UnavailableEvents)
	return result, err
}

// UsageValuationReconcileResult describes a bounded replay of usage components
// that were recorded before an audited pricing catalog was available.  It is
// intentionally idempotent: the event id and catalog id form the valuation key,
// and a settled valuation can never be replaced by a provisional one.
type UsageValuationReconcileResult struct {
	Scanned     int `json:"scanned"`
	Priced      int `json:"priced"`
	Unavailable int `json:"unavailable"`
	Skipped     int `json:"skipped"`
}

// ReconcileUsageValuations prices a small, deterministic batch.  Missing
// components/models remain unavailable; they are not assigned a family price.
// A future catalog can be replayed safely because historical events always use
// the catalog effective at their original created_at.
func (s *Store) ReconcileUsageValuations(ctx context.Context, limit int) (UsageValuationReconcileResult, error) {
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT c.usage_event_id
FROM usage_components c
WHERE NOT EXISTS (SELECT 1 FROM usage_valuations v WHERE v.usage_event_id=c.usage_event_id AND v.confidence='settled')
ORDER BY c.created_at,c.usage_event_id LIMIT ?`, limit)
	if err != nil {
		return UsageValuationReconcileResult{}, err
	}
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return UsageValuationReconcileResult{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return UsageValuationReconcileResult{}, err
	}
	result := UsageValuationReconcileResult{}
	for _, id := range ids {
		result.Scanned++
		component, err := s.GetUsageComponent(ctx, id)
		if err != nil {
			result.Skipped++
			continue
		}
		catalog, err := s.ActivePricingCatalogAt(ctx, component.CreatedAt)
		if errors.Is(err, ErrNoActivePricingCatalog) {
			result.Skipped++
			continue
		}
		if err != nil {
			return result, err
		}
		usageValue := pricing.TokenUsage{}
		complete := component.InputTotal != nil && component.InputUncached != nil && component.CachedRead != nil &&
			component.CacheWrite != nil && component.OutputTotal != nil
		if complete {
			usageValue = pricing.TokenUsage{InputTotal: *component.InputTotal, InputUncached: *component.InputUncached,
				CachedRead: *component.CachedRead, CacheWrite: *component.CacheWrite, OutputTotal: *component.OutputTotal}
			if component.OutputReasoning != nil {
				usageValue.OutputReasoning = *component.OutputReasoning
			}
		}
		tier := pricing.ResolveServiceTier(component.RequestedServiceTier, component.ForwardedServiceTier, component.ObservedServiceTier)
		if component.BilledServiceTier != "" && component.BilledServiceTier != pricing.TierUnknown {
			tier.Billed = component.BilledServiceTier
		}
		model := firstNonEmptyStorage(component.UpstreamModel, component.ResolvedModel, component.RequestedModel)
		apiValuation := pricing.UnavailableValuation("api_usd_equivalent", catalog.ID, pricing.MicroUSDScale, "usage_components_incomplete")
		if complete && component.ProductSurface == string(pricing.SurfaceOpenAIAPI) {
			apiValuation = catalog.Value(pricing.SurfaceOpenAIAPI, model, tier.Billed, usageValue)
		}
		creditValuation := pricing.UnavailableValuation("chatgpt_credits", catalog.ID, pricing.MilliCreditScale, "not_chatgpt_subscription_surface")
		if complete && component.ProductSurface == string(pricing.SurfaceChatGPTSubscription) {
			creditValuation = catalog.Value(pricing.SurfaceChatGPTSubscription, model, tier.Billed, usageValue)
		}
		if component.SettlementState != "settled" {
			if apiValuation.Confidence == "settled" {
				apiValuation.Confidence = "provisional"
			}
			if creditValuation.Confidence == "settled" {
				creditValuation.Confidence = "provisional"
			}
		}
		inserted, err := s.persistUsageValuations(ctx, id, catalog.ID, apiValuation, creditValuation)
		if err != nil {
			return result, err
		}
		if !inserted {
			result.Skipped++
			continue
		}
		if apiValuation.Confidence == "settled" || creditValuation.Confidence == "settled" {
			result.Priced++
		} else {
			result.Unavailable++
		}
	}
	return result, nil
}

func (s *Store) persistUsageValuations(ctx context.Context, eventID, catalogID string, values ...pricing.Valuation) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	inserted := false
	for _, value := range values {
		breakdown, _ := json.Marshal(map[string]interface{}{"valuation": value, "reconciled": true})
		result, err := tx.ExecContext(ctx, `INSERT INTO usage_valuations(usage_event_id,valuation_kind,catalog_id,amount_units,unit_scale,confidence,breakdown_json,computed_at)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(usage_event_id,valuation_kind,catalog_id) DO UPDATE SET amount_units=excluded.amount_units,
unit_scale=excluded.unit_scale,confidence=excluded.confidence,breakdown_json=excluded.breakdown_json,computed_at=excluded.computed_at
WHERE usage_valuations.confidence IN ('unavailable','partial','provisional') AND excluded.confidence='settled'`,
			eventID, value.Kind, catalogID, nullableInt64(value.AmountUnits), value.UnitScale, value.Confidence, string(breakdown), Now())
		if err != nil {
			return false, err
		}
		if changed, _ := result.RowsAffected(); changed > 0 {
			inserted = true
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return inserted, nil
}

type AccountUsageWindowDimensions struct {
	ModelFamily     string
	ServiceTier     string
	Homogeneous     bool
	UnsettledEvents int64
}

func (s *Store) AccountUsageWindowDimensionSummary(ctx context.Context, accountID string, from, to int64) (AccountUsageWindowDimensions, error) {
	var modelCount, tierCount int64
	var model, tier string
	var result AccountUsageWindowDimensions
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(DISTINCT upstream_model),COALESCE(MIN(upstream_model),''),
COUNT(DISTINCT billed_service_tier),COALESCE(MIN(billed_service_tier),''),
COALESCE(SUM(CASE WHEN settlement_state<>'settled' THEN 1 ELSE 0 END),0)
FROM usage_components WHERE account_id=? AND created_at>=? AND created_at<=?`, strings.TrimSpace(accountID), from, to).Scan(
		&modelCount, &model, &tierCount, &tier, &result.UnsettledEvents)
	if err != nil {
		return AccountUsageWindowDimensions{}, err
	}
	canonical, modelKnown := pricing.CanonicalModel(model)
	tierKnown := tier == pricing.TierDefault || tier == pricing.TierFast
	result.Homogeneous = modelCount == 1 && tierCount == 1 && modelKnown && tierKnown && result.UnsettledEvents == 0
	if result.Homogeneous {
		result.ModelFamily, result.ServiceTier = canonical, tier
	} else {
		result.ModelFamily, result.ServiceTier = "mixed_or_unknown", "mixed_or_unknown"
	}
	return result, nil
}

type AccountCapacityEstimate struct {
	AccountID             string `json:"account_id"`
	LimiterKind           string `json:"limiter_kind"`
	ModelFamily           string `json:"model_family"`
	ServiceTier           string `json:"service_tier"`
	SourcePlanType        string `json:"source_plan_type"`
	CycleStart            int64  `json:"cycle_start"`
	CycleEnd              int64  `json:"cycle_end"`
	UsedRatioPPM          *int64 `json:"used_ratio_ppm"`
	RemainingUnits        *int64 `json:"remaining_units"`
	UnitKind              string `json:"unit_kind"`
	USDEquivalentMicro    *int64 `json:"usd_equivalent_micro"`
	CreditsRemainingMilli *int64 `json:"credits_remaining_milli"`
	Method                string `json:"method"`
	SampleCount           int64  `json:"sample_count"`
	Confidence            string `json:"confidence"`
	LowerBoundUnits       *int64 `json:"lower_bound_units"`
	UpperBoundUnits       *int64 `json:"upper_bound_units"`
	UpdatedAt             int64  `json:"updated_at"`
}

// AccountCapacityEstimatePlan joins an empirical capacity estimate to the raw
// account plan that produced it. It is used only to build cross-account capacity
// priors; the raw plan still has to be normalized by the caller and never becomes
// seat entitlement evidence.
type AccountCapacityEstimatePlan struct {
	AccountCapacityEstimate
	PlanType string `json:"plan_type"`
}

func (s *Store) UpsertAccountCapacityEstimate(ctx context.Context, estimate AccountCapacityEstimate) error {
	if estimate.UpdatedAt == 0 {
		estimate.UpdatedAt = Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_capacity_estimates(account_id,limiter_kind,model_family,service_tier,source_plan_type,cycle_start,cycle_end,
used_ratio_ppm,remaining_units,unit_kind,usd_equivalent_micro,credits_remaining_milli,method,sample_count,confidence,lower_bound_units,upper_bound_units,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(account_id,limiter_kind,model_family,service_tier,cycle_start) DO UPDATE SET cycle_end=excluded.cycle_end,
used_ratio_ppm=excluded.used_ratio_ppm,remaining_units=excluded.remaining_units,unit_kind=excluded.unit_kind,
usd_equivalent_micro=excluded.usd_equivalent_micro,credits_remaining_milli=excluded.credits_remaining_milli,method=excluded.method,
sample_count=excluded.sample_count,confidence=excluded.confidence,lower_bound_units=excluded.lower_bound_units,
upper_bound_units=excluded.upper_bound_units,
source_plan_type=CASE WHEN account_capacity_estimates.source_plan_type=excluded.source_plan_type THEN account_capacity_estimates.source_plan_type ELSE '' END,
updated_at=excluded.updated_at`, estimate.AccountID, estimate.LimiterKind,
		estimate.ModelFamily, estimate.ServiceTier, estimate.SourcePlanType, estimate.CycleStart, estimate.CycleEnd, nullableInt64(estimate.UsedRatioPPM),
		nullableInt64(estimate.RemainingUnits), estimate.UnitKind, nullableInt64(estimate.USDEquivalentMicro),
		nullableInt64(estimate.CreditsRemainingMilli), estimate.Method, estimate.SampleCount, estimate.Confidence,
		nullableInt64(estimate.LowerBoundUnits), nullableInt64(estimate.UpperBoundUnits), estimate.UpdatedAt)
	return err
}

func (s *Store) ListAccountCapacityEstimates(ctx context.Context, accountID string) ([]AccountCapacityEstimate, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id,limiter_kind,model_family,service_tier,source_plan_type,cycle_start,cycle_end,used_ratio_ppm,
remaining_units,unit_kind,usd_equivalent_micro,credits_remaining_milli,method,sample_count,confidence,lower_bound_units,upper_bound_units,updated_at
FROM account_capacity_estimates WHERE account_id=? ORDER BY updated_at DESC,limiter_kind,model_family,service_tier`, strings.TrimSpace(accountID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AccountCapacityEstimate{}
	for rows.Next() {
		var item AccountCapacityEstimate
		var usedRatio, remaining, usd, credits, lower, upper sql.NullInt64
		if err := rows.Scan(&item.AccountID, &item.LimiterKind, &item.ModelFamily, &item.ServiceTier, &item.SourcePlanType, &item.CycleStart,
			&item.CycleEnd, &usedRatio, &remaining, &item.UnitKind, &usd, &credits, &item.Method, &item.SampleCount,
			&item.Confidence, &lower, &upper, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.UsedRatioPPM, item.RemainingUnits = nullableScanPointer(usedRatio), nullableScanPointer(remaining)
		item.USDEquivalentMicro, item.CreditsRemainingMilli = nullableScanPointer(usd), nullableScanPointer(credits)
		item.LowerBoundUnits, item.UpperBoundUnits = nullableScanPointer(lower), nullableScanPointer(upper)
		result = append(result, item)
	}
	return result, rows.Err()
}

// ListRecentFiveHourCapacityBaselines returns a bounded set of empirical 5h
// estimates together with their sampling-time plan labels. The API layer applies the
// closed plan-family normalizer and fixed-point calibration policy; storage does
// not infer Plus, Team, or Premium from strings.
func (s *Store) ListRecentFiveHourCapacityBaselines(ctx context.Context, since int64, limit int) ([]AccountCapacityEstimatePlan, error) {
	if limit <= 0 {
		limit = 256
	}
	if limit > 1024 {
		limit = 1024
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT e.account_id,e.limiter_kind,e.model_family,e.service_tier,e.cycle_start,e.cycle_end,
e.used_ratio_ppm,e.remaining_units,e.unit_kind,e.usd_equivalent_micro,e.credits_remaining_milli,e.method,e.sample_count,
e.confidence,e.lower_bound_units,e.upper_bound_units,e.updated_at,e.source_plan_type
FROM account_capacity_estimates e
WHERE e.limiter_kind='5h' AND e.updated_at>=? AND LOWER(e.source_plan_type) LIKE '%plus%'
AND e.confidence IN ('high','medium') AND e.sample_count>0
AND e.unit_kind='api_list_price_equivalent_micro_usd' AND e.method LIKE 'empirical_quota_window_%'
AND e.usd_equivalent_micro IS NOT NULL AND e.usd_equivalent_micro>0
AND e.used_ratio_ppm IS NOT NULL AND e.used_ratio_ppm>=0 AND e.used_ratio_ppm<950000
AND e.model_family<>'' AND e.model_family<>'mixed_or_unknown' AND e.service_tier IN ('default','fast')
ORDER BY CASE e.confidence WHEN 'high' THEN 3 WHEN 'medium' THEN 2 ELSE 1 END DESC,
e.sample_count DESC,e.updated_at DESC,e.account_id LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AccountCapacityEstimatePlan{}
	for rows.Next() {
		var item AccountCapacityEstimatePlan
		var usedRatio, remaining, usd, credits, lower, upper sql.NullInt64
		if err := rows.Scan(&item.AccountID, &item.LimiterKind, &item.ModelFamily, &item.ServiceTier, &item.CycleStart,
			&item.CycleEnd, &usedRatio, &remaining, &item.UnitKind, &usd, &credits, &item.Method, &item.SampleCount,
			&item.Confidence, &lower, &upper, &item.UpdatedAt, &item.PlanType); err != nil {
			return nil, err
		}
		item.UsedRatioPPM, item.RemainingUnits = nullableScanPointer(usedRatio), nullableScanPointer(remaining)
		item.USDEquivalentMicro, item.CreditsRemainingMilli = nullableScanPointer(usd), nullableScanPointer(credits)
		item.LowerBoundUnits, item.UpperBoundUnits = nullableScanPointer(lower), nullableScanPointer(upper)
		result = append(result, item)
	}
	return result, rows.Err()
}
