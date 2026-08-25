package api

import (
	"context"
	"regexp"
	"sort"
	"strconv"

	"codex-account-pool/internal/storage"
)

// Recommendation F — cache observability, ported to the cache-hits export:
//
//  1. account_health.csv — the sub2api channel_monitor_v2 cacheRateBand pattern:
//     a per-account health band derived from the request cache-hit rate, with the
//     warning/critical floors as thresholds. A zero/zero configuration disables
//     the penalty (all bands report "disabled").
//  2. interval_distribution.csv — per (provider, model, cache-key) request
//     inter-arrival buckets, so a "cold" conversation (interval beyond the
//     upstream TTL) can be told apart from a "drifted" one (short interval, but
//     the prefix still missed).
//  3. affinity_rebind_events.csv — every scheduler affinity_rebind audit joined
//     to the usage that followed it on the target account, so a cross-account
//     move shows its actual cache-rebuild cost.

const (
	defaultCacheHealthWarning  = 0.7
	defaultCacheHealthCritical = 0.4
	cacheHealthMinSamples      = 5
	cacheRebindFollowWindow    = 3600 // seconds of usage to attribute after a rebind
)

// cacheRateScore maps a cache hit rate to 0-100, linear (sub2api
// channel_monitor_v2.go cacheRateScore). 0% → 0, 100% → 100.
func cacheRateScore(cacheRate float64) float64 {
	if cacheRate <= 0 {
		return 0
	}
	if cacheRate >= 1 {
		return 100
	}
	return 100 * cacheRate
}

// cacheRateBand maps a cache hit rate to a coarse health label (sub2api
// channel_monitor_v2.go cacheRateBand): below critical → critical, below
// warning → warning, else healthy. Zero thresholds disable the evaluation.
func cacheRateBand(cacheRate, warning, critical float64) string {
	if warning <= 0 && critical <= 0 {
		return "disabled"
	}
	if cacheRate < critical {
		return "critical"
	}
	if cacheRate < warning {
		return "warning"
	}
	return "healthy"
}

func cacheAccountHealthHeader() []string {
	return []string{"account_code", "provider", "requests", "hit_requests", "request_hit_rate", "cache_rate_score", "cache_rate_band", "reason"}
}

// cacheAccountHealthRows aggregates the by-account-model rows up to account
// level and evaluates the cacheRateBand health score for each account.
func cacheAccountHealthRows(rows []storage.CacheUsageMetricRow, codebook diagnosticCodebook, warning, critical float64) [][]string {
	type acc struct {
		requests, hits int64
		providers      map[string]int
	}
	agg := make(map[string]*acc)
	for _, row := range rows {
		code := codebook.code(row.AccountID)
		a := agg[code]
		if a == nil {
			a = &acc{providers: make(map[string]int)}
			agg[code] = a
		}
		a.requests += row.Requests
		a.hits += row.HitRequests
		if row.Provider != "" {
			a.providers[row.Provider]++
		}
	}
	out := make([][]string, 0, len(agg))
	for code, a := range agg {
		rate := float64(0)
		if a.requests > 0 {
			rate = float64(a.hits) / float64(a.requests)
		}
		reason := ""
		band := cacheRateBand(rate, warning, critical)
		score := fmtScore(cacheRateScore(rate))
		if a.requests < cacheHealthMinSamples {
			// sub2api semantics: a meaningless denominator must not produce a
			// band verdict. Leave score and band empty instead of penalizing.
			band, score = "", ""
			reason = "insufficient_samples"
		} else if band == "disabled" {
			reason = "thresholds_disabled"
		}
		provider := ""
		for p := range a.providers {
			provider = p
			break
		}
		out = append(out, []string{code, provider, itoa64(a.requests), itoa64(a.hits), fmtRate(rate), score, band, reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

type cacheIntervalBucket string

const (
	intervalUnder1m  cacheIntervalBucket = "<1m"
	interval1To5m    cacheIntervalBucket = "1-5m"
	interval5To10m   cacheIntervalBucket = "5-10m"
	interval10To30m  cacheIntervalBucket = "10-30m"
	intervalOver30m  cacheIntervalBucket = ">30m"
)

func cacheIntervalBucketFor(seconds int64) cacheIntervalBucket {
	switch {
	case seconds < 60:
		return intervalUnder1m
	case seconds < 300:
		return interval1To5m
	case seconds < 600:
		return interval5To10m
	case seconds < 1800:
		return interval10To30m
	default:
		return intervalOver30m
	}
}

func cacheIntervalHeader() []string {
	return []string{"provider", "model", "cache_key_prefix", "interval_bucket", "requests", "hit_requests", "hit_rate", "creation_requests", "creation_rate", "diagnosis"}
}

type intervalSession struct {
	key      string
	provider string
	model    string
	requests []diagnosticUsageRecord
}

// cacheIntervalRows buckets per-conversation request inter-arrival times and
// labels each bucket's miss pattern: a short-interval miss is a prefix drift
// (the key changed between turns), a long-interval miss is TTL cooldown (the
// upstream cache expired during idle time).
func cacheIntervalRows(usageRows []diagnosticUsageRecord) [][]string {
	bySession := make(map[string]*intervalSession)
	for _, row := range usageRows {
		if row.CacheReadPresent == 0 && row.CacheCreationPresent == 0 {
			continue
		}
		key := row.PromptCacheKeyHash
		if key == "" {
			key = "unkeyed"
		}
		sess := bySession[row.UsageProvider+"\x00"+row.Model+"\x00"+key]
		if sess == nil {
			sess = &intervalSession{key: key, provider: row.UsageProvider, model: row.Model}
			bySession[row.UsageProvider+"\x00"+row.Model+"\x00"+key] = sess
		}
		sess.requests = append(sess.requests, row)
	}

	type bucketStat struct {
		requests, hits, creations int64
	}
	type bucketKey struct {
		provider, model, key string
		bucket               cacheIntervalBucket
	}
	stats := make(map[bucketKey]*bucketStat)
	for _, sess := range bySession {
		reqs := sess.requests
		sort.Slice(reqs, func(i, j int) bool { return reqs[i].CreatedAt < reqs[j].CreatedAt })
		for i, row := range reqs {
			if i == 0 {
				continue
			}
			b := cacheIntervalBucketFor(reqs[i].CreatedAt - reqs[i-1].CreatedAt)
			key := bucketKey{sess.provider, sess.model, sess.key, b}
			s := stats[key]
			if s == nil {
				s = &bucketStat{}
				stats[key] = s
			}
			s.requests++
			if row.CacheReadPresent > 0 {
				s.hits++
			}
			if row.CacheCreationPresent > 0 {
				s.creations++
			}
		}
	}

	out := make([][]string, 0, len(stats))
	for key, s := range stats {
		rate := float64(0)
		if s.requests > 0 {
			rate = float64(s.hits) / float64(s.requests)
		}
		creationRate := float64(0)
		if s.requests > 0 {
			creationRate = float64(s.creations) / float64(s.requests)
		}
		diagnosis := ""
		switch {
		case s.requests > 0 && rate < 0.5 && (key.bucket == intervalUnder1m || key.bucket == interval1To5m):
			diagnosis = "drift_miss"
		case s.requests > 0 && rate < 0.5 && key.bucket == intervalOver30m:
			diagnosis = "cooldown_miss"
		}
		out = append(out, []string{key.provider, key.model, truncateCacheKey(key.key), string(key.bucket),
			itoa64(s.requests), itoa64(s.hits), fmtRate(rate), itoa64(s.creations), fmtRate(creationRate), diagnosis})
	}
	sort.Slice(out, func(i, j int) bool {
		for k := 0; k < 4; k++ {
			if out[i][k] != out[j][k] {
				return out[i][k] < out[j][k]
			}
		}
		return false
	})
	return out
}

var affinityRebindDetailPattern = regexp.MustCompile(`from=(\S+) to=(\S+) affinity=(\S+) route_key=\S+ group=\S+ model=(\S+)`)

func cacheRebindHeader() []string {
	return []string{"occurred_at", "from_account_code", "to_account_code", "affinity_hash", "model", "post_rebind_requests", "post_rebind_hit_requests", "post_rebind_hit_rate"}
}

// cacheRebindRows joins affinity_rebind audit events to the usage rows that
// followed them on the target account (up to cacheRebindFollowWindow seconds),
// exposing the cache-rebuild cost of each cross-account move.
func cacheRebindRows(ctx context.Context, store *storage.Store, codebook diagnosticCodebook, usageRows []diagnosticUsageRecord, since int64) [][]string {
	audits, err := store.ListAuditLogByActionSince(ctx, "affinity_rebind", since, 5000)
	if err != nil {
		return [][]string{{"error", err.Error()}}
	}
	// Index upstream usage per account+model, ordered by time, so each rebind
	// event can count the requests in its follow window with a binary search
	// instead of rescaming the whole usage slice.
	type timedUsage struct {
		at   int64
		read int64
	}
	postByAccountModel := make(map[string][]timedUsage)
	for _, row := range usageRows {
		if row.UsageSource != "upstream" || row.AccountID == "" {
			continue
		}
		key := row.AccountID + "\x00" + row.Model
		postByAccountModel[key] = append(postByAccountModel[key], timedUsage{at: row.CreatedAt, read: row.CacheReadPresent})
	}
	for _, slice := range postByAccountModel {
		sort.Slice(slice, func(i, j int) bool { return slice[i].at < slice[j].at })
	}
	out := make([][]string, 0, len(audits))
	for _, audit := range audits {
		m := affinityRebindDetailPattern.FindStringSubmatch(audit.Detail)
		if m == nil {
			out = append(out, []string{itoa64(audit.CreatedAt), codebook.code(audit.AccountID), "", "", "", "0", "0", ""})
			continue
		}
		from, to, affinityHash, model := m[1], m[2], m[3], m[4]
		slice := postByAccountModel[to+"\x00"+model]
		windowEnd := audit.CreatedAt + cacheRebindFollowWindow
		startIdx := sort.Search(len(slice), func(i int) bool { return slice[i].at >= audit.CreatedAt })
		var requests, hits int64
		for _, t := range slice[startIdx:] {
			if t.at > windowEnd {
				break
			}
			requests++
			if t.read > 0 {
				hits++
			}
		}
		rate := float64(0)
		if requests > 0 {
			rate = float64(hits) / float64(requests)
		}
		out = append(out, []string{itoa64(audit.CreatedAt), codebook.code(from), codebook.code(to),
			truncateCacheKey(affinityHash), model, itoa64(requests), itoa64(hits), fmtRate(rate)})
	}
	return out
}

func fmtRate(rate float64) string {
	if rate < 0 {
		return ""
	}
	return strconv.FormatFloat(rate, 'f', 4, 64)
}

func fmtScore(score float64) string {
	return strconv.FormatFloat(score, 'f', 1, 64)
}
