export const PALETTE = ['#3b82f6', '#22c55e', '#8b5cf6', '#f59e0b', '#06b6d4', '#ef4444', '#ec4899', '#14b8a6'];

export const COLORS = {
  blue: PALETTE[0],
  green: PALETTE[1],
  violet: PALETTE[2],
  amber: PALETTE[3],
  cyan: PALETTE[4],
  red: PALETTE[5],
  pink: PALETTE[6],
  teal: PALETTE[7],
  grey: '#94a3b8',
};

export function modelColor(name) {
  const s = String(name || '');
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return PALETTE[h % PALETTE.length];
}
