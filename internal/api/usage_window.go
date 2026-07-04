package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
)

const usageCacheStatsResetSettingKey = "usage_cache_stats_reset_at"

type adminUsageWindowInfo struct {
	Timezone               string `json:"timezone"`
	UTCOffsetSeconds       int    `json:"utc_offset_seconds"`
	DayStartAt             int64  `json:"day_start_at"`
	NextDayStartAt         int64  `json:"next_day_start_at"`
	NowAt                  int64  `json:"now_at"`
	CacheStatsResetAt      int64  `json:"cache_stats_reset_at,omitempty"`
	CacheStatsResetInvalid bool   `json:"cache_stats_reset_invalid,omitempty"`
	CustomSinceProvided    bool   `json:"custom_since_provided,omitempty"`
	CustomUntilProvided    bool   `json:"custom_until_provided,omitempty"`
}

type adminUsageWindow struct {
	Window           adminUsageWindowInfo `json:"window"`
	WindowMode       string               `json:"window_mode"`
	EffectiveStartAt int64                `json:"effective_start_at"`
	EffectiveUntilAt int64                `json:"effective_until_at"`
}

func (w adminUsageWindow) responseFields() map[string]interface{} {
	return map[string]interface{}{
		"window":             w.Window,
		"window_mode":        w.WindowMode,
		"effective_start_at": w.EffectiveStartAt,
		"effective_until_at": w.EffectiveUntilAt,
	}
}

func (w adminUsageWindow) storageUntilAt() int64 {
	if !w.Window.CustomUntilProvided && w.EffectiveUntilAt == w.Window.NowAt {
		return w.EffectiveUntilAt + 1
	}
	return w.EffectiveUntilAt
}

func mergeWindowFields(base map[string]interface{}, win adminUsageWindow) map[string]interface{} {
	for k, v := range win.responseFields() {
		base[k] = v
	}
	return base
}

func (s *Server) adminUsageWindowEndpoint(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	now := time.Now()
	if err := s.store.EnsureUsageDailyResetAudit(r.Context(), now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	win, err := s.resolveAdminUsageWindow(r, now, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cacheWin, err := s.resolveAdminUsageWindow(r, now, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	body := win.responseFields()
	body["cache_effective_start_at"] = cacheWin.EffectiveStartAt
	body["cache_window"] = cacheWin.Window
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) adminUsageCacheReset(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	now := time.Now()
	if err := s.store.EnsureUsageDailyResetAudit(r.Context(), now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if r.URL.Query().Has("since") || r.URL.Query().Has("until") {
		writeError(w, http.StatusBadRequest, errors.New("cache reset does not accept custom since/until"))
		return
	}
	resetValue := now.UTC().Format(time.RFC3339Nano)
	if err := s.store.SetSetting(r.Context(), usageCacheStatsResetSettingKey, resetValue); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
		Action:    "usage_cache_stats_reset",
		State:     "ok",
		Reason:    "manual_reset",
		Detail:    "advanced admin cache diagnostics baseline; usage_records and user usage were not deleted",
		CreatedAt: now.Unix(),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	win, err := s.resolveAdminUsageWindow(r, now, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	body := win.responseFields()
	body["reset_at"] = now.Unix()
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) resolveAdminUsageWindow(r *http.Request, now time.Time, cacheWindow bool) (adminUsageWindow, error) {
	localNow := now.In(time.Local)
	dayStartTime := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.Local)
	nextDayStartTime := dayStartTime.AddDate(0, 0, 1)
	nowUnix := now.Unix()
	_, offset := localNow.Zone()
	info := adminUsageWindowInfo{
		Timezone:         localTimezoneName(localNow),
		UTCOffsetSeconds: offset,
		DayStartAt:       dayStartTime.Unix(),
		NextDayStartAt:   nextDayStartTime.Unix(),
		NowAt:            nowUnix,
	}
	q := r.URL.Query()
	_, hasSince := q["since"]
	_, hasUntil := q["until"]
	if hasSince || hasUntil {
		start := info.DayStartAt
		until := nowUnix
		if hasSince {
			n, err := parseUnixQuery(q.Get("since"), "since")
			if err != nil {
				return adminUsageWindow{}, err
			}
			if n < 0 {
				return adminUsageWindow{}, errors.New("since must be >= 0")
			}
			start = n
			info.CustomSinceProvided = true
		}
		if hasUntil {
			n, err := parseUnixQuery(q.Get("until"), "until")
			if err != nil {
				return adminUsageWindow{}, err
			}
			if n > nowUnix+300 {
				return adminUsageWindow{}, errors.New("until must not be more than 300 seconds in the future")
			}
			until = n
			info.CustomUntilProvided = true
		}
		if until <= start {
			return adminUsageWindow{}, errors.New("until must be greater than since")
		}
		return adminUsageWindow{
			Window:           info,
			WindowMode:       "custom",
			EffectiveStartAt: start,
			EffectiveUntilAt: until,
		}, nil
	}
	start := info.DayStartAt
	until := nowUnix
	if cacheWindow {
		resetAt, invalid, err := s.usageCacheStatsResetAt(r, now)
		if err != nil {
			return adminUsageWindow{}, err
		}
		if resetAt > 0 {
			info.CacheStatsResetAt = resetAt
			if resetAt > start {
				start = resetAt
			}
			if start > until {
				start = until
			}
		}
		info.CacheStatsResetInvalid = invalid
	}
	return adminUsageWindow{
		Window:           info,
		WindowMode:       "default",
		EffectiveStartAt: start,
		EffectiveUntilAt: until,
	}, nil
}

func parseUnixQuery(raw, name string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("%s must be a Unix second integer", name)
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Unix second integer", name)
	}
	return n, nil
}

func (s *Server) usageCacheStatsResetAt(r *http.Request, now time.Time) (int64, bool, error) {
	value, ok, err := s.store.GetSetting(r.Context(), usageCacheStatsResetSettingKey)
	if err != nil || !ok || strings.TrimSpace(value) == "" {
		return 0, false, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err == nil {
		return parsed.Unix(), false, nil
	}
	_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
		Action:    "usage_cache_stats_reset_invalid",
		State:     "ignored",
		Reason:    "invalid_setting",
		Detail:    "ignored invalid usage_cache_stats_reset_at setting",
		CreatedAt: now.Unix(),
	})
	return 0, true, nil
}

func localTimezoneName(t time.Time) string {
	name, _ := t.Zone()
	if strings.TrimSpace(name) != "" {
		return name
	}
	if time.Local != nil && strings.TrimSpace(time.Local.String()) != "" {
		return time.Local.String()
	}
	return "Local"
}
