import fs from 'node:fs';
import path from 'node:path';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;

export const EXPECTED_ROUTE_COUNTS = Object.freeze({ admin: 28, portal: 7, total: 35 });

function unwrapExpression(node) {
  let current = node;
  while (current && [
    'TSAsExpression',
    'TSSatisfiesExpression',
    'TypeCastExpression',
    'TSInstantiationExpression',
  ].includes(current.type)) current = current.expression;
  return current;
}

function keyName(node) {
  if (node?.type === 'Identifier') return node.name;
  if (node?.type === 'StringLiteral') return node.value;
  return null;
}

function objectProperty(object, name) {
  return object?.properties.find((property) => property.type === 'ObjectProperty'
    && keyName(property.key) === name) || null;
}

function literalValue(node) {
  const expression = unwrapExpression(node);
  return expression?.type === 'StringLiteral' ? expression.value : null;
}

function lazyLoaderSpecifier(node) {
  const loader = unwrapExpression(node);
  const body = loader?.type === 'ArrowFunctionExpression' ? unwrapExpression(loader.body) : null;
  if (body?.type === 'ImportExpression') return literalValue(body.source);
  if (body?.type === 'CallExpression' && body.callee.type === 'Import') return literalValue(body.arguments[0]);
  return null;
}

function routeArrayFromDeclaration(declaration, name, sourceFile) {
  const expression = unwrapExpression(declaration?.init);
  if (expression?.type !== 'ArrayExpression') {
    throw new Error(`${path.basename(sourceFile)}: could not statically read ${name} as an array`);
  }

  const routes = [];
  for (const element of expression.elements) {
    const route = unwrapExpression(element);
    if (route?.type !== 'ObjectExpression') {
      throw new Error(`${path.basename(sourceFile)}: ${name} contains a non-object route entry`);
    }
    const routePath = literalValue(objectProperty(route, 'path')?.value);
    const titleKey = literalValue(objectProperty(route, 'titleKey')?.value);
    const lazyLoader = lazyLoaderSpecifier(objectProperty(route, 'lazyLoader')?.value);
    if (!routePath || !titleKey || !lazyLoader) {
      throw new Error(`${path.basename(sourceFile)}: ${name} has a route without static path/titleKey/lazyLoader`);
    }
    routes.push({ path: routePath, titleKey, lazyLoader });
  }
  return routes;
}

/**
 * Reads the canonical, routable graph only. `adminVisualRoutes` deliberately includes legacy
 * visual aliases, so it is not the 35-route source of truth for gates that protect App routes.
 */
export function deriveCanonicalRoutes(root) {
  const sourceFile = path.join(root, 'src', 'app', 'routeDefinitions.ts');
  const ast = parser.parse(fs.readFileSync(sourceFile, 'utf8'), {
    sourceType: 'module',
    plugins: ['typescript', 'dynamicImport'],
  });
  const declarations = new Map();
  traverse(ast, {
    VariableDeclarator(pathRef) {
      const { node } = pathRef;
      if (node.id.type === 'Identifier' && ['adminRoutes', 'portalRoutes'].includes(node.id.name)) {
        declarations.set(node.id.name, node);
      }
    },
  });

  const admin = routeArrayFromDeclaration(declarations.get('adminRoutes'), 'adminRoutes', sourceFile);
  const portal = routeArrayFromDeclaration(declarations.get('portalRoutes'), 'portalRoutes', sourceFile);
  const all = [...admin, ...portal];
  const duplicatePaths = [...new Set(all.map((route) => route.path).filter((route, index, paths) => paths.indexOf(route) !== index))];
  if (duplicatePaths.length) {
    throw new Error(`${path.basename(sourceFile)}: duplicate canonical route path(s): ${duplicatePaths.join(', ')}`);
  }
  return { sourceFile, admin, portal, all };
}

function coveredPaths(entries, role, problems) {
  if (!Array.isArray(entries)) {
    problems.push(`${role} coverage manifest is not an array`);
    return [];
  }
  const paths = [];
  for (const entry of entries) {
    const route = typeof entry === 'string' ? entry : Array.isArray(entry) ? entry[1] : entry?.path;
    if (typeof route !== 'string' || !route.startsWith('/')) {
      problems.push(`${role} coverage manifest has no route path for ${JSON.stringify(entry)}`);
      continue;
    }
    paths.push(route);
  }
  const duplicates = [...new Set(paths.filter((route, index) => paths.indexOf(route) !== index))];
  if (duplicates.length) problems.push(`${role} coverage manifest repeats: ${duplicates.join(', ')}`);
  return paths;
}

/**
 * The manifest is intentionally independent from routeDefinitions.  That makes a new route fail
 * every gate until its concrete coverage case is added, rather than silently inheriting a generic
 * pass from the AST-derived list.
 */
export function assertCanonicalRouteCoverage({ root, gate, admin, portal }) {
  const canonical = deriveCanonicalRoutes(root);
  const problems = [];
  const manifest = {
    admin: coveredPaths(admin, 'admin', problems),
    portal: coveredPaths(portal, 'portal', problems),
  };

  for (const role of ['admin', 'portal']) {
    const declared = canonical[role].map((route) => route.path);
    const declaredSet = new Set(declared);
    const coveredSet = new Set(manifest[role]);
    const missing = declared.filter((route) => !coveredSet.has(route));
    const extra = manifest[role].filter((route) => !declaredSet.has(route));
    if (missing.length) problems.push(`${role} routes declared but not covered: ${missing.join(', ')}`);
    if (extra.length) problems.push(`${role} routes covered but no longer declared: ${[...new Set(extra)].join(', ')}`);
  }

  const covered = new Set([...manifest.admin, ...manifest.portal]);
  if (canonical.admin.length !== EXPECTED_ROUTE_COUNTS.admin || canonical.portal.length !== EXPECTED_ROUTE_COUNTS.portal || canonical.all.length !== EXPECTED_ROUTE_COUNTS.total) {
    problems.push(
      `routeDefinitions.ts has ${canonical.all.length}/${EXPECTED_ROUTE_COUNTS.total} canonical routes `
      + `(admin ${canonical.admin.length}/${EXPECTED_ROUTE_COUNTS.admin}, portal ${canonical.portal.length}/${EXPECTED_ROUTE_COUNTS.portal}): `
      + canonical.all.map((route) => route.path).join(', '),
    );
  }
  if (covered.size !== EXPECTED_ROUTE_COUNTS.total) {
    problems.push(`${gate} manifest covers ${covered.size}/${EXPECTED_ROUTE_COUNTS.total} unique canonical routes`);
  }

  if (problems.length) {
    console.error(`${gate} route coverage failed:`);
    for (const problem of problems) console.error(`  - ${problem}`);
    return { ok: false, canonical, covered };
  }

  console.log(`${gate} route coverage: ${covered.size}/${EXPECTED_ROUTE_COUNTS.total} (${canonical.admin.length} admin + ${canonical.portal.length} portal)`);
  return { ok: true, canonical, covered };
}
