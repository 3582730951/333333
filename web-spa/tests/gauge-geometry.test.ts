import { describe, expect, it } from 'vitest';
import { gaugeTextGeometry } from '../src/components/MicroCharts.jsx';

// The radial gauge stacks a big value over a small caption inside the ring. That pairing
// shipped overlapping: the caption was placed at valueSize * 0.82 below the ring's centre,
// a constant unrelated to either string's rendered height, and the UI review harness caught
// the two glyph boxes intersecting by 4px on Dashboard and Quota in both themes.
//
// These cover the invariant rather than the numbers: whatever the gauge size and however long
// the value string, the two boxes must stay apart and the block must stay inside the ring.

// Mirrors RadialGauge: the value shrinks as the string grows so it stays inside the ring.
function valueSizeFor(size: number, display: string): number {
  return display.length > 5 ? size * 0.17 : display.length > 3 ? size * 0.2 : size * 0.24;
}

function captionSizeFor(size: number): number {
  return Math.max(10, size * 0.093);
}

// Every size actually used by a call site, plus the component default.
const SIZES = [132, 128, 148, 96, 72, 200];
// Real readouts: percentages, the em-dash empty state, and the widest formatted values.
const VALUES = ['0%', '61%', '77%', '100%', '—', '1.23M', '12.5K/s', '999.9K/s'];

describe('gaugeTextGeometry', () => {
  it('never lets the value and caption boxes touch', () => {
    const touching: string[] = [];
    for (const size of SIZES) {
      for (const display of VALUES) {
        const valueSize = valueSizeFor(size, display);
        const captionSize = captionSizeFor(size);
        const geom = gaugeTextGeometry({ size, valueSize, captionSize, hasCaption: true });
        const valueBottom = geom.valueY + geom.valueBox / 2;
        const captionTop = geom.captionY - geom.captionBox / 2;
        if (captionTop - valueBottom < geom.gap - 1e-6) {
          touching.push(`${size}px "${display}": gap ${(captionTop - valueBottom).toFixed(2)}`);
        }
      }
    }
    expect(touching).toEqual([]);
  });

  it('keeps the text block inside the ring', () => {
    // A 132px gauge draws an 11px ring, leaving a 110px inner diameter. Scale that ratio.
    const tooTall: string[] = [];
    for (const size of SIZES) {
      const thickness = size === 132 ? 11 : Math.max(6, size * 0.083);
      const innerDiameter = size - thickness * 2;
      for (const display of VALUES) {
        const geom = gaugeTextGeometry({
          size,
          valueSize: valueSizeFor(size, display),
          captionSize: captionSizeFor(size),
          hasCaption: true,
        });
        if (geom.blockHeight > innerDiameter) {
          tooTall.push(`${size}px "${display}": ${geom.blockHeight.toFixed(1)} > ${innerDiameter.toFixed(1)}`);
        }
      }
    }
    expect(tooTall).toEqual([]);
  });

  it('centres the block on the ring so the pair reads as optically centred', () => {
    for (const size of SIZES) {
      const geom = gaugeTextGeometry({
        size,
        valueSize: valueSizeFor(size, '61%'),
        captionSize: captionSizeFor(size),
        hasCaption: true,
      });
      const blockTop = geom.valueY - geom.valueBox / 2;
      expect(blockTop + geom.blockHeight / 2).toBeCloseTo(size / 2, 6);
    }
  });

  it('centres a lone value when there is no caption', () => {
    const geom = gaugeTextGeometry({
      size: 132,
      valueSize: valueSizeFor(132, '61%'),
      captionSize: captionSizeFor(132),
      hasCaption: false,
    });
    expect(geom.valueY).toBeCloseTo(66, 6);
  });

  it('separates the boxes by more than the old formula did', () => {
    // Regression guard on the specific case the harness measured: a 132px gauge showing
    // "61%" over "请求". The old placement put the centres 27.98px apart; the boxes need
    // more than that to clear each other.
    const valueSize = valueSizeFor(132, '61%');
    const captionSize = captionSizeFor(132);
    const geom = gaugeTextGeometry({ size: 132, valueSize, captionSize, hasCaption: true });
    const centreDistance = geom.captionY - geom.valueY;
    const halfBoxes = geom.valueBox / 2 + geom.captionBox / 2;
    expect(centreDistance).toBeGreaterThan(halfBoxes);
    expect(centreDistance).toBeGreaterThan(27.98);
  });
});
