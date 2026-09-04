#!/usr/bin/env node
/**
 * Aurora P0 bundle measurement for the embedded console build.
 *
 * HTML is parsed with parse5 and JavaScript import edges with @babel/parser;
 * gzip sizes are measured directly from the assets that Go embeds.
 *
 * Usage:
 *   node scripts/audit/measure-bundle.mjs --out /tmp/aurora-p0-bundle.json
 */
import fs from 'node:fs';
import path from 'node:path';
import { gzipSync } from 'node:zlib';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';
import { parse as parseHtml } from 'parse5';

const traverse = traverseModule.default;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const dist = path.resolve(root, '../internal/console/dist');

function readArg(name) {
  const index = process.argv.indexOf(name);
  return index === -1 ? null : process.argv[index + 1] || null;
}

function normalizeAsset(value) {
  const withoutQuery = value.split('?')[0];
  const marker = '/console/';
  const index = withoutQuery.indexOf(marker);
  return index === -1 ? withoutQuery.replace(/^\//, '') : withoutQuery.slice(index + marker.length);
}

function attributes(node) {
  return Object.fromEntries((node.attrs || []).map((attribute) => [attribute.name, attribute.value]));
}

function collectInitial(document) {
  const found = new Set();
  const visit = (node) => {
    if (node.nodeName === 'script') {
      const attrs = attributes(node);
      if (attrs.type === 'module' && attrs.src?.endsWith('.js')) found.add(normalizeAsset(attrs.src));
    }
    if (node.nodeName === 'link') {
      const attrs = attributes(node);
      if (attrs.rel === 'modulepreload' && attrs.href?.endsWith('.js')) found.add(normalizeAsset(attrs.href));
    }
    for (const child of node.childNodes || []) visit(child);
  };
  visit(document);
  return found;
}

function importsOf(asset) {
  const source = fs.readFileSync(path.join(dist, asset), 'utf8');
  const ast = parser.parse(source, { sourceType: 'module', plugins: ['importAttributes'] });
  const imports = [];
  traverse(ast, {
    ImportDeclaration(pathRef) { imports.push(pathRef.node.source.value); },
    ExportNamedDeclaration(pathRef) { if (pathRef.node.source) imports.push(pathRef.node.source.value); },
    ExportAllDeclaration(pathRef) { imports.push(pathRef.node.source.value); },
  });
  return imports;
}

function staticGraph(initial) {
  const graph = new Set(initial);
  const visit = (asset) => {
    for (const specifier of importsOf(asset)) {
      if (!specifier.startsWith('.')) continue;
      const child = path.posix.normalize(path.posix.join(path.posix.dirname(asset), specifier));
      if (!child.endsWith('.js') || graph.has(child)) continue;
      graph.add(child);
      visit(child);
    }
  };
  for (const asset of initial) visit(asset);
  return graph;
}

function gzipBytes(asset) {
  return gzipSync(fs.readFileSync(path.join(dist, asset))).length;
}

function totalBytes(assets) {
  return [...assets].reduce((sum, asset) => sum + gzipBytes(asset), 0);
}

const html = parseHtml(fs.readFileSync(path.join(dist, 'index.html'), 'utf8'));
const initial = collectInitial(html);
const graph = staticGraph(initial);
const assets = fs.readdirSync(path.join(dist, 'assets')).filter((asset) => asset.endsWith('.js'));
const allChunks = assets.map((asset) => ({
  asset: `assets/${asset}`,
  rawBytes: fs.statSync(path.join(dist, 'assets', asset)).size,
  gzipBytes: gzipBytes(`assets/${asset}`),
  initial: graph.has(`assets/${asset}`),
})).sort((a, b) => b.gzipBytes - a.gzipBytes || a.asset.localeCompare(b.asset));
const atmosphere = allChunks.filter((entry) => /AtmosphereLayer|atmosphere/i.test(entry.asset));
const charts = allChunks.filter((entry) => /Charts|chart/i.test(entry.asset));
const output = {
  generatedAt: new Date().toISOString(),
  dist: path.relative(root, dist).replaceAll(path.sep, '/'),
  initial: {
    entries: [...initial].sort(),
    staticGraph: [...graph].sort(),
    gzipBytes: totalBytes(initial),
    staticGraphGzipBytes: totalBytes(graph),
    budgetBytes: 256 * 1024,
    headroomBytes: 256 * 1024 - totalBytes(initial),
  },
  atmosphere,
  charts,
  chunks: allChunks,
};
const out = readArg('--out');
const text = `${JSON.stringify(output, null, 2)}\n`;
if (out) {
  fs.mkdirSync(path.dirname(path.resolve(out)), { recursive: true });
  fs.writeFileSync(out, text);
} else process.stdout.write(text);
console.error(`Aurora P0 bundle: ${output.initial.gzipBytes} initial gzip bytes; ${output.initial.headroomBytes} bytes to 256 KiB; output ${out || 'stdout'}`);
