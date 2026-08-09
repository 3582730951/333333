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
  showErrorToast: vi.fn(),
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

vi.mock('../src/components/ErrorToast.jsx', () => ({
  showErrorToast: mocks.showErrorToast,
}));

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
    mocks.oauthComplete.mockReset();
    mocks.writeClipboard.mockReset();
    mocks.showErrorToast.mockReset();
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

  it.each([
    ['missing session', { auth_url: authURL }],
    ['missing URL', { session_id: 'session-1' }],
    ['relative URL', { session_id: 'session-1', auth_url: '/oauth/start' }],
    ['unsafe URL', { session_id: 'session-1', auth_url: 'javascript:alert(1)' }],
  ])('keeps the generate action available for a malformed response: %s', async (_name, response) => {
    mocks.oauthStart.mockResolvedValue(response);
    renderLoginModal();
    fireEvent.click(screen.getByRole('button', { name: '生成授权链接' }));

    await waitFor(() => expect(mocks.showErrorToast).toHaveBeenCalledOnce());
    expect(mocks.showErrorToast.mock.calls[0][1]).toEqual({ prefix: '生成登录链接失败' });
    expect(screen.getByRole('button', { name: '生成授权链接' })).toBeInTheDocument();
    expect(screen.queryByRole('textbox', { name: '授权链接' })).not.toBeInTheDocument();
  });

  it('accepts the legacy camelCase response without losing the generated link', async () => {
    mocks.oauthStart.mockResolvedValue({ loginId: 'session-legacy', authUrl: authURL, expiresIn: 120 });
    mocks.writeClipboard.mockResolvedValue(true);
    renderLoginModal();
    fireEvent.click(screen.getByRole('button', { name: '生成授权链接' }));

    const input = await screen.findByRole('textbox', { name: '授权链接' }) as HTMLInputElement;
    expect(input.value).toBe(authURL);
    fireEvent.click(screen.getByRole('button', { name: '复制授权链接' }));
    await waitFor(() => expect(mocks.writeClipboard).toHaveBeenCalledWith(authURL));
  });

  it('shows where Claude Authentication code goes and submits the complete copied block', async () => {
    mocks.oauthComplete.mockResolvedValue({ id: 'claude-account', label: 'Claude OAuth' });
    renderLoginModal();
    const claudeTab = screen.getByRole('tab', { name: /Claude/ });
    fireEvent.mouseDown(claudeTab, { button: 0, ctrlKey: false });
    await waitFor(() => expect(claudeTab).toHaveAttribute('aria-selected', 'true'));
    fireEvent.click(screen.getByRole('button', { name: '生成授权链接' }));

    const callback = await screen.findByRole('textbox', { name: 'Claude Authentication code 或回调地址' });
    const copiedBlock = 'Authentication code\nPaste this into Claude Code: CLAUDE-CODE#CLAUDE-STATE';
    fireEvent.change(callback, { target: { value: copiedBlock } });
    expect(screen.getByText(/3\. 登录后找到 Authentication code/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: '完成授权' }));

    await waitFor(() => expect(mocks.oauthComplete).toHaveBeenCalledWith(
      'session-1', copiedBlock, '', '',
    ));
  });
});
