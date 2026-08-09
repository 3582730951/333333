// Hourly bucketing for the heat strips on the admin pages.
//
// Several endpoints return a flat list of recent records stamped with unix seconds -- model
// quality runs, supervisor events -- and the useful question about all of them is the same one:
// is this spread evenly across the day, or concentrated in one hour? A count alone cannot answer
// it, and every page that asked was about to reimplement the same twenty lines of slot
// arithmetic. The bucket window ends at the newest record rather than at "now" so a snapshot
// taken hours after the last event still shows where the events were, instead of an empty band
// pushed off the left edge.

import { fmtDateTime } from './format.js';

const SECONDS_PER_HOUR = 3600;

/**
 * Bucket records into `hours` consecutive hourly slots ending at the hour of the newest record.
 *
 * @param items    records to bucket
 * @param timeOf   reads the unix-second stamp off a record; falsy results skip the record
 * @param series   named predicates or weights, each `(item) => boolean | number`
 * @param hours    slot count, default 24
 * @returns `null` when no record carries a usable stamp, otherwise `{ slots, counts, totals,
 *          from, to }` where `slots` holds the epoch second each slot starts at, `counts` maps
 *          each series name to a per-slot array, and `totals` maps it to the window sum.
 */
export function hourlyBuckets(items, { timeOf, series, hours = 24 }) {
  const names = Object.keys(series);
  let newest = 0;
  for (const item of items) {
    const stamp = Number(timeOf(item)) || 0;
    if (stamp > newest) newest = stamp;
  }
  if (!newest) return null;

  const endHour = Math.floor(newest / SECONDS_PER_HOUR);
  /** @type {Record<string, number[]>} */
  const counts = {};
  for (const name of names) counts[name] = new Array(hours).fill(0);

  for (const item of items) {
    const stamp = Number(timeOf(item)) || 0;
    if (!stamp) continue;
    const slot = hours - 1 - (endHour - Math.floor(stamp / SECONDS_PER_HOUR));
    if (slot < 0 || slot >= hours) continue;
    for (const name of names) {
      // A predicate returning true counts as one, so callers can pass either a filter or a
      // weight without declaring which they meant.
      const weight = Number(series[name](item)) || 0;
      if (weight) counts[name][slot] += weight;
    }
  }

  const slots = [];
  for (let index = 0; index < hours; index += 1) {
    slots.push((endHour - (hours - 1 - index)) * SECONDS_PER_HOUR);
  }
  /** @type {Record<string, number>} */
  const totals = {};
  for (const name of names) totals[name] = counts[name].reduce((sum, value) => sum + value, 0);

  return { slots, counts, totals, from: slots[0], to: slots[slots.length - 1] };
}

/**
 * Shape one bucketed series into HeatStrip cells. `unit` is appended to the value in the cell's
 * accessible text, which is what a screen reader and the tooltip both read.
 */
export function heatCells(values, slots, unit) {
  return values.map((value, index) => ({
    key: index,
    value,
    label: fmtDateTime(slots[index]),
    valueText: `${value} ${unit}`,
  }));
}
