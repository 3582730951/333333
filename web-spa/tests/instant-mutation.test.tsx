import React, { type PropsWithChildren } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import useInstantMutation from '../src/hooks/useInstantMutation';

function wrapper({ children }: PropsWithChildren) {
  return <QueryClientProvider client={new QueryClient({ defaultOptions: { mutations: { retry: false } } })}>{children}</QueryClientProvider>;
}

describe('useInstantMutation lifecycle', () => {
  it('coalesces clicks until an async success callback has completed', async () => {
    let releaseSuccess = () => {};
    const mutationFn = vi.fn(async () => ({ ok: true }));
    const onSuccess = vi.fn(() => new Promise<void>((resolve) => { releaseSuccess = resolve; }));
    const { result } = renderHook(() => useInstantMutation({ mutationFn, onSuccess }), { wrapper });

    let first!: Promise<{ ok: boolean }>;
    let second!: Promise<{ ok: boolean }>;
    act(() => {
      first = result.current.run('value');
      second = result.current.run('value');
    });
    expect(second).toBe(first);
    await waitFor(() => expect(onSuccess).toHaveBeenCalledTimes(1));
    expect(mutationFn).toHaveBeenCalledTimes(1);
    expect(result.current.pending).toBe(true);

    await act(async () => {
      releaseSuccess();
      await first;
    });
    expect(result.current.phase).toBe('settled');
    expect(result.current.pending).toBe(false);
  });

  it('surfaces success callback failures without rolling back a committed request', async () => {
    const error = new Error('render failed');
    const rollback = vi.fn();
    const { result } = renderHook(() => useInstantMutation({
      mutationFn: vi.fn(async () => 'ok'),
      optimistic: () => ({ previous: true }),
      rollback,
      onSuccess: async () => { throw error; },
    }), { wrapper });

    let pending!: Promise<string>;
    act(() => { pending = result.current.run('value'); });
    await act(async () => {
      await expect(pending).rejects.toBe(error);
    });
    expect(rollback).not.toHaveBeenCalled();
    expect(result.current.phase).toBe('error');
    expect(result.current.pending).toBe(false);
    expect(result.current.error?.cause).toBe(error);
  });

  it('awaits rollback on request failure and then rejects the original request', async () => {
    const requestError = new Error('request failed');
    const rollback = vi.fn().mockResolvedValue(undefined);
    const { result } = renderHook(() => useInstantMutation({
      mutationFn: vi.fn().mockRejectedValue(requestError),
      optimistic: () => undefined,
      rollback,
    }), { wrapper });

    let pending!: Promise<unknown>;
    act(() => { pending = result.current.run('value'); });
    await act(async () => {
      await expect(pending).rejects.toBe(requestError);
    });
    expect(rollback).toHaveBeenCalledWith(undefined, 'value', requestError);
    expect(result.current.phase).toBe('error');
    expect(result.current.pending).toBe(false);
  });

  it('clears an optimistic failure and permits a later run', async () => {
    const optimisticError = new Error('optimistic failed');
    let fail = true;
    const mutationFn = vi.fn(async () => 'ok');
    const { result } = renderHook(() => useInstantMutation({
      mutationFn,
      optimistic: () => {
        if (fail) throw optimisticError;
        return { previous: true };
      },
    }), { wrapper });

    let first!: Promise<string>;
    act(() => { first = result.current.run('value'); });
    await act(async () => {
      await expect(first).rejects.toBe(optimisticError);
    });
    expect(result.current.pending).toBe(false);

    fail = false;
    let second!: Promise<string>;
    act(() => { second = result.current.run('value'); });
    await act(async () => { await expect(second).resolves.toBe('ok'); });
    expect(mutationFn).toHaveBeenCalledTimes(1);
    expect(result.current.phase).toBe('settled');
  });

  it('clears a rollback failure and permits a later run', async () => {
    const requestError = new Error('request failed');
    const rollbackError = new Error('rollback failed');
    const mutationFn = vi.fn<() => Promise<string>>()
      .mockRejectedValueOnce(requestError)
      .mockResolvedValueOnce('ok');
    const rollback = vi.fn().mockRejectedValueOnce(rollbackError);
    const { result } = renderHook(() => useInstantMutation({
      mutationFn,
      optimistic: () => ({ previous: true }),
      rollback,
    }), { wrapper });

    let first!: Promise<string>;
    act(() => { first = result.current.run('value'); });
    await act(async () => {
      await expect(first).rejects.toBe(rollbackError);
    });
    expect(result.current.pending).toBe(false);

    let second!: Promise<string>;
    act(() => { second = result.current.run('value'); });
    await act(async () => { await expect(second).resolves.toBe('ok'); });
    expect(mutationFn).toHaveBeenCalledTimes(2);
    expect(rollback).toHaveBeenCalledTimes(1);
    expect(result.current.phase).toBe('settled');
  });
});
