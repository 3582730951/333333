import '@testing-library/jest-dom/vitest';
import { afterAll, afterEach, beforeAll } from 'vitest';
import { cleanup } from '@testing-library/react';
import { setupServer } from 'msw/node';
import { loadLocale } from '../src/lib/i18n.js';

// Production main.jsx loads the selected locale before mounting React. Component
// tests import pages directly, so mirror that bootstrap invariant for both locale
// branches while keeping the production dictionaries as lazy chunks.
await Promise.all([loadLocale('zh'), loadLocale('en')]);

export const server = setupServer();

class TestResizeObserver implements ResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}

if (!globalThis.ResizeObserver) globalThis.ResizeObserver = TestResizeObserver;

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => {
  cleanup();
  server.resetHandlers();
});
afterAll(() => server.close());
