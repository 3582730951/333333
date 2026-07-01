// Small formatting helpers shared across pages.

export const fmtInt = (n) => (n == null ? '—' : Number(n).toLocaleString('en-US'));

export function fmtTokens(n) {
  n = Number(n) || 0;
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}

export function fmtBytes(bytes) {
  bytes = Number(bytes) || 0;
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (bytes >= 1024 && i < u.length - 1) { bytes /= 1024; i++; }
  return (i === 0 ? bytes : bytes.toFixed(1)) + ' ' + u[i];
}

export const fmtKB = (kb) => fmtBytes((Number(kb) || 0) * 1024);

export function fmtDuration(sec) {
  sec = Number(sec) || 0;
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d}天 ${h}小时`;
  if (h > 0) return `${h}小时 ${m}分`;
  return `${m}分`;
}

const pad = (n) => String(n).padStart(2, '0');

export function fmtTime(epochSec) {
  if (!epochSec) return '';
  const d = new Date(Number(epochSec) * 1000);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function fmtDateTime(epochSec) {
  if (!epochSec) return '—';
  const d = new Date(Number(epochSec) * 1000);
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function fmtRelative(epochSec) {
  if (!epochSec) return '—';
  const diff = Math.floor(Date.now() / 1000) - Number(epochSec);
  if (diff < 0) return 'in ' + fmtDuration(-diff);
  if (diff < 60) return diff + '秒前';
  if (diff < 3600) return Math.floor(diff / 60) + '分钟前';
  if (diff < 86400) return Math.floor(diff / 3600) + '小时前';
  return Math.floor(diff / 86400) + '天前';
}
