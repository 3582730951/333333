import { afterEach, describe, expect, it, vi } from 'vitest';
import { downloadBlob } from '../src/lib/browserDownload.js';

describe('browser download', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('keeps the object URL alive until the browser has started the download', () => {
    vi.useFakeTimers();
    const createObjectURL = vi.fn(() => 'blob:diagnostic-archive');
    const revokeObjectURL = vi.fn();
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: createObjectURL });
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: revokeObjectURL });
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);

    expect(downloadBlob('diagnostics.zip', new Blob(['PK\u0003\u0004']))).toBe(true);
    expect(click).toHaveBeenCalledTimes(1);
    expect(createObjectURL).toHaveBeenCalledTimes(1);
    expect(revokeObjectURL).not.toHaveBeenCalled();
    expect(document.querySelector('a[download="diagnostics.zip"]')).toBeNull();

    vi.advanceTimersByTime(999);
    expect(revokeObjectURL).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:diagnostic-archive');
  });
});
