// CSV export helpers (client-side, no secrets — exports only the visible columns).

import { downloadTextFile } from './browserDownload.js';

// columns: [{ title, get }] where get(row) returns the cell's plain value.
export function toCSV(rows, columns) {
  const esc = (v) => {
    const s = v == null ? '' : String(v);
    return /[",\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
  };
  const header = columns.map((c) => esc(c.title)).join(',');
  const body = (rows || []).map((r) => columns.map((c) => esc(c.get(r))).join(',')).join('\n');
  return header + '\n' + body;
}

export function downloadCSV(name, text) {
  // Prepend a UTF-8 BOM so Excel renders CJK correctly.
  return downloadTextFile(name, `\uFEFF${text}`, 'text/csv;charset=utf-8');
}
