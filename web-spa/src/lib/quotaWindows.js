const HOUR_SECONDS = 60 * 60;
const DAY_SECONDS = 24 * HOUR_SECONDS;

// The Claude OAuth endpoint supplies token counts for both its five-hour and
// seven-day windows. Codex stream observations only publish a percentage for
// 7d_polled, so that limiter must not join a Token total.
const TOKEN_METERED_LIMITERS = new Set([
  'unified',
  'tokens',
  'input_tokens',
  'output_tokens',
  '5h_oauth_usage',
  '7d_oauth_usage',
]);

function isRecord(value) {
  return value !== null && typeof value === 'object';
}

function finiteNumberOrNull(value) {
  if (value == null || value === '') return null;
  const number = Number(value);
  return Number.isFinite(number) ? number : null;
}

function hasOwn(object, key) {
  return isRecord(object) && Object.prototype.hasOwnProperty.call(object, key);
}

// A quota window's meaning belongs to the window metadata, never to whether
// the backend happened to place it in primary or secondary.
export function quotaWindowKind(window) {
  if (!isRecord(window)) return null;
  const limiter = String(window.limiter_type || '').trim().toLowerCase();
  if (limiter.includes('7d') || limiter.includes('seven_day') || limiter.includes('seven-day')) return '7d';
  if (limiter.includes('5h') || limiter.includes('five_hour') || limiter.includes('five-hour')) return '5h';

  const seconds = finiteNumberOrNull(window.limit_window_seconds);
  if (seconds == null || seconds <= 0) return null;
  // Allow the small duration drift seen in upstream window reports while not
  // misclassifying monthly or other non-plan windows as a seven-day quota.
  if (seconds >= 6 * DAY_SECONDS && seconds <= 8 * DAY_SECONDS) return '7d';
  if (seconds >= 4 * HOUR_SECONDS && seconds <= 6 * HOUR_SECONDS) return '5h';
  return null;
}

export function quotaWindowLabel(window, fallback = '主额度窗口') {
  const kind = quotaWindowKind(window);
  if (kind === '5h') return '5 小时窗口';
  if (kind === '7d') return '7 天窗口';

  const seconds = finiteNumberOrNull(window?.limit_window_seconds);
  if (seconds != null && seconds > 0) {
    if (seconds % DAY_SECONDS === 0) return `${seconds / DAY_SECONDS} 天窗口`;
    if (seconds % HOUR_SECONDS === 0) return `${seconds / HOUR_SECONDS} 小时窗口`;
    if (seconds % 60 === 0) return `${seconds / 60} 分钟窗口`;
  }
  const limiter = String(window?.limiter_type || '').trim();
  return limiter ? `${limiter} 窗口` : fallback;
}

export function quotaSummaryWindows(summary) {
  if (!isRecord(summary)) return [];
  return [summary.primary, summary.secondary].filter(isRecord);
}

export function quotaWindowForKind(row, kind) {
  return quotaSummaryWindows(row?.quota_summary).find((window) => quotaWindowKind(window) === kind) || null;
}

// The table's "5h" heading is historical UI shorthand for the primary quota
// window. Providers also use names such as tokens, unified, kiro_usage, and
// cursor_monthly for that primary window, so only a known 7d primary is absent
// from this column.
export function quotaWindowUsage(row, kind) {
  const summary = row?.quota_summary;
  if (isRecord(summary)) {
    const primary = isRecord(summary.primary) ? summary.primary : null;

    if (kind === '5h') {
      if (!primary || quotaWindowKind(primary) === '7d') {
        return { exists: false, usedPercent: null };
      }
      return { exists: true, usedPercent: finiteNumberOrNull(primary.used_percent) };
    }

    if (kind === '7d') {
      const window = quotaWindowForKind(row, '7d');
      return window
        ? { exists: true, usedPercent: finiteNumberOrNull(window.used_percent) }
        : { exists: false, usedPercent: null };
    }

    return { exists: false, usedPercent: null };
  }

  // Flat fields belong to the legacy response shape only. Do not revive them
  // once quota_summary is present: secondary_7d_used_pct is a non-omitempty Go
  // float and is therefore 0 even when no 7d window exists. In the legacy
  // shape, used_percent describes its primary (unless that primary is known
  // 7d), while secondary_7d_used_pct explicitly describes the 7d window.
  const primaryKind = quotaWindowKind(row);
  if (kind === '5h') {
    if (primaryKind === '7d') return { exists: false, usedPercent: null };
    return {
      exists: hasOwn(row, 'used_percent'),
      usedPercent: finiteNumberOrNull(row?.used_percent),
    };
  }

  if (kind === '7d') {
    if (primaryKind === '7d') {
      return {
        exists: hasOwn(row, 'used_percent'),
        usedPercent: finiteNumberOrNull(row?.used_percent),
      };
    }
    return {
      exists: hasOwn(row, 'secondary_7d_used_pct'),
      usedPercent: finiteNumberOrNull(row?.secondary_7d_used_pct),
    };
  }

  return { exists: false, usedPercent: null };
}

// Mobile cards need to distinguish an absent window from a reported window
// whose percentage is unavailable. Keep that distinction in one pure helper so
// callers can render the latter as "unknown" instead of dropping its row.
export function quotaUsageDetails(row) {
  const details = [];
  for (const kind of ['5h', '7d']) {
    const usage = quotaWindowUsage(row, kind);
    if (usage.exists) details.push({ kind, usedPercent: usage.usedPercent });
  }
  return details;
}

export function quotaWindowUsedPercent(row, kind) {
  return quotaWindowUsage(row, kind).usedPercent;
}

export function isTokenMeteredQuotaLimiter(limiter) {
  return TOKEN_METERED_LIMITERS.has(String(limiter || '').trim().toLowerCase());
}
