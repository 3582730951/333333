import { getLocale } from './i18n.js';

const intlLocale = () => getLocale() === 'en' ? 'en-US' : 'zh-CN';
const numberFormatter = () => new Intl.NumberFormat(intlLocale(), { maximumFractionDigits: 0 });

export const fmtInt = (value) => (value == null ? '—' : numberFormatter().format(Number(value) || 0));

export function fmtTokens(value) {
  const n = Number(value) || 0;
  return new Intl.NumberFormat(intlLocale(), {
    notation: Math.abs(n) >= 1000 ? 'compact' : 'standard',
    maximumFractionDigits: Math.abs(n) >= 1e6 ? 2 : 1,
  }).format(n);
}

export function fmtBytes(value) {
  let bytes = Number(value) || 0;
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let index = 0;
  while (Math.abs(bytes) >= 1024 && index < units.length - 1) { bytes /= 1024; index += 1; }
  return `${new Intl.NumberFormat(intlLocale(), { maximumFractionDigits: index ? 1 : 0 }).format(bytes)} ${units[index]}`;
}

export const fmtKB = (kb) => fmtBytes((Number(kb) || 0) * 1024);

// Shortens an identifier from the middle, keeping both ends.
//
// Account ids, emails and error strings are frequently distinguished only by their tail —
// acct_prod_primary_001 vs acct_prod_primary_002 — so a trailing ellipsis throws away the one
// part the reader needs, and letting the cell wrap breaks the token at an arbitrary character
// (…_spend_00 / 2), which is worse still: the suffix is there but no longer legible as one.
//
// Callers pass the full string as a title so nothing is lost. Previously copy-pasted into
// Accounts (24/14) and EmailPool (22/14); the defaults here are EmailPool's, and both keep
// passing explicit lengths where the column width called for something different.
export function middleEllipsis(value, head = 22, tail = 14) {
  const text = String(value ?? '');
  if (text.length <= head + tail + 1) return text;
  return `${text.slice(0, head)}…${text.slice(-tail)}`;
}

export function fmtDuration(value) {
  const seconds = Math.max(0, Number(value) || 0);
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const unit = getLocale() === 'en'
    ? { day: 'd', hour: 'h', minute: 'm' }
    : { day: '天', hour: '小时', minute: '分' };
  if (days) return `${days}${unit.day} ${hours}${unit.hour}`;
  if (hours) return `${hours}${unit.hour} ${minutes}${unit.minute}`;
  return `${minutes}${unit.minute}`;
}

function dateValue(epochSec) {
  const value = Number(epochSec);
  return Number.isFinite(value) && value > 0 ? new Date(value * 1000) : null;
}

export function fmtTime(epochSec) {
  const date = dateValue(epochSec);
  if (!date) return '';
  return new Intl.DateTimeFormat(intlLocale(), { hour: '2-digit', minute: '2-digit', hour12: false }).format(date);
}

export function fmtDateTime(epochSec) {
  const date = dateValue(epochSec);
  if (!date) return '—';
  return new Intl.DateTimeFormat(intlLocale(), {
    month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false,
  }).format(date);
}

export function fmtRelative(epochSec) {
  if (!epochSec) return '—';
  const diff = Number(epochSec) - Math.floor(Date.now() / 1000);
  const formatter = new Intl.RelativeTimeFormat(intlLocale(), { numeric: 'auto' });
  if (Math.abs(diff) < 60) return formatter.format(Math.round(diff), 'second');
  if (Math.abs(diff) < 3600) return formatter.format(Math.round(diff / 60), 'minute');
  if (Math.abs(diff) < 86400) return formatter.format(Math.round(diff / 3600), 'hour');
  return formatter.format(Math.round(diff / 86400), 'day');
}
