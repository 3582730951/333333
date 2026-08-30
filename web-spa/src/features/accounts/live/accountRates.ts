import { useEffect, useMemo, useSyncExternalStore } from 'react';
import { getToken } from '../../../api.js';
import { fetchAccountRates } from '../api/accounts';
import type { AccountRequestRate, AccountRow } from '../model/types';
import { dispatchBrowserEvent } from '../../../lib/browserEvents.js';
import {
  addDocumentListener,
  clearBrowserTimeout,
  isDocumentVisible,
  requestBrowserAnimationFrame,
  setBrowserTimeout,
} from '../../../lib/browserLifecycle.js';

type RateBatch = Record<string, AccountRequestRate>;
type RateListener = () => void;

const rates = new Map<string, AccountRequestRate>();
const listeners = new Map<string, Set<RateListener>>();
const batchListeners = new Set<(batch: RateBatch) => void>();
const owners = new Map<symbol, string[]>();
const pending = new Map<string, AccountRequestRate>();

let frame: unknown = null;
let eventSource: EventSource | null = null;
let reconnectTimer: unknown = null;
let pollAbort: AbortController | null = null;
let reconnectAttempt = 0;
let generation = 0;
let removeVisibilityListener: (() => void) | null = null;

const unavailableRate = (sampledAt = 0): AccountRequestRate => ({
  rpm: 0,
  logical_rpm: 0,
  attempt_rpm: 0,
  root_rpm: 0,
  subagent_rpm: 0,
  unknown_rpm: 0,
  attempt_root_rpm: 0,
  attempt_subagent_rpm: 0,
  attempt_unknown_rpm: 0,
  tpm: 0,
  input_tpm: 0,
  cached_input_tpm: 0,
  output_tpm: 0,
  window_seconds: 60,
  sampled_at: sampledAt,
  state: 'unavailable',
});

function normalizeRate(value: unknown, sampledAt = 0, windowSeconds = 60): AccountRequestRate {
  const row = value && typeof value === 'object' ? value as Partial<AccountRequestRate> : {};
  const state = row.state === 'live' || row.state === 'stale' || row.state === 'unavailable'
    ? row.state
    : 'unavailable';
  return {
    rpm: Math.max(0, Math.trunc(Number(row.rpm) || 0)),
    logical_rpm: Math.max(0, Math.trunc(Number(row.logical_rpm ?? row.rpm) || 0)),
    attempt_rpm: Math.max(0, Math.trunc(Number(row.attempt_rpm ?? row.rpm) || 0)),
    root_rpm: Math.max(0, Math.trunc(Number(row.root_rpm) || 0)),
    subagent_rpm: Math.max(0, Math.trunc(Number(row.subagent_rpm) || 0)),
    unknown_rpm: Math.max(0, Math.trunc(Number(row.unknown_rpm) || 0)),
    attempt_root_rpm: Math.max(0, Math.trunc(Number(row.attempt_root_rpm) || 0)),
    attempt_subagent_rpm: Math.max(0, Math.trunc(Number(row.attempt_subagent_rpm) || 0)),
    attempt_unknown_rpm: Math.max(0, Math.trunc(Number(row.attempt_unknown_rpm) || 0)),
    tpm: Math.max(0, Math.trunc(Number(row.tpm) || 0)),
    input_tpm: Math.max(0, Math.trunc(Number(row.input_tpm) || 0)),
    cached_input_tpm: Math.max(0, Math.trunc(Number(row.cached_input_tpm) || 0)),
    output_tpm: Math.max(0, Math.trunc(Number(row.output_tpm) || 0)),
    window_seconds: Math.max(1, Math.trunc(Number(row.window_seconds) || windowSeconds || 60)),
    sampled_at: Math.max(0, Math.trunc(Number(row.sampled_at) || sampledAt || 0)),
    state,
  };
}

function sameRate(left: AccountRequestRate, right: AccountRequestRate): boolean {
  return left.rpm === right.rpm
    && left.logical_rpm === right.logical_rpm
    && left.attempt_rpm === right.attempt_rpm
    && left.root_rpm === right.root_rpm
    && left.subagent_rpm === right.subagent_rpm
    && left.unknown_rpm === right.unknown_rpm
    && left.attempt_root_rpm === right.attempt_root_rpm
    && left.attempt_subagent_rpm === right.attempt_subagent_rpm
    && left.attempt_unknown_rpm === right.attempt_unknown_rpm
    && left.tpm === right.tpm
    && left.input_tpm === right.input_tpm
    && left.cached_input_tpm === right.cached_input_tpm
    && left.output_tpm === right.output_tpm
    && left.window_seconds === right.window_seconds
    && left.sampled_at === right.sampled_at
    && left.state === right.state;
}

function flushPending() {
  frame = null;
  if (!pending.size) return;
  const batch: RateBatch = {};
  for (const [id, next] of pending) {
    pending.delete(id);
    const previous = rates.get(id);
    if (previous && sameRate(previous, next)) continue;
    rates.set(id, next);
    batch[id] = next;
    listeners.get(id)?.forEach((listener) => listener());
  }
  if (!Object.keys(batch).length) return;
  batchListeners.forEach((listener) => listener(batch));
  let totalRPM = 0;
  for (const rate of rates.values()) {
    if (rate.state === 'live') totalRPM += rate.logical_rpm;
  }
  dispatchBrowserEvent('pool-rpm-activity', Math.min(1, totalRPM / 120));
}

function queueRates(accounts: Record<string, unknown>, sampledAt = 0, windowSeconds = 60) {
  for (const [id, value] of Object.entries(accounts || {})) {
    if (!id) continue;
    pending.set(id, normalizeRate(value, sampledAt, windowSeconds));
  }
  if (frame == null) frame = requestBrowserAnimationFrame(flushPending);
}

function unionIDs(): string[] {
  const union = new Set<string>();
  owners.forEach((ids) => ids.forEach((id) => union.add(id)));
  return [...union].slice(0, 100);
}

function closeConnection() {
  generation += 1;
  eventSource?.close();
  eventSource = null;
  pollAbort?.abort();
  pollAbort = null;
  clearBrowserTimeout(reconnectTimer);
  reconnectTimer = null;
}

function scheduleConnect(delay: number) {
  clearBrowserTimeout(reconnectTimer);
  reconnectTimer = setBrowserTimeout(() => {
    reconnectTimer = null;
    connect();
  }, delay);
}

function parseFrame(event: MessageEvent<string>) {
  try {
    const payload = JSON.parse(event.data || '{}') as {
      sampled_at?: number;
      window_seconds?: number;
      accounts?: Record<string, unknown>;
    };
    queueRates(payload.accounts || {}, Number(payload.sampled_at) || 0, Number(payload.window_seconds) || 60);
  } catch {
    // A malformed telemetry frame must not disturb the account table.
  }
}

function connectEventSource(ids: string[], currentGeneration: number) {
  const source = new EventSource(`/admin/stream/account-rates?ids=${encodeURIComponent(ids.join(','))}&interval=1`, { withCredentials: true });
  eventSource = source;
  source.onopen = () => { reconnectAttempt = 0; };
  source.addEventListener('snapshot', parseFrame as EventListener);
  source.addEventListener('delta', parseFrame as EventListener);
  source.addEventListener('handoff', () => {
    if (currentGeneration !== generation) return;
    closeConnection();
    reconnectAttempt = 0;
    scheduleConnect(0);
  });
  source.onerror = () => {
    if (currentGeneration !== generation) return;
    closeConnection();
    const delay = Math.min(30_000, 500 * (2 ** reconnectAttempt));
    reconnectAttempt += 1;
    scheduleConnect(delay);
  };
}

async function pollRates(ids: string[], currentGeneration: number) {
  const controller = new AbortController();
  pollAbort = controller;
  try {
    const payload = await fetchAccountRates(ids, controller.signal);
    if (currentGeneration !== generation || controller.signal.aborted) return;
    reconnectAttempt = 0;
    queueRates(payload.accounts, payload.sampled_at, payload.window_seconds);
    scheduleConnect(2_000);
  } catch {
    if (currentGeneration !== generation || controller.signal.aborted) return;
    const delay = Math.min(30_000, 1_000 * (2 ** reconnectAttempt));
    reconnectAttempt += 1;
    scheduleConnect(delay);
  }
}

function connect() {
  closeConnection();
  const ids = unionIDs();
  if (!ids.length || !isDocumentVisible()) return;
  const currentGeneration = generation;
  if (getToken()) {
    void pollRates(ids, currentGeneration);
    return;
  }
  connectEventSource(ids, currentGeneration);
}

function ensureVisibilityListener() {
  if (removeVisibilityListener) return;
  removeVisibilityListener = addDocumentListener('visibilitychange', () => {
    if (isDocumentVisible()) {
      reconnectAttempt = 0;
      scheduleConnect(0);
    } else {
      closeConnection();
    }
  });
}

export function seedAccountRates(rows: AccountRow[]) {
  for (const account of rows) {
    if (!account?.id || !account.request_rate) continue;
    const next = normalizeRate(account.request_rate);
    const current = rates.get(account.id);
    if (!current || next.sampled_at >= current.sampled_at) pending.set(account.id, next);
  }
  if (pending.size && frame == null) frame = requestBrowserAnimationFrame(flushPending);
}

export function subscribeAccountRateFeed(ids: string[]): () => void {
  const owner = Symbol('account-rate-feed');
  owners.set(owner, [...new Set(ids.map((id) => String(id || '').trim()).filter(Boolean))].slice(0, 100));
  ensureVisibilityListener();
  scheduleConnect(0);
  return () => {
    owners.delete(owner);
    if (owners.size) {
      scheduleConnect(0);
      return;
    }
    closeConnection();
    removeVisibilityListener?.();
    removeVisibilityListener = null;
  };
}

export function subscribeAccountRateBatches(listener: (batch: RateBatch) => void): () => void {
  batchListeners.add(listener);
  return () => batchListeners.delete(listener);
}

function subscribeAccountRate(id: string, listener: RateListener): () => void {
  let set = listeners.get(id);
  if (!set) {
    set = new Set();
    listeners.set(id, set);
  }
  set.add(listener);
  return () => {
    set?.delete(listener);
    if (!set?.size) listeners.delete(id);
  };
}

export function useAccountRequestRate(id: string, initial?: AccountRequestRate): AccountRequestRate {
  const fallback = useMemo(() => normalizeRate(initial), [
    initial?.rpm, initial?.logical_rpm, initial?.attempt_rpm,
    initial?.root_rpm, initial?.subagent_rpm, initial?.unknown_rpm,
    initial?.attempt_root_rpm, initial?.attempt_subagent_rpm, initial?.attempt_unknown_rpm,
    initial?.tpm, initial?.input_tpm, initial?.cached_input_tpm, initial?.output_tpm,
    initial?.window_seconds, initial?.sampled_at, initial?.state,
  ]);
  useEffect(() => {
    if (!id || !initial) return;
    const next = normalizeRate(initial);
    const current = rates.get(id);
    if (!current || next.sampled_at >= current.sampled_at) {
      pending.set(id, next);
      if (frame == null) frame = requestBrowserAnimationFrame(flushPending);
    }
  }, [id, initial?.rpm, initial?.logical_rpm, initial?.attempt_rpm,
    initial?.root_rpm, initial?.subagent_rpm, initial?.unknown_rpm,
    initial?.attempt_root_rpm, initial?.attempt_subagent_rpm, initial?.attempt_unknown_rpm,
    initial?.tpm, initial?.input_tpm, initial?.cached_input_tpm, initial?.output_tpm,
    initial?.window_seconds, initial?.sampled_at, initial?.state]);
  return useSyncExternalStore(
    (listener) => subscribeAccountRate(id, listener),
    () => rates.get(id) || fallback,
    () => fallback || unavailableRate(),
  );
}
