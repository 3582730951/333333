import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { beforeEach, describe, expect, it, vi } from 'vitest';

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  oauthStart: vi.fn(),
  oauthComplete: vi.fn(),
  post: vi.fn(),
  writeClipboard: vi.fn(),
}));

vi.mock('../src/api.js', () => ({
  get: mocks.get,
  oauthStart: mocks.oauthStart,
  oauthComplete: mocks.oauthComplete,
  post: mocks.post,
}));

vi.mock('../src/lib/browserClipboard.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../src/lib/browserClipboard.js')>();
  return { ...actual, writeClipboard: mocks.writeClipboard };
});

import OAuthLoginModal from '../src/components/OAuthLoginModal.jsx';

const LoginModal = OAuthLoginModal as any;
const authURL = 'https://example.test/oauth?state=copy-test';

function renderLoginModal() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <LoginModal open onClose={() => {}} />
    </QueryClientProvider>,
  );
}

describe('OAuth authorization link copy', () => {
  beforeEach(() => {
    mocks.get.mockResolvedValue([]);
    mocks.oauthStart.mockResolvedValue({ session_id: 'session-1', auth_url: authURL, expires_in: 900 });
    mocks.writeClipboard.mockReset();
  });

  it('copies the generated link from an explicitly labelled button', async () => {
    mocks.writeClipboard.mockResolvedValue(true);
    renderLoginModal();
    fireEvent.click(screen.getByRole('button', { name: '生成授权链接' }));
    const copy = await screen.findByRole('button', { name: '复制授权链接' });
    fireEvent.click(copy);

    await waitFor(() => expect(mocks.writeClipboard).toHaveBeenCalledWith(authURL));
    expect(await screen.findByRole('button', { name: '授权链接已复制' })).toBeInTheDocument();
  });

  it('selects the visible authorization link when browser copy is blocked', async () => {
    mocks.writeClipboard.mockResolvedValue(false);
    renderLoginModal();
    fireEvent.click(screen.getByRole('button', { name: '生成授权链接' }));
    fireEvent.click(await screen.findByRole('button', { name: '复制授权链接' }));

    const input = await screen.findByRole('textbox', { name: '授权链接' }) as HTMLInputElement;
    await waitFor(() => expect(document.activeElement).toBe(input));
    expect(input.selectionStart).toBe(0);
    expect(input.selectionEnd).toBe(authURL.length);
  });
});
