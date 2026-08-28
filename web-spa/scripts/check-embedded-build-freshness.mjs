import fs from 'node:fs';
import path from 'node:path';

import { computeSourceDigest, spaRoot } from './source-digest.mjs';

const metadataPath = path.resolve(spaRoot, '../internal/console/dist/build-meta.json');
if (!fs.existsSync(metadataPath)) {
  throw new Error(`embedded SPA freshness metadata is missing: ${metadataPath}; run npm run build`);
}

let metadata;
try {
  metadata = JSON.parse(fs.readFileSync(metadataPath, 'utf8'));
} catch (error) {
  throw new Error(`embedded SPA freshness metadata is invalid: ${error.message}`);
}

const expected = computeSourceDigest();
const actual = String(metadata?.source_sha256 || '').trim();
if (metadata?.schema !== 1 || !/^[a-f0-9]{64}$/.test(actual)) {
  throw new Error('embedded SPA freshness metadata has an unsupported schema or digest');
}
if (actual !== expected) {
  throw new Error(`embedded SPA is stale: source=${expected} dist=${actual}; run npm run build`);
}

console.log(`Embedded SPA source digest is current: ${actual}`);
