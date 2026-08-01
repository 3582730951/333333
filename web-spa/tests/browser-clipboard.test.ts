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
