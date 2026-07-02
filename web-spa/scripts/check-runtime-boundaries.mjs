import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const srcRoot = path.join(root, 'src');
const storageModule = path.join(srcRoot, 'lib', 'browserStorage.js');
const cookieModule = path.join(srcRoot, 'lib', 'browserCookies.js');
const eventModule = path.join(srcRoot, 'lib', 'browserEvents.js');
const clipboardModule = path.join(srcRoot, 'lib', 'browserClipboard.js');
const navigationModule = path.join(srcRoot, 'lib', 'browserNavigation.js');
const downloadModule = path.join(srcRoot, 'lib', 'browserDownload.js');
const networkModule = path.join(srcRoot, 'lib', 'browserNetwork.js');
const abortModule = path.join(srcRoot, 'lib', 'browserAbort.js');
const lifecycleModule = path.join(srcRoot, 'lib', 'browserLifecycle.js');
const documentModule = path.join(srcRoot, 'lib', 'browserDocument.js');
const browserStoragePattern = /\b(?:localStorage|sessionStorage)\b/g;
const browserCookiePattern = /\bdocument\.cookie\b|\bdecodeURIComponent\b/g;
const browserEventPattern = /\bnew\s+(?:CustomEvent|EventTarget)\b|\bwindow\.dispatchEvent\b/g;
const browserClipboardPattern = /\bnavigator\.clipboard\b|\bdocument\.execCommand\b|\bdocument\.createElement\s*\(\s*['"]textarea['"]\s*\)/g;
const browserNavigationPattern = /\bwindow\.(?:open|location)\b/g;
const browserDownloadPattern = /\bURL\.(?:createObjectURL|revokeObjectURL)\b|\bdocument\.createElement\s*\(\s*['"]a['"]\s*\)|\.download\s*=/g;
const browserNetworkPattern = /\bfetch\s*\(|\bnavigator\.(?:sendBeacon|userAgent|connection|mozConnection|webkitConnection)\b/g;
const browserAbortPattern = /\bnew\s+AbortController\b|(?:\.|\?\.)abort(?:\?\.)?\s*\(/g;
const browserLifecyclePattern = /\bwindow\.(?:addEventListener|removeEventListener|setTimeout|clearTimeout|setInterval|clearInterval|requestIdleCallback|cancelIdleCallback|requestAnimationFrame|cancelAnimationFrame|innerWidth)\b|\bdocument\.(?:addEventListener|removeEventListener|visibilityState)\b|\b(?:setTimeout|clearTimeout|setInterval|clearInterval)\s*\(/g;
const browserDocumentPattern = /\bdocument\.(?:body|getElementById)\b|\bnew\s+MutationObserver\b|\bMutationObserver\b/g;

function listSourceFiles(dir) {
  const files = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const fullPath = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...listSourceFiles(fullPath));
    } else if (/\.[cm]?jsx?$/.test(entry.name)) {
      files.push(fullPath);
    }
  }
  return files;
}

function lineNumber(source, index) {
  return source.slice(0, index).split('\n').length;
}

function checkBrowserStorageAccess() {
  return checkPatternOutsideModule(browserStoragePattern, storageModule, 'browser storage');
}

function checkBrowserCookieAccess() {
  return checkPatternOutsideModule(browserCookiePattern, cookieModule, 'browser cookie');
}

function checkBrowserEventAccess() {
  return checkPatternOutsideModule(browserEventPattern, eventModule, 'browser event');
}

function checkBrowserClipboardAccess() {
  return checkPatternOutsideModule(browserClipboardPattern, clipboardModule, 'browser clipboard');
}

function checkBrowserNavigationAccess() {
  return checkPatternOutsideModule(browserNavigationPattern, navigationModule, 'browser navigation');
}

function checkBrowserDownloadAccess() {
  return checkPatternOutsideModule(browserDownloadPattern, downloadModule, 'browser download');
}

function checkBrowserNetworkAccess() {
  return checkPatternOutsideModule(browserNetworkPattern, networkModule, 'browser network');
}

function checkBrowserAbortAccess() {
  return checkPatternOutsideModule(browserAbortPattern, abortModule, 'browser abort');
}

function checkBrowserLifecycleAccess() {
  return checkPatternOutsideModule(browserLifecyclePattern, lifecycleModule, 'browser lifecycle');
}

function checkBrowserDocumentAccess() {
  return checkPatternOutsideModules(
    browserDocumentPattern,
    [documentModule, clipboardModule, downloadModule],
    'browser document',
  );
}

function checkPatternOutsideModule(pattern, allowedModule, label) {
  return checkPatternOutsideModules(pattern, [allowedModule], label);
}

function checkPatternOutsideModules(pattern, allowedModules, label) {
  const failures = [];
  const allowed = new Set(allowedModules);
  for (const file of listSourceFiles(srcRoot)) {
    if (allowed.has(file)) continue;
    const source = fs.readFileSync(file, 'utf8');
    for (const match of source.matchAll(pattern)) {
      failures.push(`${path.relative(root, file)}:${lineNumber(source, match.index)} uses ${label} primitive ${match[0]} directly`);
    }
  }
  return failures;
}

function checkAppIsAdminOrdering() {
  const appFile = path.join(srcRoot, 'App.jsx');
  const source = fs.readFileSync(appFile, 'utf8');
  const declaration = source.indexOf('const isAdmin = auth.role ===');
  const dependencyUse = source.indexOf('[auth.ready, auth.authed, isAdmin]');
  if (declaration === -1 || dependencyUse === -1 || declaration > dependencyUse) {
    return ['src/App.jsx must declare isAdmin before hook dependency arrays reference it.'];
  }
  return [];
}

const failures = [
  ...checkBrowserStorageAccess(),
  ...checkBrowserCookieAccess(),
  ...checkBrowserEventAccess(),
  ...checkBrowserClipboardAccess(),
  ...checkBrowserNavigationAccess(),
  ...checkBrowserDownloadAccess(),
  ...checkBrowserNetworkAccess(),
  ...checkBrowserAbortAccess(),
  ...checkBrowserLifecycleAccess(),
  ...checkBrowserDocumentAccess(),
  ...checkAppIsAdminOrdering(),
];

if (failures.length > 0) {
  console.error('Frontend runtime boundary check failed:');
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log('Frontend runtime boundary check passed.');
