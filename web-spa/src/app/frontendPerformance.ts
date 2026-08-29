import { useSyncExternalStore } from 'react';
import { postJSONKeepalive } from '../lib/browserNetwork.js';
import {
  addDocumentListener, addWindowListener, clearBrowserInterval, setBrowserInterval,
} from '../lib/browserLifecycle.js';

export type FrontendPerformanceSnapshot = {
  lcp_ms: number;
  cls: number;
  inp_ms: number;
  ttfb_ms: number;
  long_task_count: number;
  long_task_ms: number;
  route_intent_commit_ms: number;
  route_commit_data_ready_ms: number;
  mutation_accept_ms: number;
  mutation_settled_ms: number;
  sampled_at: number;
};

const EMPTY: FrontendPerformanceSnapshot = {
  lcp_ms: 0,
  cls: 0,
  inp_ms: 0,
  ttfb_ms: 0,
  long_task_count: 0,
  long_task_ms: 0,
  route_intent_commit_ms: 0,
  route_commit_data_ready_ms: 0,
  mutation_accept_ms: 0,
  mutation_settled_ms: 0,
  sampled_at: 0,
};

let snapshot = EMPTY;
let started = false;
let cleanup: (() => void) | null = null;
const listeners = new Set<() => void>();
const trace: Array<{ name: string; duration: number; at: number }> = [];

function publish(patch: Partial<FrontendPerformanceSnapshot>) {
  snapshot = { ...snapshot, ...patch, sampled_at: Math.floor(Date.now() / 1000) };
  for (const listener of listeners) listener();
}

function remember(name: string, duration: number) {
  if (!Number.isFinite(duration) || duration < 0) return;
  trace.push({ name, duration: Math.round(duration * 100) / 100, at: Date.now() });
  if (trace.length > 120) trace.splice(0, trace.length - 120);
}

function observe(type: string, handle: (entries: PerformanceEntry[]) => void) {
  if (typeof PerformanceObserver !== 'function') return () => {};
  try {
    const observer = new PerformanceObserver((list) => handle(list.getEntries()));
    observer.observe({ type, buffered: true } as PerformanceObserverInit);
    return () => observer.disconnect();
  } catch {
    return () => {};
  }
}

function report() {
  if (!snapshot.sampled_at) return;
  postJSONKeepalive('/client/performance', snapshot, () => {});
}

export function startFrontendPerformance() {
  if (started || typeof window === 'undefined') return cleanup || (() => {});
  started = true;
  const navigation = performance.getEntriesByType('navigation')[0] as PerformanceNavigationTiming | undefined;
  if (navigation) publish({ ttfb_ms: Math.max(0, navigation.responseStart - navigation.requestStart) });

  const disposers = [
    observe('largest-contentful-paint', (entries) => {
      const last = entries.at(-1);
      if (last) publish({ lcp_ms: last.startTime });
    }),
    observe('layout-shift', (entries) => {
      let cls = snapshot.cls;
      for (const entry of entries as Array<PerformanceEntry & { value?: number; hadRecentInput?: boolean }>) {
        if (!entry.hadRecentInput) cls += Number(entry.value) || 0;
      }
      publish({ cls });
    }),
    observe('event', (entries) => {
      let inp = snapshot.inp_ms;
      for (const entry of entries) inp = Math.max(inp, entry.duration || 0);
      publish({ inp_ms: inp });
    }),
    observe('longtask', (entries) => {
      const total = entries.reduce((sum, entry) => sum + entry.duration, 0);
      publish({
        long_task_count: snapshot.long_task_count + entries.length,
        long_task_ms: snapshot.long_task_ms + total,
      });
    }),
    observe('measure', (entries) => {
      const patch: Partial<FrontendPerformanceSnapshot> = {};
      for (const entry of entries) {
        remember(entry.name, entry.duration);
        if (entry.name === 'pool:route:intent-to-commit') patch.route_intent_commit_ms = entry.duration;
        if (entry.name === 'pool:route:commit-to-data') patch.route_commit_data_ready_ms = entry.duration;
        if (entry.name === 'pool:mutation:intent-to-accepted') patch.mutation_accept_ms = entry.duration;
        if (entry.name === 'pool:mutation:accepted-to-settled') patch.mutation_settled_ms = entry.duration;
      }
      if (Object.keys(patch).length) publish(patch);
    }),
  ];
  const interval = setBrowserInterval(report, 30_000);
  const removePageHide = addWindowListener('pagehide', report);
  const removeVisibility = addDocumentListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') report();
  });
  cleanup = () => {
    disposers.forEach((dispose) => dispose());
    clearBrowserInterval(interval);
    removePageHide();
    removeVisibility();
    cleanup = null;
    started = false;
  };
  return cleanup;
}

export function markRouteIntent() {
  try { performance.mark('pool:route:intent'); } catch { /* unsupported */ }
}

export function markRouteCommit() {
  try {
    performance.mark('pool:route:commit');
    performance.measure('pool:route:intent-to-commit', 'pool:route:intent', 'pool:route:commit');
  } catch { /* an initial route has no intent mark */ }
}

export function markRouteDataReady() {
  try {
    performance.mark('pool:route:data-ready');
    performance.measure('pool:route:commit-to-data', 'pool:route:commit', 'pool:route:data-ready');
  } catch { /* an initial route can become ready before a commit mark */ }
}

export function getFrontendPerformanceSnapshot() {
  return snapshot;
}

export function subscribeFrontendPerformance(listener: () => void) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function useFrontendPerformanceSnapshot() {
  return useSyncExternalStore(subscribeFrontendPerformance, getFrontendPerformanceSnapshot, () => EMPTY);
}

export function exportFrontendPerformanceTrace() {
  return JSON.stringify({ snapshot, measures: trace }, null, 2);
}
