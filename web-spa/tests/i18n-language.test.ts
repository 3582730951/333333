import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getLocale, setLocale, syncDocumentLanguage } from '../src/lib/i18n.js';

const mainEntryMocks = vi.hoisted(() => {
  const render = vi.fn();
  return {
    render,
    createRoot: vi.fn(() => ({ render })),
  };
});

vi.mock('react-dom/client', () => ({
  default: { createRoot: mainEntryMocks.createRoot },
}));

describe('document language synchronization', () => {
  beforeEach(() => {
    window.localStorage.clear();
    setLocale('zh');
    document.documentElement.removeAttribute('lang');
    document.body.innerHTML = '<div id="root"></div>';
    mainEntryMocks.createRoot.mockClear();
    mainEntryMocks.render.mockClear();
  });

  afterEach(() => {
    setLocale('zh');
    window.localStorage.clear();
  });

  it('executes the real main entry language wiring for a saved English locale', async () => {
    window.localStorage.setItem('pool_locale', 'en');
    vi.resetModules();

    await import('../src/main.jsx');

    expect(document.documentElement.lang).toBe('en');
    expect(mainEntryMocks.createRoot).toHaveBeenCalledWith(document.getElementById('root'));
    expect(mainEntryMocks.render).toHaveBeenCalledTimes(1);
  }, 30_000);

  it('keeps storage, locale, and document language aligned when switching to Chinese', () => {
    setLocale('en');
    expect(getLocale()).toBe('en');
    expect(window.localStorage.getItem('pool_locale')).toBe('en');
    expect(document.documentElement.lang).toBe('en');

    setLocale('zh');

    expect(getLocale()).toBe('zh');
    expect(window.localStorage.getItem('pool_locale')).toBe('zh');
    expect(document.documentElement.lang).toBe('zh-CN');
  });

  it('normalizes unsupported locales to Chinese', () => {
    setLocale('fr');

    expect(getLocale()).toBe('zh');
    expect(document.documentElement.lang).toBe('zh-CN');
  });
});
