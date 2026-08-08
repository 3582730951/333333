import React, { useId } from 'react';

// Lightweight, dependency-free visualisation primitives.
//
// These deliberately avoid recharts: they sit inside KPI tiles and dense summary
// rails where a lazily-loaded chart bundle would cause a visible reflow, and where
// a full cartesian chart carries far more chrome than the data needs. Every
// primitive is driven by design tokens so light/dark and forced-colors all follow
// the rest of the console.

function clamp01(value) {
  const number = Number(value);
  if (!Number.isFinite(number)) return 0;
  return Math.max(0, Math.min(1, number));
}

// How tall a rendered line of text is as a multiple of its font-size.
//
// This is not a guess and it is not an em box. getBoundingClientRect on an SVG <text>
// returns the font's full ascent+descent, and the console's stack resolves to a CJK-capable
// face whose metrics are considerably taller than 1em — the value and caption inside the
// radial gauges were laid out 0.82 * fontSize apart and the review harness measured them
// overlapping by 4px. Back-solving that measurement (centres 27.98px apart, 4px of overlap,
// font sizes 31.68 and 12.28) gives (31.68 + 12.28) * k / 2 = 31.98, so k = 1.455.
// Rounded up for headroom, since the caption face may be taller again than the value's.
//
// The gauge ring has far more vertical room than the text block needs (a 132px gauge has a
// 110px inner diameter for a ~72px block), so erring high costs nothing visually and erring
// low puts glyphs on top of each other. If the font stack changes, re-measure: the harness
// reports the overlap in pixels, and gauge-geometry.test.ts pins the invariant.
const TEXT_BOX_RATIO = 1.5;

// Places the value and its caption inside the ring so their rendered boxes cannot touch.
//
// Both are centred as one block rather than positioned from the ring's centre independently:
// that keeps the pair optically centred whatever the strings are, and makes the separation an
// explicit gap instead of the difference between two constants. Exported for the unit test.
export function gaugeTextGeometry({ size, valueSize, captionSize, hasCaption }) {
  const valueBox = valueSize * TEXT_BOX_RATIO;
  const captionBox = captionSize * TEXT_BOX_RATIO;
  const gap = Math.max(3, size * 0.03);
  const blockHeight = hasCaption ? valueBox + gap + captionBox : valueBox;
  const blockTop = size / 2 - blockHeight / 2;
  return {
    valueY: blockTop + valueBox / 2,
    captionY: blockTop + blockHeight - captionBox / 2,
    blockHeight,
    gap,
    valueBox,
    captionBox,
  };
}

// Monotone cubic interpolation: keeps sparklines smooth without the overshoot
// that a naive Catmull-Rom spline introduces on spiky series.
function monotonePath(points) {
  const n = points.length;
  if (n === 0) return '';
  if (n === 1) return `M ${points[0][0]} ${points[0][1]}`;
  if (n === 2) return `M ${points[0][0]} ${points[0][1]} L ${points[1][0]} ${points[1][1]}`;

  const dx = [];
  const dy = [];
  const slope = [];
  for (let i = 0; i < n - 1; i += 1) {
    dx[i] = points[i + 1][0] - points[i][0];
    dy[i] = points[i + 1][1] - points[i][1];
    slope[i] = dx[i] === 0 ? 0 : dy[i] / dx[i];
  }

  const tangent = [slope[0]];
  for (let i = 1; i < n - 1; i += 1) {
    if (slope[i - 1] * slope[i] <= 0) {
      tangent[i] = 0;
    } else {
      const w1 = 2 * dx[i] + dx[i - 1];
      const w2 = dx[i] + 2 * dx[i - 1];
      tangent[i] = (w1 + w2) / (w1 / slope[i - 1] + w2 / slope[i]);
    }
  }
  tangent[n - 1] = slope[n - 2];

  let path = `M ${points[0][0]} ${points[0][1]}`;
  for (let i = 0; i < n - 1; i += 1) {
    const x0 = points[i][0];
    const y0 = points[i][1];
    const x1 = points[i + 1][0];
    const y1 = points[i + 1][1];
    const c = dx[i] / 3;
    path += ` C ${x0 + c} ${y0 + c * tangent[i]}, ${x1 - c} ${y1 - c * tangent[i + 1]}, ${x1} ${y1}`;
  }
  return path;
}

// Compact trend line for KPI tiles. Renders as an image role with a text
// alternative so the number beside it stays the accessible source of truth.
export function Sparkline({
  values = [],
  width = 120,
  height = 32,
  color = 'var(--pool-accent)',
  fill = true,
  showEndDot = true,
  ariaLabel,
  className = '',
}) {
  const gradientId = `spark-${useId().replace(/[^a-zA-Z0-9_-]/g, '')}`;
  const series = (values || []).map((value) => (Number.isFinite(Number(value)) ? Number(value) : 0));
  if (series.length < 2) {
    return <div className={`pool-sparkline pool-sparkline--empty ${className}`} style={{ height }} aria-hidden="true" />;
  }

  const min = Math.min(...series);
  const max = Math.max(...series);
  const span = max - min;
  const padY = 3;
  const usable = height - padY * 2;
  // A flat series should sit on the centre line rather than collapse to the floor.
  const toY = (value) => (span === 0 ? padY + usable / 2 : padY + usable - ((value - min) / span) * usable);
  const stepX = width / (series.length - 1);
  const points = series.map((value, index) => [index * stepX, toY(value)]);
  const line = monotonePath(points);
  const area = `${line} L ${width} ${height} L 0 ${height} Z`;
  const lastY = points[points.length - 1][1];

  return (
    <div className={`pool-sparkline-wrap ${className}`}>
      <svg
        className="pool-sparkline"
        viewBox={`0 0 ${width} ${height}`}
        preserveAspectRatio="none"
        role={ariaLabel ? 'img' : undefined}
        aria-label={ariaLabel}
        aria-hidden={ariaLabel ? undefined : true}
        focusable="false"
      >
        {fill ? (
          <defs>
            <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={color} stopOpacity="0.28" />
              <stop offset="100%" stopColor={color} stopOpacity="0.02" />
            </linearGradient>
          </defs>
        ) : null}
        {fill ? <path d={area} fill={`url(#${gradientId})`} stroke="none" /> : null}
        <path
          d={line}
          fill="none"
          stroke={color}
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
          vectorEffect="non-scaling-stroke"
        />
      </svg>
      {/* The viewBox is stretched non-uniformly, which would squash an SVG circle
          into an ellipse. The end marker is a DOM node so it stays round at any
          container aspect ratio; the last point is always at the right edge. */}
      {showEndDot ? (
        <span
          className="pool-sparkline__dot"
          style={{ top: `${(lastY / height) * 100}%`, background: color }}
          aria-hidden="true"
        />
      ) : null}
    </div>
  );
}

// Activity-ring style gauge for a single rate. The value is also printed in the
// centre so the figure is readable without interpreting the arc.
export function RadialGauge({
  value = 0,
  size = 132,
  thickness = 11,
  color = 'var(--pool-accent)',
  track = 'var(--pool-gray-soft)',
  label,
  caption,
  valueText,
  ariaLabel,
}) {
  const ratio = clamp01(value);
  const radius = (size - thickness) / 2;
  const circumference = 2 * Math.PI * radius;
  const dash = circumference * ratio;
  const display = valueText ?? `${Math.round(ratio * 100)}%`;
  // Keep the readout inside the ring even for long strings such as "100%".
  const valueSize = display.length > 5 ? size * 0.17 : display.length > 3 ? size * 0.2 : size * 0.24;
  const captionSize = Math.max(10, size * 0.093);
  const { valueY, captionY } = gaugeTextGeometry({
    size,
    valueSize,
    captionSize,
    hasCaption: !!caption,
  });

  return (
    <div className="pool-gauge">
      <svg
        className="pool-gauge__svg"
        viewBox={`0 0 ${size} ${size}`}
        role="img"
        aria-label={ariaLabel || `${label || ''} ${display}`.trim()}
        focusable="false"
      >
        <g transform={`rotate(-90 ${size / 2} ${size / 2})`}>
          <circle
            cx={size / 2}
            cy={size / 2}
            r={radius}
            fill="none"
            stroke={track}
            strokeWidth={thickness}
          />
          <circle
            cx={size / 2}
            cy={size / 2}
            r={radius}
            fill="none"
            stroke={color}
            strokeWidth={thickness}
            strokeLinecap="round"
            strokeDasharray={`${dash} ${circumference - dash}`}
          />
        </g>
        <text
          className="pool-gauge__value"
          x={size / 2}
          y={valueY}
          textAnchor="middle"
          dominantBaseline="central"
          style={{ fontSize: valueSize }}
        >
          {display}
        </text>
        {caption ? (
          <text
            className="pool-gauge__caption"
            x={size / 2}
            y={captionY}
            textAnchor="middle"
            dominantBaseline="central"
            style={{ fontSize: captionSize }}
          >
            {caption}
          </text>
        ) : null}
      </svg>
      {label ? <div className="pool-gauge__label">{label}</div> : null}
    </div>
  );
}

// Horizontal ranked bars. For "top N" breakdowns this beats a donut: the labels
// sit on their own line, values stay right-aligned and nothing has to be squeezed
// into wedge callouts.
//
// `keepZero` decides what a 0 means for the caller. For a token share it is noise
// worth dropping; for something like a depleted credit balance it is the single most
// important row on the page, so it must survive and render as an empty track.
export function RankedBars({
  rows = [],
  valueFormatter = (value) => String(value),
  emptyText,
  max: maxOverride,
  keepZero = false,
  ariaLabel,
}) {
  const items = (rows || []).filter((row) => row && Number.isFinite(Number(row.value)) && (keepZero ? Number(row.value) >= 0 : Number(row.value) > 0));
  if (!items.length) return <div className="pool-chart-empty">{emptyText || '暂无可展示的数据'}</div>;
  const max = maxOverride || Math.max(...items.map((row) => Number(row.value) || 0), 1);

  return (
    <ul className="pool-ranked" aria-label={ariaLabel}>
      {items.map((row, index) => {
        const value = Number(row.value) || 0;
        const ratio = max > 0 ? clamp01(value / max) : 0;
        return (
          <li key={row.key || row.name || index} className="pool-ranked__row">
            <div className="pool-ranked__head">
              <span className="pool-ranked__name" title={row.name}>
                <i className="pool-ranked__dot" style={{ background: row.color || 'var(--pool-accent)' }} />
                <span>{row.name}</span>
              </span>
              <b className="pool-ranked__value">{valueFormatter(row.value)}</b>
            </div>
            <div className="pool-ranked__track">
              {value > 0 ? (
                <span style={{ width: `${Math.max(ratio * 100, 1.5)}%`, background: row.color || 'var(--pool-accent)' }} />
              ) : null}
            </div>
            {row.meta ? <div className="pool-ranked__meta">{row.meta}</div> : null}
          </li>
        );
      })}
    </ul>
  );
}

// Intensity strip for evenly spaced buckets (hourly traffic, daily runs...).
// Reads as a single glanceable band rather than yet another axis-heavy chart.
export function HeatStrip({
  cells = [],
  color = 'var(--pool-accent)',
  ariaLabel,
  footer,
}) {
  const items = (cells || []).map((cell) => (typeof cell === 'object' && cell !== null ? cell : { value: cell }));
  if (!items.length) return null;
  const max = Math.max(...items.map((cell) => Number(cell.value) || 0));

  return (
    <div className="pool-heatstrip" role="img" aria-label={ariaLabel}>
      <div className="pool-heatstrip__cells">
        {items.map((cell, index) => {
          const ratio = max > 0 ? clamp01(Number(cell.value) / max) : 0;
          return (
            <span
              key={cell.key || index}
              className="pool-heatstrip__cell"
              title={cell.label ? `${cell.label} · ${cell.valueText ?? cell.value}` : undefined}
              style={{
                // Floor the alpha so empty buckets still read as a track, not a gap.
                background: ratio === 0 ? 'var(--pool-gray-soft)' : color,
                opacity: ratio === 0 ? 1 : 0.24 + ratio * 0.76,
              }}
            />
          );
        })}
      </div>
      {footer ? <div className="pool-heatstrip__footer">{footer}</div> : null}
    </div>
  );
}

// Single-track proportional bar used to show composition inside one row
// (for example cache read vs. write vs. miss).
export function StackedMeter({ segments = [], ariaLabel, legend = true, valueFormatter }) {
  const items = (segments || []).filter((segment) => Number(segment.value) > 0);
  const total = items.reduce((sum, segment) => sum + Number(segment.value), 0);
  if (!total) return null;

  return (
    <div className="pool-stacked-meter">
      <div className="pool-stacked-meter__track" role="img" aria-label={ariaLabel}>
        {items.map((segment, index) => (
          <span
            key={segment.key || segment.name || index}
            style={{ width: `${(Number(segment.value) / total) * 100}%`, background: segment.color || 'var(--pool-accent)' }}
            title={`${segment.name}: ${valueFormatter ? valueFormatter(segment.value) : segment.value}`}
          />
        ))}
      </div>
      {legend ? (
        <ul className="pool-stacked-meter__legend">
          {items.map((segment, index) => (
            <li key={segment.key || segment.name || index}>
              <i style={{ background: segment.color || 'var(--pool-accent)' }} />
              <span>{segment.name}</span>
              <b>{valueFormatter ? valueFormatter(segment.value) : segment.value}</b>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

// Delta pill: renders the direction of change next to a KPI.
export function DeltaBadge({ value, formatter, neutralText = '—', invert = false }) {
  if (value === null || value === undefined || !Number.isFinite(Number(value))) {
    return <span className="pool-delta pool-delta--flat">{neutralText}</span>;
  }
  const number = Number(value);
  const rounded = Math.abs(number) < 0.0005 ? 0 : number;
  const tone = rounded === 0 ? 'flat' : (rounded > 0) !== invert ? 'up' : 'down';
  const text = formatter ? formatter(rounded) : `${rounded > 0 ? '+' : ''}${(rounded * 100).toFixed(1)}%`;
  return (
    <span className={`pool-delta pool-delta--${tone}`}>
      <span aria-hidden="true" className="pool-delta__arrow">{rounded === 0 ? '→' : rounded > 0 ? '↑' : '↓'}</span>
      <span>{text}</span>
    </span>
  );
}
