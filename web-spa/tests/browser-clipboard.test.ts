import { afterEach, describe, expect, it, vi } from 'vitest';
import { selectTextForManualCopy, writeClipboard } from '../src/lib/browserClipboard.js';

function setSecureContext(value: boolean) {
  Object.defineProperty(window, 'isSecureContext', { configurable: true, value });
}

function setClipboard(writeText: ReturnType<typeof vi.fn> | undefined) {
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: writeText ? { writeText } : undefined,
  });
}

describe('browser clipboard', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    Reflect.deleteProperty(document, 'execCommand');
    setClipboard(undefined);
    document.body.replaceChildren();
  });

  it('uses the Clipboard API in a secure context', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    const execCommand = vi.fn();
    setSecureContext(true);
    setClipboard(writeText);
    Object.defineProperty(document, 'execCommand', { configurable: true, value: execCommand });

    await expect(writeClipboard('https://example.test/oauth')).resolves.toBe(true);
    expect(writeText).toHaveBeenCalledWith('https://example.test/oauth');
    expect(execCommand).not.toHaveBeenCalled();
  });

  it('copies synchronously through a textarea on an HTTP deployment and restores focus', async () => {
    const original = document.createElement('button');
    document.body.appendChild(original);
    original.focus();
    const execCommand = vi.fn().mockReturnValue(true);
    setSecureContext(false);
    setClipboard(undefined);
    Object.defineProperty(document, 'execCommand', { configurable: true, value: execCommand });

    await expect(writeClipboard('legacy-copy')).resolves.toBe(true);
    expect(execCommand).toHaveBeenCalledWith('copy');
    expect(document.activeElement).toBe(original);
    expect(document.querySelector('textarea')).toBeNull();
  });

  it('uses an exposed Clipboard API even when an embedded browser reports an insecure context', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    const execCommand = vi.fn();
    setSecureContext(false);
    setClipboard(writeText);
    Object.defineProperty(document, 'execCommand', { configurable: true, value: execCommand });

    await expect(writeClipboard('https://example.test/embedded')).resolves.toBe(true);
    expect(writeText).toHaveBeenCalledWith('https://example.test/embedded');
    expect(execCommand).not.toHaveBeenCalled();
  });

  it('falls back to the synchronous DOM copy path when Clipboard permission is rejected', async () => {
    const writeText = vi.fn().mockRejectedValue(new DOMException('blocked', 'NotAllowedError'));
    const execCommand = vi.fn().mockReturnValue(true);
    setSecureContext(true);
    setClipboard(writeText);
    Object.defineProperty(document, 'execCommand', { configurable: true, value: execCommand });

    await expect(writeClipboard('https://example.test/fallback')).resolves.toBe(true);
    expect(writeText).toHaveBeenCalledOnce();
    expect(execCommand).toHaveBeenCalledWith('copy');
  });

  it('does not report success when the generated value is empty', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    const execCommand = vi.fn().mockReturnValue(true);
    setSecureContext(true);
    setClipboard(writeText);
    Object.defineProperty(document, 'execCommand', { configurable: true, value: execCommand });

    await expect(writeClipboard('')).resolves.toBe(false);
    expect(writeText).not.toHaveBeenCalled();
    expect(execCommand).not.toHaveBeenCalled();
  });

  it('selects the visible value when programmatic copy is unavailable', () => {
    const input = document.createElement('input');
    input.value = 'https://example.test/manual';
    document.body.appendChild(input);

    expect(selectTextForManualCopy(input)).toBe(true);
    expect(document.activeElement).toBe(input);
    expect(input.selectionStart).toBe(0);
    expect(input.selectionEnd).toBe(input.value.length);
  });
});
