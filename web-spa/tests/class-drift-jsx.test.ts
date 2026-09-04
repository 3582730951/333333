import { readFileSync } from 'node:fs';
import path from 'node:path';

import { describe, expect, it } from 'vitest';

import { collectOwnedClassesFromJsx } from '../scripts/lib/class-drift-jsx.mjs';

const fixtureDir = path.join(process.cwd(), 'tests', 'fixtures', 'class-drift');
const readFixture = (name: string) => readFileSync(path.join(fixtureDir, name), 'utf8');

const staticFixtures = [
  ['string-literal.fixture.tsx', ['pool-fixture-string-literal-missing']],
  ['template-literal.fixture.tsx', [
    'pool-fixture-template-direct-missing',
    'pool-fixture-template-hole-a-missing',
    'pool-fixture-template-hole-b-missing',
  ]],
  ['conditional-expression.fixture.tsx', [
    'pool-fixture-conditional-a-missing',
    'pool-fixture-conditional-b-missing',
  ]],
  ['logical-expression.fixture.tsx', ['pool-fixture-logical-missing']],
  ['array-expression.fixture.tsx', [
    'pool-fixture-array-missing',
    'pool-fixture-array-conditional-missing',
  ]],
  ['clsx-call.fixture.tsx', [
    'pool-fixture-clsx-missing',
    'pool-fixture-clsx-logical-missing',
    'pool-fixture-clsx-object-missing',
  ]],
] as const;

describe('class drift JSX attribute extraction', () => {
  it.each(staticFixtures)('extracts literals from %s', (file, expectedClasses) => {
    const result = collectOwnedClassesFromJsx(readFixture(file), { file: `tests/fixtures/class-drift/${file}` });
    expect(result.classes).toEqual(expect.arrayContaining([...expectedClasses]));
    expect(result.indeterminate).toEqual([]);
  });

  it('reports a dynamic prefix interpolation instead of silently dropping it', () => {
    const file = 'identifier-interpolation.fixture.tsx';
    const result = collectOwnedClassesFromJsx(readFixture(file), { file: `tests/fixtures/class-drift/${file}` });
    expect(result.classes).toEqual([]);
    expect(result.indeterminate).toEqual([
      expect.objectContaining({
        expression: '`pool-fixture-${state}`',
        reason: 'template interpolation cannot be statically determined',
      }),
    ]);
  });

  it('covers bare condition, logical, array, and clsx forms that the former RHS regex skipped', () => {
    const legacyRhs = /className\s*=\s*(?:"([^"]*)"|\{`([^`]*)`\}|\{'([^']*)'\})/g;
    for (const file of [
      'conditional-expression.fixture.tsx',
      'logical-expression.fixture.tsx',
      'array-expression.fixture.tsx',
      'clsx-call.fixture.tsx',
    ]) {
      expect([...readFixture(file).matchAll(legacyRhs)]).toEqual([]);
    }
  });

  it('keeps extracting the literal states inside App.tsx template holes', () => {
    const source = readFileSync(path.join(process.cwd(), 'src', 'App.tsx'), 'utf8');
    const result = collectOwnedClassesFromJsx(source, { file: 'src/App.tsx' });
    expect(result.classes).toEqual(expect.arrayContaining([
      'pool-app-mobile',
      'pool-admin-shell',
      'pool-portal-shell',
      'pool-sidebar-is-collapsed',
    ]));
  });
});
