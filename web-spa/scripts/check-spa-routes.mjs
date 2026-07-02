import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const appFile = path.join(root, 'src', 'App.jsx');
const screenshotFile = path.join(root, 'screenshot_all_pages.js');

function parse(file) {
  return parser.parse(fs.readFileSync(file, 'utf8'), {
    sourceType: 'module',
    plugins: ['jsx', 'importMeta'],
  });
}

function objectStringProperty(objectNode, propertyName) {
  if (!objectNode || objectNode.type !== 'ObjectExpression') return null;
  const prop = objectNode.properties.find((p) => (
    p.type === 'ObjectProperty'
    && ((p.key.type === 'Identifier' && p.key.name === propertyName)
      || (p.key.type === 'StringLiteral' && p.key.value === propertyName))
    && p.value.type === 'StringLiteral'
  ));
  return prop?.value.value || null;
}

function objectArrayProperty(objectNode, propertyName) {
  if (!objectNode || objectNode.type !== 'ObjectExpression') return null;
  const prop = objectNode.properties.find((p) => (
    p.type === 'ObjectProperty'
    && ((p.key.type === 'Identifier' && p.key.name === propertyName)
      || (p.key.type === 'StringLiteral' && p.key.value === propertyName))
    && p.value.type === 'ArrayExpression'
  ));
  return prop?.value || null;
}

function collectVariableArray(program, variableName) {
  let result = null;
  traverse(program, {
    VariableDeclarator(pathRef) {
      if (result) return;
      if (pathRef.node.id.type !== 'Identifier' || pathRef.node.id.name !== variableName) return;
      if (pathRef.node.init?.type === 'ArrayExpression') {
        result = pathRef.node.init;
      }
    },
  });
  return result;
}

function collectNavLinksFromRouteModel(arrayNode) {
  if (!arrayNode) return [];
  const links = [];
  for (const item of arrayNode.elements || []) {
    if (!item || item.type !== 'ObjectExpression') continue;
    const children = objectArrayProperty(item, 'children');
    if (children) {
      links.push(...collectNavLinksFromRouteModel(children));
      continue;
    }
    const target = objectStringProperty(item, 'navPath') || objectStringProperty(item, 'path');
    if (target?.startsWith('/')) links.push(target);
  }
  return links;
}

function collectRoutePathsFromRouteModel(arrayNode) {
  if (!arrayNode) return [];
  const routes = [];
  for (const item of arrayNode.elements || []) {
    if (!item || item.type !== 'ObjectExpression') continue;
    const routePath = objectStringProperty(item, 'path');
    if (routePath) routes.push(routePath);
    const children = objectArrayProperty(item, 'children');
    if (children) routes.push(...collectRoutePathsFromRouteModel(children));
  }
  return routes;
}

function collectRedirectsFromRouteModel(arrayNode) {
  if (!arrayNode) return [];
  const redirects = [];
  for (const item of arrayNode.elements || []) {
    if (!item || item.type !== 'ObjectExpression') continue;
    const routePath = objectStringProperty(item, 'path');
    const redirectTo = objectStringProperty(item, 'redirectTo');
    if (routePath && redirectTo) redirects.push({ routePath, redirectTo });
    const children = objectArrayProperty(item, 'children');
    if (children) redirects.push(...collectRedirectsFromRouteModel(children));
  }
  return redirects;
}

function routeExists(target, routes) {
  return routes.includes(stripHashAndSearch(target));
}

function stripHashAndSearch(target) {
  return target.split(/[?#]/, 1)[0] || '/';
}

function normalizeConsolePath(pathname) {
  return pathname.startsWith('/console') ? pathname.slice('/console'.length) || '/' : pathname;
}

function collectScreenshotPaths(file) {
  if (!fs.existsSync(file)) return [];
  const ast = parse(file);
  const paths = [];
  traverse(ast, {
    ObjectExpression(pathRef) {
      const value = objectStringProperty(pathRef.node, 'path');
      if (value) paths.push(value);
    },
  });
  return paths;
}

function addProblem(problems, message) {
  problems.push(message);
}

const appAst = parse(appFile);
const adminModel = collectVariableArray(appAst, 'ADMIN_ROUTE_MODEL');
const adminExtraRoutes = collectVariableArray(appAst, 'ADMIN_EXTRA_ROUTES');
const portalModel = collectVariableArray(appAst, 'PORTAL_ROUTE_MODEL');
const adminNav = collectNavLinksFromRouteModel(adminModel);
const portalNav = collectNavLinksFromRouteModel(portalModel);
const routes = [
  ...collectRoutePathsFromRouteModel(adminModel),
  ...collectRoutePathsFromRouteModel(adminExtraRoutes),
  ...collectRoutePathsFromRouteModel(portalModel),
];
const redirects = [
  ...collectRedirectsFromRouteModel(adminModel),
  ...collectRedirectsFromRouteModel(adminExtraRoutes),
  ...collectRedirectsFromRouteModel(portalModel),
];
const screenshotPaths = collectScreenshotPaths(screenshotFile).map(normalizeConsolePath);
const problems = [];

if (!adminModel) addProblem(problems, 'missing ADMIN_ROUTE_MODEL');
if (!adminExtraRoutes) addProblem(problems, 'missing ADMIN_EXTRA_ROUTES');
if (!portalModel) addProblem(problems, 'missing PORTAL_ROUTE_MODEL');

for (const link of adminNav) {
  if (link.startsWith('/portal')) {
    addProblem(problems, `admin nav links into portal route: ${link}`);
  }
  if (!routeExists(link, routes)) {
    addProblem(problems, `admin nav target has no Route: ${link}`);
  }
}

for (const link of portalNav) {
  if (!link.startsWith('/portal')) {
    addProblem(problems, `portal nav target must stay under /portal: ${link}`);
  }
  if (!routeExists(link, routes)) {
    addProblem(problems, `portal nav target has no Route: ${link}`);
  }
}

for (const link of [...adminNav, ...portalNav]) {
  const duplicates = [...adminNav, ...portalNav].filter((item) => item === link);
  if (duplicates.length > 1) {
    addProblem(problems, `duplicate nav target: ${link}`);
  }
}

for (const route of routes) {
  const duplicates = routes.filter((item) => item === route);
  if (duplicates.length > 1) {
    addProblem(problems, `duplicate Route path: ${route}`);
  }
}

for (const redirect of redirects) {
  if (!routeExists(redirect.redirectTo, routes)) {
    addProblem(problems, `redirect target has no Route: ${redirect.routePath} -> ${redirect.redirectTo}`);
  }
}

for (const screenshotPath of screenshotPaths) {
  if (!routeExists(screenshotPath, routes)) {
    addProblem(problems, `screenshot path has no Route: ${screenshotPath}`);
  }
}

if (problems.length > 0) {
  console.error('SPA route consistency check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('SPA route consistency check passed.');
