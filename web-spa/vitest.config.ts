import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    include: ['./tests/**/*.test.ts', './tests/**/*.test.tsx'],
    environment: 'jsdom',
    setupFiles: ['./tests/setup.ts'],
    restoreMocks: true,
    // jsdom workers have a high per-process startup/memory cost. Letting Vitest
    // mirror a large host CPU count made the 1-core/1-GB deployment gate spawn
    // dozens of forks and intermittently time out before a test file started.
    // Keep the default deterministic; larger CI hosts can opt in explicitly.
    maxWorkers: Math.max(1, Number.parseInt(process.env.VITEST_MAX_WORKERS || '2', 10) || 2),
    coverage: {
      provider: 'v8',
      reporter: ['text', 'html'],
      include: ['src/api/**/*.ts', 'src/model/**/*.ts', 'src/features/**/*.ts', 'src/lib/breakpoints.ts'],
    },
  },
});
