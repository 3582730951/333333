package storage

import (
	"context"
	"strings"
)

// ProviderModelUsageRow is the stable provider + model dimension used by the
// dashboard and usage pages. Model-only fields remain populated for older clients.
type ProviderModelUsageRow struct {
	UserUsageRow
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	ProviderType string `json:"provider_type"`
	DimensionKey string `json:"dimension_key"`
	DisplayLabel string `json:"display_label"`
}

type ProviderModelSeriesDescriptor struct {
	SeriesDimension string `json:"series_dimension"`
	SeriesKey       string `json:"series_key"`
	SeriesLabel     string `json:"series_label"`
	ProviderID      string `json:"provider_id"`
	ProviderName    string `json:"provider_name"`
	ProviderType    string `json:"provider_type"`
	Model           string `json:"model"`
}

type ProviderModelSeriesRow struct {
	Bucket              int64  `json:"bucket"`
	SeriesDimension     string `json:"series_dimension"`
	SeriesKey           string `json:"series_key"`
	SeriesLabel         string `json:"series_label"`
	ProviderID          string `json:"provider_id"`
	ProviderName        string `json:"provider_name"`
	ProviderType        string `json:"provider_type"`
	Model               string `json:"model"`
	Requests            int64  `json:"requests"`
	PromptTokens        int64  `json:"prompt_tokens"`
	CompletionTokens    int64  `json:"completion_tokens"`
	CachedTokens        int64  `json:"cached_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	TotalTokens         int64  `json:"total_tokens"`
}

const normalizedUsageProviderSQL = `(CASE WHEN TRIM(COALESCE(usage_provider,''))='' THEN '__unknown__' ELSE lower(TRIM(COALESCE(usage_provider,''))) END)`
const providerModelUsageKeySQL = `(` + normalizedUsageProviderSQL + ` || '::' || CASE WHEN ` + normalizedUsageModelSQL + `='' THEN '__unknown__' ELSE ` + normalizedUsageModelSQL + ` END)`

func usageProviderDisplay(providerID, configuredName string) (name, providerType string) {
	providerID = strings.ToLower(strings.TrimSpace(providerID))
	switch providerID {
	case "codex":
		return "Codex", "codex"
	case "openai", "chatgpt":
		return "ChatGPT", "chatgpt"
	case "claude", "anthropic":
		return "Claude", "claude"
	case "kiro":
		return "Kiro", "kiro"
	case "antigravity":
		return "Antigravity", "antigravity"
	case "", "__unknown__":
		return "Unknown", "unknown"
	default:
		if configuredName = strings.TrimSpace(configuredName); configuredName != "" {
			return configuredName, "model_provider"
		}
		return providerID, "model_provider"
	}
}

func finalizeProviderModelUsage(providerID, configuredName, model string) (id, name, providerType, modelValue, modelKey, modelLabel, key, label string) {
	id = strings.ToLower(strings.TrimSpace(providerID))
	if id == "" {
		id = "__unknown__"
	}
	name, providerType = usageProviderDisplay(id, configuredName)
	modelValue = applyUsageModelFields(model, &modelKey, &modelLabel)
	key = id + "::" + modelKey
	label = name + " · " + modelLabel
	return
}

func (s *Store) UsageByProviderModelWindow(ctx context.Context, since, until int64) ([]ProviderModelUsageRow, error) {
	cutover := s.UsageAccuracyCutover(ctx)
	if since < cutover {
		since = cutover
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT `+normalizedUsageProviderSQL+`, COALESCE(MAX(cp.name),''), `+normalizedUsageModelSQL+`,
       COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0),
       COALESCE(SUM(cached_tokens),0), COALESCE(SUM(`+cacheTotalInputTokensSQL+`),0),
       COALESCE(SUM(CASE WHEN cache_read_tokens > 0 THEN cache_read_tokens ELSE cached_tokens END),0),
       COALESCE(SUM(cache_creation_tokens),0)
FROM usage_records ur
LEFT JOIN custom_providers cp ON lower(cp.id)=`+normalizedUsageProviderSQL+`
WHERE ur.created_at >= ? AND ur.created_at < ? AND ur.estimated=0
GROUP BY `+normalizedUsageProviderSQL+`, `+normalizedUsageModelSQL+`
ORDER BY SUM(ur.total_tokens) DESC, COUNT(*) DESC, `+normalizedUsageProviderSQL+`, `+normalizedUsageModelSQL, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderModelUsageRow{}
	for rows.Next() {
		var row ProviderModelUsageRow
		var configuredName, providerID, model string
		if err := rows.Scan(&providerID, &configuredName, &model, &row.Requests, &row.PromptTokens, &row.CompletionTokens, &row.TotalTokens,
			&row.CachedTokens, &row.CacheInputTokens, &row.CacheReadTokens, &row.CacheCreationTokens); err != nil {
			return nil, err
		}
		row.ProviderID, row.ProviderName, row.ProviderType, row.Model, row.ModelKey, row.ModelLabel, row.DimensionKey, row.DisplayLabel =
			finalizeProviderModelUsage(providerID, configuredName, model)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) UsageProviderModelSeriesWindow(ctx context.Context, since, until, bucketSeconds int64, seriesLimit int) ([]ProviderModelSeriesDescriptor, []ProviderModelSeriesRow, error) {
	if bucketSeconds <= 0 {
		bucketSeconds = 3600
	}
	if seriesLimit <= 0 {
		seriesLimit = 6
	}
	cutover := s.UsageAccuracyCutover(ctx)
	if since < cutover {
		since = cutover
	}
	topRows, err := s.rdb.QueryContext(ctx, `
SELECT `+normalizedUsageProviderSQL+`, COALESCE(MAX(cp.name),''), `+normalizedUsageModelSQL+`,
       COALESCE(SUM(ur.total_tokens),0), COUNT(*)
FROM usage_records ur
LEFT JOIN custom_providers cp ON lower(cp.id)=`+normalizedUsageProviderSQL+`
WHERE ur.created_at >= ? AND ur.created_at < ? AND ur.estimated=0
GROUP BY `+normalizedUsageProviderSQL+`, `+normalizedUsageModelSQL+`
ORDER BY SUM(ur.total_tokens) DESC, COUNT(*) DESC, `+normalizedUsageProviderSQL+`, `+normalizedUsageModelSQL+`
LIMIT ?`, since, until, seriesLimit)
	if err != nil {
		return nil, nil, err
	}
	descriptors := []ProviderModelSeriesDescriptor{}
	keys := []string{}
	for topRows.Next() {
		var providerID, configuredName, model string
		var total, count int64
		if err := topRows.Scan(&providerID, &configuredName, &model, &total, &count); err != nil {
			topRows.Close()
			return nil, nil, err
		}
		_ = total
		_ = count
		id, name, providerType, modelValue, _, modelLabel, key, label := finalizeProviderModelUsage(providerID, configuredName, model)
		descriptors = append(descriptors, ProviderModelSeriesDescriptor{
			SeriesDimension: "provider_model", SeriesKey: key, SeriesLabel: label,
			ProviderID: id, ProviderName: name, ProviderType: providerType, Model: modelValue,
		})
		_ = modelLabel
		keys = append(keys, key)
	}
	if err := topRows.Err(); err != nil {
		topRows.Close()
		return nil, nil, err
	}
	topRows.Close()
	if len(keys) == 0 {
		return descriptors, []ProviderModelSeriesRow{}, nil
	}
	args := []interface{}{bucketSeconds, bucketSeconds, since, until}
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT (ur.created_at / ?) * ? AS bucket,
       `+normalizedUsageProviderSQL+`, COALESCE(MAX(cp.name),''), `+normalizedUsageModelSQL+`,
       COUNT(*), COALESCE(SUM(ur.prompt_tokens),0), COALESCE(SUM(ur.completion_tokens),0),
       COALESCE(SUM(ur.cached_tokens),0),
       COALESCE(SUM(CASE WHEN ur.cache_read_tokens > 0 THEN ur.cache_read_tokens ELSE ur.cached_tokens END),0),
       COALESCE(SUM(ur.cache_creation_tokens),0), COALESCE(SUM(ur.total_tokens),0)
FROM usage_records ur
LEFT JOIN custom_providers cp ON lower(cp.id)=`+normalizedUsageProviderSQL+`
WHERE ur.created_at >= ? AND ur.created_at < ? AND ur.estimated=0
  AND `+providerModelUsageKeySQL+` IN (`+sqlPlaceholders(len(keys))+`)
GROUP BY bucket, `+normalizedUsageProviderSQL+`, `+normalizedUsageModelSQL+`
ORDER BY bucket, `+normalizedUsageProviderSQL+`, `+normalizedUsageModelSQL, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []ProviderModelSeriesRow{}
	for rows.Next() {
		var row ProviderModelSeriesRow
		var providerID, configuredName, model string
		if err := rows.Scan(&row.Bucket, &providerID, &configuredName, &model, &row.Requests, &row.PromptTokens,
			&row.CompletionTokens, &row.CachedTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.TotalTokens); err != nil {
			return nil, nil, err
		}
		row.ProviderID, row.ProviderName, row.ProviderType, row.Model, _, _, row.SeriesKey, row.SeriesLabel =
			finalizeProviderModelUsage(providerID, configuredName, model)
		row.SeriesDimension = "provider_model"
		out = append(out, row)
	}
	return descriptors, out, rows.Err()
}
