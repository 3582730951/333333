import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { fetchCodexRuntimes, fetchCodexThreads } from './api';
import type { CodexThread, CodexThreadFilters, CodexThreadPage, CodexThreadStatusEvent } from './types';

export const codexThreadQueryKeys = {
  runtimes: ['codex-runtimes'] as const,
  list: (filters: CodexThreadFilters) => ['codex-threads', filters] as const,
  detail: (threadHandle: string) => ['codex-thread', threadHandle] as const,
};

type ListState = {
  filterKey: string;
  rows: CodexThread[];
  nextCursor?: string;
  backwardsCursor?: string;
  loading: boolean;
  refreshing: boolean;
  error: unknown;
  lastRefresh: Date | null;
};

function canonicalFilters(filters: CodexThreadFilters): CodexThreadFilters {
  return {
    ...filters,
    modelProviders: [...(filters.modelProviders || [])].map((value) => value.trim()).filter(Boolean).sort(),
    sourceKinds: [...(filters.sourceKinds || [])].map((value) => value.trim()).filter(Boolean).sort(),
    searchTerm: filters.searchTerm?.trim() || undefined,
  };
}

function sameThreadKey(a: CodexThread, b: CodexThread): boolean {
  return a.threadKey === b.threadKey && a.runtimeId === b.runtimeId;
}

function dedupeThreads(rows: CodexThread[]): CodexThread[] {
  const seen = new Set<string>();
  return rows.filter((row) => {
    const key = `${row.runtimeId}\u0000${row.threadKey}`;
    if (!row.threadKey || seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

function isAborted(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError';
}

export function useCodexRuntimes() {
  return useQuery({
    queryKey: codexThreadQueryKeys.runtimes,
    queryFn: ({ signal }) => fetchCodexRuntimes(signal),
    staleTime: 15_000,
    placeholderData: (previous) => previous,
  });
}

// The list intentionally owns its request generation rather than relying only
// on transport cancellation. A late proxy response can still settle after a
// newer refresh; generation matching makes that response a no-op and preserves
// the last successful page when a refresh fails.
export function useCodexThreadList(filters: CodexThreadFilters) {
  const normalized = useMemo(() => canonicalFilters(filters), [filters]);
  const filterKey = useMemo(() => JSON.stringify(normalized), [normalized]);
  const queryKey = useMemo(() => codexThreadQueryKeys.list(normalized), [normalized]);
  const [state, setState] = useState<ListState>({
    filterKey,
    rows: [],
    loading: Boolean(normalized.runtimeId),
    refreshing: false,
    error: null,
    lastRefresh: null,
  });
  const stateRef = useRef(state);
  const generationRef = useRef(0);
  const requestRef = useRef<AbortController | null>(null);
  stateRef.current = state;

  const request = useCallback(async (cursor?: string, append = false) => {
    if (!normalized.runtimeId) {
      requestRef.current?.abort();
      setState({ filterKey, rows: [], loading: false, refreshing: false, error: null, lastRefresh: null });
      return;
    }
    const generation = ++generationRef.current;
    requestRef.current?.abort();
    const controller = new AbortController();
    requestRef.current = controller;
    const hasCurrentRows = stateRef.current.filterKey === filterKey && stateRef.current.rows.length > 0;
    setState((previous) => ({
      ...(previous.filterKey === filterKey ? previous : { filterKey, rows: [], nextCursor: undefined, backwardsCursor: undefined, lastRefresh: null }),
      filterKey,
      loading: !hasCurrentRows && !append,
      refreshing: hasCurrentRows || append,
      error: null,
    }));
    try {
      const page = await fetchCodexThreads(normalized, cursor, controller.signal);
      if (generation !== generationRef.current) return;
      setState((previous) => ({
        filterKey,
        rows: append && previous.filterKey === filterKey ? dedupeThreads([...previous.rows, ...page.data]) : dedupeThreads(page.data),
        nextCursor: page.nextCursor,
        backwardsCursor: page.backwardsCursor,
        loading: false,
        refreshing: false,
        error: null,
        lastRefresh: new Date(),
      }));
    } catch (error) {
      if (generation !== generationRef.current || isAborted(error)) return;
      setState((previous) => ({
        ...(previous.filterKey === filterKey ? previous : { filterKey, rows: [], nextCursor: undefined, backwardsCursor: undefined, lastRefresh: null }),
        filterKey,
        loading: false,
        refreshing: false,
        error,
      }));
    }
  }, [filterKey, normalized]);

  useEffect(() => {
    void request();
    return () => requestRef.current?.abort();
  }, [request]);

  const reload = useCallback(() => request(), [request]);
  const loadNext = useCallback(() => {
    const cursor = stateRef.current.filterKey === filterKey ? stateRef.current.nextCursor : undefined;
    if (cursor) return request(cursor, true);
    return Promise.resolve();
  }, [filterKey, request]);
  const patchStatus = useCallback((event: CodexThreadStatusEvent) => {
    setState((previous) => {
      if (previous.filterKey !== filterKey) return previous;
      let changed = false;
      const rows = previous.rows.map((row) => {
        if (!sameThreadKey(row, event) || event.revision <= row.revision) return row;
        changed = true;
        return { ...row, ...event };
      });
      return changed ? { ...previous, rows } : previous;
    });
  }, [filterKey]);

  const visible = state.filterKey === filterKey ? state : {
    ...state,
    rows: [],
    nextCursor: undefined,
    backwardsCursor: undefined,
    loading: Boolean(normalized.runtimeId),
    refreshing: false,
    error: null,
  };
  return { ...visible, queryKey, reload, loadNext, patchStatus };
}

export type { CodexThreadPage };
