import React, { type PropsWithChildren } from 'react';
import { QueryClient, QueryClientProvider, useQuery } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { useQueryView } from '../src/features/shared/queries';

describe('useQueryView', () => {
  it('keeps cached data interactive during background refresh', async () => {
    let resolveRefresh: (value: { version: number }) => void = () => {};
    const queryFn = vi.fn()
      .mockResolvedValueOnce({ version: 1 })
      .mockImplementationOnce(() => new Promise<{ version: number }>((resolve) => {
        resolveRefresh = resolve;
      }));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    );
    const { result } = renderHook(() => useQueryView(useQuery({
      queryKey: ['query-view-refresh'],
      queryFn,
    })), { wrapper });

    await waitFor(() => expect(result.current.data).toEqual({ version: 1 }));
    expect(result.current.loading).toBe(false);

    let reload: Promise<{ version: number } | undefined> = Promise.resolve(undefined);
    act(() => { reload = result.current.reload(); });
    await waitFor(() => expect(result.current.refreshing).toBe(true));
    expect(result.current.loading).toBe(false);
    expect(result.current.data).toEqual({ version: 1 });

    await act(async () => {
      resolveRefresh({ version: 2 });
      await reload;
    });
    await waitFor(() => {
      expect(result.current.refreshing).toBe(false);
      expect(result.current.data).toEqual({ version: 2 });
    });
  });
});
