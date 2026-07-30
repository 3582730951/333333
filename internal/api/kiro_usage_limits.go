package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type kiroUsageSnapshot struct {
	Current     float64
	Limit       float64
	Remaining   float64
	UsedPercent float64
	ResetAt     int64
	Status      string
}

// parseKiroUsageLimits selects the agentic quota row by resource type instead
// of relying on response order. Kiro can return additional resource rows, trial
// credits, and bonus grants; all active credits participate in readiness.
func parseKiroUsageLimits(limits map[string]interface{}) (kiroUsageSnapshot, error) {
	var snapshot kiroUsageSnapshot
	list, _ := lookup(limits, "usageBreakdownList").([]interface{})
	if len(list) == 0 {
		return snapshot, errors.New("kiro usage breakdown missing")
	}
	var base map[string]interface{}
	for _, item := range list {
		row, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		resource := strings.TrimSpace(fmt.Sprint(lookup(row, "resourceType")))
		if strings.EqualFold(resource, "AGENTIC_REQUEST") {
			base = row
			break
		}
	}
	// Older Kiro responses omitted resourceType when only one quota existed.
	if base == nil && len(list) == 1 {
		base, _ = list[0].(map[string]interface{})
	}
	if base == nil {
		return snapshot, errors.New("kiro AGENTIC_REQUEST usage breakdown missing")
	}

	current, _ := kiroJSONNumber(base, "currentUsageWithPrecision", "currentUsage")
	limit, limitPresent := kiroJSONNumber(base, "usageLimitWithPrecision", "usageLimit")
	if trial, ok := lookup(base, "freeTrialInfo").(map[string]interface{}); ok &&
		strings.EqualFold(strings.TrimSpace(fmt.Sprint(lookup(trial, "freeTrialStatus"))), "ACTIVE") {
		trialCurrent, _ := kiroJSONNumber(trial, "currentUsageWithPrecision", "currentUsage")
		trialLimit, trialLimitPresent := kiroJSONNumber(trial, "usageLimitWithPrecision", "usageLimit")
		current += trialCurrent
		limit += trialLimit
		limitPresent = limitPresent || trialLimitPresent
	}
	if bonuses, ok := lookup(base, "bonuses").([]interface{}); ok {
		for _, item := range bonuses {
			bonus, ok := item.(map[string]interface{})
			if !ok || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(lookup(bonus, "status"))), "ACTIVE") {
				continue
			}
			bonusCurrent, _ := kiroJSONNumber(bonus, "currentUsageWithPrecision", "currentUsage")
			bonusLimit, bonusLimitPresent := kiroJSONNumber(bonus, "usageLimitWithPrecision", "usageLimit")
			current += bonusCurrent
			limit += bonusLimit
			limitPresent = limitPresent || bonusLimitPresent
		}
	}
	if !limitPresent {
		return snapshot, errors.New("kiro agentic usage limit missing")
	}
	current = math.Max(0, current)
	limit = math.Max(0, limit)
	remaining := math.Max(0, limit-current)
	used := float64(-1)
	if limit > 0 {
		used = math.Min(100, math.Max(0, current/limit*100))
	} else {
		used = 100
	}
	status := "allowed"
	if remaining <= 0 {
		status = "exhausted"
	}
	reset := kiroResetAt(lookup(base, "nextDateReset"))
	if reset == 0 {
		reset = kiroResetAt(lookup(limits, "nextDateReset"))
	}
	return kiroUsageSnapshot{
		Current:     current,
		Limit:       limit,
		Remaining:   remaining,
		UsedPercent: used,
		ResetAt:     reset,
		Status:      status,
	}, nil
}

func kiroJSONNumber(m map[string]interface{}, keys ...string) (float64, bool) {
	for _, key := range keys {
		value := lookup(m, key)
		switch number := value.(type) {
		case float64:
			return number, true
		case float32:
			return float64(number), true
		case int:
			return float64(number), true
		case int64:
			return float64(number), true
		case json.Number:
			parsed, err := number.Float64()
			return parsed, err == nil
		case string:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
			return parsed, err == nil
		}
	}
	return 0, false
}

func kiroResetAt(value interface{}) int64 {
	if value == nil {
		return 0
	}
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if text == "" {
			return 0
		}
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.Unix()
		}
		if number, err := strconv.ParseFloat(text, 64); err == nil {
			return normalizeKiroEpoch(number)
		}
		return 0
	}
	switch number := value.(type) {
	case float64:
		return normalizeKiroEpoch(number)
	case float32:
		return normalizeKiroEpoch(float64(number))
	case int:
		return normalizeKiroEpoch(float64(number))
	case int64:
		return normalizeKiroEpoch(float64(number))
	case json.Number:
		parsed, err := number.Float64()
		if err == nil {
			return normalizeKiroEpoch(parsed)
		}
	}
	return 0
}

func normalizeKiroEpoch(value float64) int64 {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	// Kiro deployments have returned both epoch seconds and milliseconds.
	if value >= 1e12 {
		value /= 1000
	}
	return int64(value)
}
