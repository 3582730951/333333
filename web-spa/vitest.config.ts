import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    include: ['./tests/**/*.test.ts', './tests/**/*.test.tsx'],
    environment: 'jsdom',
    setupFiles: ['./tests/setup.ts'],
    restoreMocks: true,
    coverage: {
      provider: 'v8',
      reporter: ['text', 'html'],
      include: ['src/api/**/*.ts', 'src/model/**/*.ts', 'src/features/**/*.ts', 'src/lib/breakpoints.ts'],
    },
  },
});
