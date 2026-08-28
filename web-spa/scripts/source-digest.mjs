import fs from 'node:fs';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
export const spaRoot = path.resolve(scriptDirectory, '..');

function sourceFiles(directory, base = directory) {
  const files = [];
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const absolute = path.join(directory, entry.name);
    if (entry.isDirectory()) {
      files.push(...sourceFiles(absolute, base));
    } else if (entry.isFile()) {
      files.push(path.relative(base, absolute).split(path.sep).join('/'));
    }
  }
  return files.sort();
}

// The digest is deterministic across hosts and mtimes. Paths are framed before
// bytes so renames and ambiguous concatenations cannot produce the same value.
export function computeSourceDigest(root = spaRoot) {
  const sourceRoot = path.join(root, 'src');
  const hash = createHash('sha256');
  for (const relative of sourceFiles(sourceRoot)) {
    const content = fs.readFileSync(path.join(sourceRoot, relative));
    hash.update(`${Buffer.byteLength(relative)}:`);
    hash.update(relative);
    hash.update(`${content.length}:`);
    hash.update(content);
  }
  return hash.digest('hex');
}
