package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"codex-account-pool/internal/pricing"
	"codex-account-pool/internal/usage"
)

const pricingSchemaSQL = `
CREATE TABLE IF NOT EXISTS pricing_catalog_versions (
  id TEXT PRIMARY KEY,
  catalog_kind TEXT NOT NULL,
  source_url TEXT NOT NULL,
  effective_at BIGINT NOT NULL,
  fetched_at BIGINT NOT NULL,
  currency TEXT NOT NULL,
  sha256 TEXT NOT NULL,
  status TEXT NOT NULL,
  raw_snapshot_ref TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS pricing_rates (
  catalog_id TEXT NOT NULL,
  product_surface TEXT NOT NULL,
  provider TEXT NOT NULL,
  model_pattern TEXT NOT NULL,
  service_tier TEXT NOT NULL,
  context_band TEXT NOT NULL,
  unit_kind TEXT NOT NULL,
  input_rate_units BIGINT,
  cached_read_rate_units BIGINT,
  cache_write_rate_units BIGINT,
  output_rate_units BIGINT,
  per_request_rate_units BIGINT,
  multiplier_milli BIGINT NOT NULL DEFAULT 1000,
  source_line_ref TEXT NOT NULL,
  PRIMARY KEY(catalog_id,product_surface,provider,model_pattern,service_tier,context_band,unit_kind),
  FOREIGN KEY(catalog_id) REFERENCES pricing_catalog_versions(id)
);
CREATE TABLE IF NOT EXISTS usage_components (
  usage_event_id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL DEFAULT '',
  user_id TEXT NOT NULL DEFAULT '',
  api_key_hash TEXT NOT NULL DEFAULT '',
  upstream_attempt_id TEXT NOT NULL DEFAULT '',
  product_surface TEXT NOT NULL DEFAULT 'third_party',
  requested_model TEXT NOT NULL DEFAULT '',
  resolved_model TEXT NOT NULL DEFAULT '',
  upstream_model TEXT NOT NULL DEFAULT '',
  requested_service_tier TEXT NOT NULL DEFAULT 'default',
  forwarded_service_tier TEXT NOT NULL DEFAULT 'default',
  observed_service_tier TEXT NOT NULL DEFAULT 'absent',
  billed_service_tier TEXT NOT NULL DEFAULT 'unknown',
  billed_tier_reason TEXT NOT NULL DEFAULT '',
  input_total BIGINT,
  input_uncached BIGINT,
  cached_read BIGINT,
  cache_write BIGINT,
  output_total BIGINT,
  output_reasoning BIGINT,
  image_input BIGINT,
  image_output BIGINT,
  audio_input BIGINT,
  audio_output BIGINT,
  tool_calls BIGINT,
  field_presence_json TEXT NOT NULL DEFAULT '{}',
  usage_source TEXT NOT NULL DEFAULT '',
  settlement_state TEXT NOT NULL DEFAULT 'unsettled',
  estimated INTEGER NOT NULL DEFAULT 0,
  integrity_error TEXT NOT NULL DEFAULT '',
  settled_at BIGINT,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_components_account_created ON usage_components(account_id,created_at);
CREATE INDEX IF NOT EXISTS idx_usage_components_user_created ON usage_components(user_id,created_at);
CREATE INDEX IF NOT EXISTS idx_usage_components_settlement ON usage_components(settlement_state,updated_at);
CREATE TABLE IF NOT EXISTS usage_valuations (
  usage_event_id TEXT NOT NULL,
  valuation_kind TEXT NOT NULL,
  catalog_id TEXT NOT NULL,
  amount_units BIGINT,
  unit_scale BIGINT NOT NULL,
  confidence TEXT NOT NULL,
  breakdown_json TEXT NOT NULL,
  computed_at BIGINT NOT NULL,
  PRIMARY KEY(usage_event_id,valuation_kind,catalog_id),
  FOREIGN KEY(catalog_id) REFERENCES pricing_catalog_versions(id)
);
CREATE INDEX IF NOT EXISTS idx_usage_valuations_kind_time ON usage_valuations(valuation_kind,computed_at);
CREATE TABLE IF NOT EXISTS account_entitlement_evidence (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  source_kind TEXT NOT NULL,
  source_fingerprint TEXT NOT NULL,
  plan_family TEXT NOT NULL,
  seat_type TEXT NOT NULL,
  usage_multiplier_milli BIGINT,
  no_five_hour_limit INTEGER NOT NULL DEFAULT 0,
  entitlement_flags_known INTEGER NOT NULL DEFAULT 0,
  raw_plan_label TEXT NOT NULL DEFAULT '',
  confidence TEXT NOT NULL,
  observed_at BIGINT NOT NULL,
  expires_at BIGINT NOT NULL,
  payload_redacted_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_entitlement_account_observed ON account_entitlement_evidence(account_id,observed_at);
CREATE TABLE IF NOT EXISTS account_capacity_estimates (
  account_id TEXT NOT NULL,
  limiter_kind TEXT NOT NULL,
  model_family TEXT NOT NULL,
  service_tier TEXT NOT NULL,
  cycle_start BIGINT NOT NULL,
  cycle_end BIGINT NOT NULL,
  used_ratio_ppm BIGINT,
  remaining_units BIGINT,
  unit_kind TEXT NOT NULL,
  usd_equivalent_micro BIGINT,
  credits_remaining_milli BIGINT,
  method TEXT NOT NULL,
  sample_count BIGINT NOT NULL,
  confidence TEXT NOT NULL,
  lower_bound_units BIGINT,
  upper_bound_units BIGINT,
  updated_at BIGINT NOT NULL,
  PRIMARY KEY(account_id,limiter_kind,model_family,service_tier,cycle_start)
);
CREATE INDEX IF NOT EXISTS idx_capacity_account_updated ON account_capacity_estimates(account_id,updated_at);
`

func nullableInt64(value *int64) interface{} {
	if value == nil {
		return nil
	}
	return *value
}

func (s *Store) ensureBuiltinPricingCatalog(ctx context.Context) error {
	catalog := pricing.OfficialOpenAI20260829()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existingSHA string
	err = tx.QueryRowContext(ctx, `SELECT sha256 FROM pricing_catalog_versions WHERE id=?`, catalog.ID).Scan(&existingSHA)
	if err == nil && existingSHA != catalog.SHA256 {
		return fmt.Errorf("immutable pricing catalog %s checksum mismatch", catalog.ID)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO pricing_catalog_versions(id,catalog_kind,source_url,effective_at,expires_at,fetched_at,currency,sha256,status,raw_snapshot_ref)
VALUES(?,?,?,?,?,?,?,?,?,?)`, catalog.ID, catalog.CatalogKind, catalog.SourceURL, catalog.EffectiveAt, catalog.ExpiresAt, catalog.FetchedAt,
			catalog.Currency, catalog.SHA256, catalog.Status, catalog.SnapshotRef); err != nil {
			return err
		}
	}
	for _, rate := range catalog.Rates {
		if _, err = tx.ExecContext(ctx, `INSERT INTO pricing_rates(catalog_id,product_surface,provider,model_pattern,service_tier,context_band,unit_kind,
input_rate_units,cached_read_rate_units,cache_write_rate_units,output_rate_units,per_request_rate_units,multiplier_milli,source_line_ref)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, catalog.ID, string(rate.ProductSurface), rate.Provider, rate.Model,
			rate.ServiceTier, rate.ContextBand, string(rate.UnitKind), rate.InputRateUnits, rate.CachedRateUnits,
			nullableInt64(rate.CacheWriteRate), rate.OutputRateUnits, rate.PerRequestUnits, rate.MultiplierMilli, rate.SourceLineRef); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) usageProductSurface(ctx context.Context, exec sqlExecContext, accountID, providerHint, explicit string) pricing.ProductSurface {
	switch pricing.ProductSurface(strings.TrimSpace(explicit)) {
	case pricing.SurfaceOpenAIAPI, pricing.SurfaceChatGPTSubscription, pricing.SurfaceEnterpriseContract, pricing.SurfaceThirdParty:
		return pricing.ProductSurface(strings.TrimSpace(explicit))
	}
	provider := strings.ToLower(strings.TrimSpace(providerHint))
	if provider != "openai" && provider != "codex" {
		return pricing.SurfaceThirdParty
	}
	var authMethod, credentialMode, apiKey string
	err := exec.QueryRowContext(ctx, `SELECT COALESCE(auth_method,''),COALESCE(credential_mode,''),COALESCE(openai_api_key,'') FROM account_auth_tokens WHERE account_id=?`, strings.TrimSpace(accountID)).
		Scan(&authMethod, &credentialMode, &apiKey)
	if err != nil {
		return pricing.SurfaceThirdParty
	}
	authMethod = strings.ToLower(strings.TrimSpace(authMethod))
	credentialMode = strings.ToLower(strings.TrimSpace(credentialMode))
	if strings.Contains(credentialMode, "chatgpt") || strings.Contains(credentialMode, "auth_token") {
		return pricing.SurfaceChatGPTSubscription
	}
	if authMethod == "api_key" || strings.TrimSpace(apiKey) != "" {
		return pricing.SurfaceOpenAIAPI
	}
	if authMethod == "oauth" || authMethod == "token" || credentialMode != "" {
		return pricing.SurfaceChatGPTSubscription
	}
	return pricing.SurfaceThirdParty
}

func componentTokenUsage(normalized usage.Normalized) (pricing.TokenUsage, bool) {
	if normalized.InputTotal == nil || normalized.InputUncached == nil || normalized.CachedRead == nil || normalized.CacheWrite == nil || normalized.OutputTotal == nil {
		return pricing.TokenUsage{}, false
	}
	result := pricing.TokenUsage{InputTotal: *normalized.InputTotal, InputUncached: *normalized.InputUncached, OutputTotal: *normalized.OutputTotal}
	result.CachedRead = *normalized.CachedRead
	result.CacheWrite = *normalized.CacheWrite
	if normalized.OutputReasoning != nil {
		result.OutputReasoning = *normalized.OutputReasoning
	}
	return result, true
}

func (s *Store) writeNormalizedUsage(ctx context.Context, exec sqlExecContext, write UsageRecordWrite, diag UsageDiagnostics, now int64) error {
	eventID := strings.TrimSpace(diag.UsageEventID)
	if eventID == "" {
		return nil
	}
	presence := usage.PresenceFromRaw(write.Raw)
	if strings.TrimSpace(diag.UsageFieldPresenceJSON) != "" {
		_ = json.Unmarshal([]byte(diag.UsageFieldPresenceJSON), &presence)
	}
	integrityError := strings.TrimSpace(diag.UsageIntegrityError)
	if integrityError == "" {
		integrityError = usage.IntegrityFromRaw(write.Raw)
	}
	parsed := usage.Parsed{
		Model: write.Model, ServiceTier: diag.ObservedServiceTier, PromptTokens: write.Prompt,
		CompletionTokens: write.Completion, OutputReasoningTokens: diag.OutputReasoningTokens,
		TotalTokens: write.Total, CachedTokens: write.Cached, CacheReadTokens: write.CacheRead,
		CacheCreationTokens: write.CacheCreation, Presence: presence, IntegrityError: integrityError, RawUsage: write.Raw,
	}
	normalized := usage.Normalize(parsed, diag.UsageProvider, diag.UsageSource, diag.Estimated)
	tier := pricing.ResolveServiceTier(diag.RequestedServiceTier, diag.ForwardedServiceTier, diag.ObservedServiceTier)
	surface := s.usageProductSurface(ctx, exec, write.AccountID, normalized.Provider, diag.ProductSurface)
	settlement := normalized.SettlementState
	if tier.Settlement == "unsettled" && settlement != "integrity_error" {
		settlement = "unsettled"
	}
	settledAt := interface{}(nil)
	if settlement == "settled" && tier.Settlement == "final" {
		settledAt = now
	}
	_, err := exec.ExecContext(ctx, `INSERT INTO usage_components(usage_event_id,account_id,user_id,api_key_hash,upstream_attempt_id,product_surface,
requested_model,resolved_model,upstream_model,requested_service_tier,forwarded_service_tier,observed_service_tier,billed_service_tier,billed_tier_reason,
input_total,input_uncached,cached_read,cache_write,output_total,output_reasoning,field_presence_json,usage_source,settlement_state,estimated,integrity_error,settled_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(usage_event_id) DO UPDATE SET account_id=excluded.account_id,user_id=excluded.user_id,api_key_hash=excluded.api_key_hash,
 product_surface=excluded.product_surface,requested_model=excluded.requested_model,resolved_model=excluded.resolved_model,upstream_model=excluded.upstream_model,
 requested_service_tier=excluded.requested_service_tier,forwarded_service_tier=excluded.forwarded_service_tier,observed_service_tier=excluded.observed_service_tier,
 billed_service_tier=excluded.billed_service_tier,billed_tier_reason=excluded.billed_tier_reason,input_total=excluded.input_total,input_uncached=excluded.input_uncached,
 cached_read=excluded.cached_read,cache_write=excluded.cache_write,output_total=excluded.output_total,output_reasoning=excluded.output_reasoning,
 field_presence_json=excluded.field_presence_json,usage_source=excluded.usage_source,settlement_state=excluded.settlement_state,estimated=excluded.estimated,
 integrity_error=excluded.integrity_error,settled_at=excluded.settled_at,updated_at=excluded.updated_at
WHERE (usage_components.estimated>0 AND excluded.estimated=0) OR
 (usage_components.settlement_state IN ('unsettled','partial','provisional') AND excluded.settlement_state IN ('settled','integrity_error'))`,
		eventID, write.AccountID, write.UserID, write.APIKeyHash, "", string(surface), diag.RequestedModel, diag.ResolvedModel,
		firstNonEmptyStorage(diag.ActualModel, write.Model), tier.Requested, tier.Forwarded, tier.Observed, tier.Billed, tier.Reason,
		nullableInt64(normalized.InputTotal), nullableInt64(normalized.InputUncached), nullableInt64(normalized.CachedRead), nullableInt64(normalized.CacheWrite),
		nullableInt64(normalized.OutputTotal), nullableInt64(normalized.OutputReasoning), normalized.PresenceJSON, diag.UsageSource, settlement,
		boolInt(diag.Estimated), normalized.IntegrityError, settledAt, now, now)
	if err != nil {
		return err
	}

	occurredAt := now
	if err = exec.QueryRowContext(ctx, `SELECT created_at FROM usage_events WHERE event_id=?`, eventID).Scan(&occurredAt); err != nil {
		return err
	}
	catalog, err := s.ActivePricingCatalogAt(ctx, occurredAt)
	if err != nil {
		if errors.Is(err, ErrNoActivePricingCatalog) {
			// Telemetry is the source of truth; a temporarily missing/expired
			// catalog must not roll back the usage row.  The admin reconciliation
			// endpoint can price this component once an audited catalog is active.
			return nil
		}
		return err
	}
	tokens, complete := componentTokenUsage(normalized)
	apiValuation := pricing.UnavailableValuation("api_usd_equivalent", catalog.ID, pricing.MicroUSDScale, "usage_components_incomplete")
	if complete && normalized.Provider == "openai" {
		apiValuation = catalog.Value(pricing.SurfaceOpenAIAPI, firstNonEmptyStorage(diag.ActualModel, write.Model, diag.ResolvedModel), tier.Billed, tokens)
	}
	creditValuation := pricing.UnavailableValuation("chatgpt_credits", catalog.ID, pricing.MilliCreditScale, "not_chatgpt_subscription_surface")
	if complete && surface == pricing.SurfaceChatGPTSubscription && normalized.Provider == "openai" {
		creditValuation = catalog.Value(pricing.SurfaceChatGPTSubscription, firstNonEmptyStorage(diag.ActualModel, write.Model, diag.ResolvedModel), tier.Billed, tokens)
	}
	for _, valuation := range []pricing.Valuation{apiValuation, creditValuation} {
		if valuation.Confidence == "settled" && (settlement != "settled" || tier.Settlement != "final") {
			valuation.Confidence = "provisional"
		}
		breakdown, _ := json.Marshal(map[string]interface{}{"valuation": valuation, "service_tier": tier, "product_surface": surface})
		if _, err = exec.ExecContext(ctx, `INSERT INTO usage_valuations(usage_event_id,valuation_kind,catalog_id,amount_units,unit_scale,confidence,breakdown_json,computed_at)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(usage_event_id,valuation_kind,catalog_id) DO UPDATE SET amount_units=excluded.amount_units,
 unit_scale=excluded.unit_scale,confidence=excluded.confidence,breakdown_json=excluded.breakdown_json,computed_at=excluded.computed_at
WHERE usage_valuations.confidence IN ('unavailable','partial','provisional') AND excluded.confidence='settled'`,
			eventID, valuation.Kind, valuation.CatalogID, nullableInt64(valuation.AmountUnits), valuation.UnitScale, valuation.Confidence, string(breakdown), now); err != nil {
			return err
		}
	}
	return nil
}

type PricingCatalogVersion struct {
	ID             string `json:"id"`
	CatalogKind    string `json:"catalog_kind"`
	SourceURL      string `json:"source_url"`
	Currency       string `json:"currency"`
	SHA256         string `json:"sha256"`
	Status         string `json:"status"`
	RawSnapshotRef string `json:"raw_snapshot_ref"`
	EffectiveAt    int64  `json:"effective_at"`
	ExpiresAt      int64  `json:"expires_at,omitempty"`
	FetchedAt      int64  `json:"fetched_at"`
}

func (s *Store) ListPricingCatalogVersions(ctx context.Context) ([]PricingCatalogVersion, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id,catalog_kind,source_url,effective_at,expires_at,fetched_at,currency,sha256,status,raw_snapshot_ref
FROM pricing_catalog_versions ORDER BY effective_at DESC,id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PricingCatalogVersion{}
	for rows.Next() {
		var item PricingCatalogVersion
		if err := rows.Scan(&item.ID, &item.CatalogKind, &item.SourceURL, &item.EffectiveAt, &item.ExpiresAt, &item.FetchedAt, &item.Currency, &item.SHA256, &item.Status, &item.RawSnapshotRef); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

type UsageValuationRow struct {
	UsageEventID       string          `json:"usage_event_id"`
	ValuationKind      string          `json:"valuation_kind"`
	CatalogID          string          `json:"catalog_id"`
	CatalogEffectiveAt int64           `json:"catalog_effective_at"`
	CatalogSourceURL   string          `json:"catalog_source_url"`
	Confidence         string          `json:"confidence"`
	BreakdownJSON      json.RawMessage `json:"breakdown"`
	AmountUnits        *int64          `json:"amount_units"`
	UnitScale          int64           `json:"unit_scale"`
	ComputedAt         int64           `json:"computed_at"`
}

func (s *Store) ListUsageValuations(ctx context.Context, eventID string) ([]UsageValuationRow, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT v.usage_event_id,v.valuation_kind,v.catalog_id,p.effective_at,p.source_url,
v.amount_units,v.unit_scale,v.confidence,v.breakdown_json,v.computed_at
FROM usage_valuations v JOIN pricing_catalog_versions p ON p.id=v.catalog_id
WHERE v.usage_event_id=? ORDER BY v.valuation_kind,p.effective_at DESC,v.catalog_id DESC`, strings.TrimSpace(eventID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []UsageValuationRow{}
	for rows.Next() {
		var item UsageValuationRow
		var amount sql.NullInt64
		var breakdown string
		if err := rows.Scan(&item.UsageEventID, &item.ValuationKind, &item.CatalogID, &item.CatalogEffectiveAt, &item.CatalogSourceURL,
			&amount, &item.UnitScale, &item.Confidence, &breakdown, &item.ComputedAt); err != nil {
			return nil, err
		}
		item.BreakdownJSON = json.RawMessage(breakdown)
		if amount.Valid {
			item.AmountUnits = &amount.Int64
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
