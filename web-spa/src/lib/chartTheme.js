export const PALETTE = [
  'var(--chart-blue)',
  'var(--chart-green)',
  'var(--chart-purple)',
  'var(--chart-orange)',
  'var(--pool-info)',
  'var(--chart-red)',
  'var(--pool-purple)',
  'var(--pool-success)',
];

export const COLORS = {
  blue: PALETTE[0],
  green: PALETTE[1],
  violet: PALETTE[2],
  amber: PALETTE[3],
  cyan: PALETTE[4],
  red: PALETTE[5],
  pink: PALETTE[6],
  teal: PALETTE[7],
  grey: 'var(--chart-gray)',
};

export function modelColor(name) {
  const s = String(name || '');
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return PALETTE[h % PALETTE.length];
}
