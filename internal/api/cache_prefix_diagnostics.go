package api

import (
	"sort"
	"strconv"
	"strings"
)

// The cache-prefix reports intentionally expose only metadata.  Prompt bytes,
// section text and reversible conversation identifiers never leave the process.
// When the request body was not retained by the bounded scanner, the reports
// say so explicitly instead of pretending that a byte-level diff was observed.

func cacheStablePrefixDiffHeader() []string {
	return []string{
		"account_code", "provider", "model", "occurred_at", "prompt_cache_key_hash",
		"stable_prefix_source", "stable_prefix_reason", "stable_prefix_bytes",
		"first_changed_offset", "longest_common_section_prefix", "evidence_state", "completeness",
	}
}

func cacheStablePrefixDiffRows(rows []diagnosticUsageRecord, codebook diagnosticCodebook) [][]string {
	ordered := append([]diagnosticUsageRecord(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].AccountID != ordered[j].AccountID {
			return ordered[i].AccountID < ordered[j].AccountID
		}
		if ordered[i].Model != ordered[j].Model {
			return ordered[i].Model < ordered[j].Model
		}
		if ordered[i].CreatedAt != ordered[j].CreatedAt {
			return ordered[i].CreatedAt < ordered[j].CreatedAt
		}
		return ordered[i].ID < ordered[j].ID
	})
	out := make([][]string, 0, len(ordered))
	for _, row := range ordered {
		state, completeness := "metadata_only", "partial_no_prompt_bytes"
		// StablePrefixBytes is measured by the bounded request scanner.  It is
		// safe to export, but it is not enough to claim a content diff; leave the
		// byte offset and section-prefix fields empty until a retained, redacted
		// section digest is available.
		if row.StablePrefixSource == "" && row.StablePrefixBytes == 0 {
			state = "unavailable"
			completeness = "missing_stable_prefix_metadata"
		}
		out = append(out, []string{
			codebook.code(row.AccountID), row.UsageProvider, row.Model, itoa64(row.CreatedAt),
			truncateCacheKey(row.PromptCacheKeyHash), row.StablePrefixSource, row.StablePrefixReason,
			itoa64(row.StablePrefixBytes), "", "", state, completeness,
		})
	}
	return out
}

func cacheKeyChurnHeader() []string {
	return []string{
		"account_code", "provider", "model", "cache_key_hash", "requests", "hit_requests",
		"creation_requests", "first_seen_at", "last_seen_at", "key_changes", "hit_rate", "evidence_state",
	}
}

type cacheKeyChurnStat struct {
	account, provider, model, key string
	requests, hits, creations     int64
	first, last                   int64
	keyChanges                    int64
}

// cacheKeyChurnRows aggregates observed key identities without exporting the
// original prompt_cache_key.  A key change is counted on the destination key;
// this makes a namespace move visible while keeping every row independently
// useful to operators.
func cacheKeyChurnRows(rows []diagnosticUsageRecord, codebook diagnosticCodebook) [][]string {
	ordered := append([]diagnosticUsageRecord(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].AccountID != ordered[j].AccountID {
			return ordered[i].AccountID < ordered[j].AccountID
		}
		if ordered[i].UsageProvider != ordered[j].UsageProvider {
			return ordered[i].UsageProvider < ordered[j].UsageProvider
		}
		if ordered[i].Model != ordered[j].Model {
			return ordered[i].Model < ordered[j].Model
		}
		if ordered[i].CreatedAt != ordered[j].CreatedAt {
			return ordered[i].CreatedAt < ordered[j].CreatedAt
		}
		return ordered[i].ID < ordered[j].ID
	})
	stats := map[string]*cacheKeyChurnStat{}
	var previous string
	for _, row := range ordered {
		key := strings.TrimSpace(row.PromptCacheKeyHash)
		if key == "" {
			key = "unkeyed"
		}
		identity := row.AccountID + "\x00" + row.UsageProvider + "\x00" + row.Model + "\x00" + key
		stat := stats[identity]
		if stat == nil {
			stat = &cacheKeyChurnStat{account: row.AccountID, provider: row.UsageProvider, model: row.Model, key: key, first: row.CreatedAt}
			stats[identity] = stat
		}
		stat.requests++
		if row.CacheReadPresent > 0 {
			stat.hits++
		}
		if row.CacheCreationPresent > 0 {
			stat.creations++
		}
		if stat.first == 0 || (row.CreatedAt > 0 && row.CreatedAt < stat.first) {
			stat.first = row.CreatedAt
		}
		if row.CreatedAt > stat.last {
			stat.last = row.CreatedAt
		}
		stream := row.AccountID + "\x00" + row.UsageProvider + "\x00" + row.Model
		if previous != "" {
			previousStream, previousKey := splitCacheChurnIdentity(previous)
			if previousStream == stream && previousKey != key {
				stat.keyChanges++
			}
		}
		previous = stream + "\x00" + key
	}
	out := make([][]string, 0, len(stats))
	for _, stat := range stats {
		rate := ""
		if stat.requests > 0 {
			rate = strconv.FormatFloat(float64(stat.hits)/float64(stat.requests), 'f', 4, 64)
		}
		out = append(out, []string{
			codebook.code(stat.account), stat.provider, stat.model, truncateCacheKey(stat.key),
			itoa64(stat.requests), itoa64(stat.hits), itoa64(stat.creations), itoa64(stat.first),
			itoa64(stat.last), itoa64(stat.keyChanges), rate, "metadata_only",
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		if out[i][2] != out[j][2] {
			return out[i][2] < out[j][2]
		}
		return out[i][3] < out[j][3]
	})
	return out
}

func splitCacheChurnIdentity(value string) (stream, key string) {
	parts := strings.Split(value, "\x00")
	if len(parts) < 4 {
		return "", ""
	}
	return strings.Join(parts[:3], "\x00"), parts[3]
}

func cacheNamespaceMovesHeader() []string {
	return []string{
		"account_code", "provider", "model", "occurred_at", "from_cache_key_hash", "to_cache_key_hash",
		"reason", "evidence_state",
	}
}

func cacheNamespaceMoveRows(rows []diagnosticUsageRecord, codebook diagnosticCodebook) [][]string {
	ordered := append([]diagnosticUsageRecord(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].AccountID != ordered[j].AccountID {
			return ordered[i].AccountID < ordered[j].AccountID
		}
		if ordered[i].UsageProvider != ordered[j].UsageProvider {
			return ordered[i].UsageProvider < ordered[j].UsageProvider
		}
		if ordered[i].Model != ordered[j].Model {
			return ordered[i].Model < ordered[j].Model
		}
		if ordered[i].CreatedAt != ordered[j].CreatedAt {
			return ordered[i].CreatedAt < ordered[j].CreatedAt
		}
		return ordered[i].ID < ordered[j].ID
	})
	last := map[string]string{}
	out := make([][]string, 0)
	for _, row := range ordered {
		stream := row.AccountID + "\x00" + row.UsageProvider + "\x00" + row.Model
		to := strings.TrimSpace(row.PromptCacheKeyHash)
		if to == "" {
			to = "unkeyed"
		}
		from := last[stream]
		if from != "" && from != to {
			out = append(out, []string{
				codebook.code(row.AccountID), row.UsageProvider, row.Model, itoa64(row.CreatedAt),
				truncateCacheKey(from), truncateCacheKey(to), "prompt_cache_key_changed", "metadata_only",
			})
		}
		last[stream] = to
	}
	return out
}

func topCacheMissExplanationHeader() []string {
	return []string{
		"provider", "model", "reason", "requests", "miss_requests", "hit_requests",
		"creation_requests", "estimated_requests", "miss_rate", "evidence_state",
	}
}

type cacheMissExplanationStat struct {
	provider, model, reason           string
	requests, misses, hits, creations int64
	estimated                         int64
}

func topCacheMissExplanationRows(rows []diagnosticUsageRecord) [][]string {
	stats := map[string]*cacheMissExplanationStat{}
	for _, row := range rows {
		// Rows without either cache field cannot distinguish a cache miss from an
		// unsupported/unreported provider and therefore do not belong in a miss
		// ranking.  In particular, Kiro's unreported state is not a cache miss.
		reported := row.CacheReadPresent != 0 || row.CacheCreationPresent != 0
		capability := strings.ToLower(strings.TrimSpace(row.CacheCapability))
		if !reported && (capability == "" || capability == "unknown" || capability == "unreported" || capability == "explicitly_unsupported") {
			continue
		}
		reason := strings.TrimSpace(row.DiagnosticsMissReason)
		if reason == "" {
			reason = strings.TrimSpace(row.StablePrefixReason)
		}
		if reason == "" {
			reason = strings.TrimSpace(row.CacheCapability)
		}
		if reason == "" {
			reason = "unclassified"
		}
		key := row.UsageProvider + "\x00" + row.Model + "\x00" + reason
		stat := stats[key]
		if stat == nil {
			stat = &cacheMissExplanationStat{provider: row.UsageProvider, model: row.Model, reason: reason}
			stats[key] = stat
		}
		stat.requests++
		if row.CacheReadPresent > 0 {
			stat.hits++
		} else {
			stat.misses++
		}
		if row.CacheCreationPresent > 0 {
			stat.creations++
		}
		if row.Estimated != 0 {
			stat.estimated++
		}
	}
	out := make([][]string, 0, len(stats))
	for _, stat := range stats {
		missRate := ""
		if stat.requests > 0 {
			missRate = strconv.FormatFloat(float64(stat.misses)/float64(stat.requests), 'f', 4, 64)
		}
		out = append(out, []string{stat.provider, stat.model, stat.reason, itoa64(stat.requests), itoa64(stat.misses), itoa64(stat.hits), itoa64(stat.creations), itoa64(stat.estimated), missRate, "metadata_only"})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][4] != out[j][4] {
			return out[i][4] > out[j][4]
		}
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		if out[i][1] != out[j][1] {
			return out[i][1] < out[j][1]
		}
		return out[i][2] < out[j][2]
	})
	return out
}
