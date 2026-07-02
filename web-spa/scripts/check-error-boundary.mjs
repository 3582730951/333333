import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const boundaryFile = path.join(root, 'src', 'components', 'AppErrorBoundary.jsx');
const mainFile = path.join(root, 'src', 'main.jsx');
const boundarySource = fs.readFileSync(boundaryFile, 'utf8');
const mainSource = fs.readFileSync(mainFile, 'utf8');

const ast = parser.parse(boundarySource, {
  sourceType: 'module',
  plugins: ['jsx', 'importMeta'],
  errorRecovery: true,
});

let windowErrorListenerUsesCapture = false;
let windowErrorListenerReportsResources = false;
let windowErrorListenerDropsMissingEventError = false;
let reportPayloadHasResourceURL = false;
let installReplacesGlobalHandlers = false;
let exportsGlobalHandlerUninstall = false;

function stringLiteralValue(node) {
  if (node?.type === 'StringLiteral') return node.value;
  if (node?.type === 'Literal') return node.value;
  return undefined;
}

function isTrueCaptureArg(node) {
  if (node?.type === 'BooleanLiteral') return node.value === true;
  if (node?.type === 'Literal') return node.value === true;
  if (node?.type !== 'ObjectExpression') return false;
  return node.properties.some((prop) => (
    prop.type === 'ObjectProperty' &&
    prop.key?.name === 'capture' &&
    prop.value?.type === 'BooleanLiteral' &&
    prop.value.value === true
  ));
}

function isWindowErrorAddEventListener(node) {
  return (
    node?.callee?.type === 'MemberExpression' &&
    node.callee.object?.name === 'window' &&
    node.callee.property?.name === 'addEventListener' &&
    stringLiteralValue(node.arguments?.[0]) === 'error'
  );
}

function isWindowErrorListenHelper(node) {
  return (
    node?.callee?.type === 'Identifier' &&
    node.callee.name === 'listen' &&
    node.arguments?.[0]?.name === 'window' &&
    stringLiteralValue(node.arguments?.[1]) === 'error'
  );
}

function isWindowErrorListenerRegistration(node) {
  return isWindowErrorAddEventListener(node) || isWindowErrorListenHelper(node);
}

function captureArgument(node) {
  return isWindowErrorListenHelper(node) ? node.arguments?.[3] : node.arguments?.[2];
}

function listenerArgumentPath(pathRef) {
  return isWindowErrorListenHelper(pathRef.node) ? pathRef.get('arguments.2') : pathRef.get('arguments.1');
}

function listenerBodyPath(pathRef) {
  const argPath = listenerArgumentPath(pathRef);
  if (!argPath?.node) return pathRef;
  if (argPath.isFunctionExpression?.() || argPath.isArrowFunctionExpression?.()) return argPath;
  if (!argPath.isIdentifier?.()) return pathRef;

  const binding = pathRef.scope.getBinding(argPath.node.name);
  const bindingPath = binding?.path;
  if (!bindingPath) return pathRef;
  if (bindingPath.isFunctionDeclaration?.()) return bindingPath;
  if (bindingPath.isVariableDeclarator?.()) {
    const initPath = bindingPath.get('init');
    if (initPath?.isFunctionExpression?.() || initPath?.isArrowFunctionExpression?.()) {
      return initPath;
    }
  }
  return pathRef;
}

function isNegatedEventError(node) {
  return (
    node?.type === 'UnaryExpression' &&
    node.operator === '!' &&
    node.argument?.type === 'MemberExpression' &&
    node.argument.object?.name === 'event' &&
    node.argument.property?.name === 'error'
  );
}

function containsReturn(pathRef) {
  let found = false;
  pathRef.traverse({
    ReturnStatement(returnPath) {
      found = true;
      returnPath.stop();
    },
  });
  return found;
}

function listenerCalls(pathRef, name) {
  let found = false;
  pathRef.traverse({
    CallExpression(callPath) {
      if (callPath.node.callee?.name === name) {
        found = true;
        callPath.stop();
      }
    },
  });
  return found;
}

traverse(ast, {
  ExportNamedDeclaration(pathRef) {
    const declaration = pathRef.node.declaration;
    if (declaration?.type === 'FunctionDeclaration' && declaration.id?.name === 'uninstallGlobalErrorHandlers') {
      exportsGlobalHandlerUninstall = true;
    }
  },
  FunctionDeclaration(pathRef) {
    if (pathRef.node.id?.name !== 'installGlobalErrorHandlers') return;
    pathRef.traverse({
      CallExpression(callPath) {
        if (callPath.node.callee?.name === 'uninstallGlobalErrorHandlers') {
          installReplacesGlobalHandlers = true;
          callPath.stop();
        }
      },
    });
  },
  CallExpression(pathRef) {
    const node = pathRef.node;
    if (isWindowErrorListenerRegistration(node)) {
      const bodyPath = listenerBodyPath(pathRef);
      windowErrorListenerUsesCapture = isTrueCaptureArg(captureArgument(node));
      windowErrorListenerReportsResources =
        listenerCalls(bodyPath, 'resourceURLFromErrorEvent') &&
        listenerCalls(bodyPath, 'errorFromWindowEvent') &&
        listenerCalls(bodyPath, 'reportClientError');
      bodyPath.traverse({
        IfStatement(ifPath) {
          if (isNegatedEventError(ifPath.node.test) && containsReturn(ifPath.get('consequent'))) {
            windowErrorListenerDropsMissingEventError = true;
            ifPath.stop();
          }
        },
      });
    }
  },
  ObjectProperty(pathRef) {
    if (pathRef.node.key?.name === 'resource_url') {
      reportPayloadHasResourceURL = true;
    }
  },
});

const failures = [];
if (!windowErrorListenerUsesCapture) {
  failures.push('window error listener must use capture=true so script/link load errors are observed.');
}
if (!windowErrorListenerReportsResources) {
  failures.push('window error listener must normalize resource load events and report them.');
}
if (windowErrorListenerDropsMissingEventError) {
  failures.push('window error listener must not return early when event.error is missing.');
}
if (!reportPayloadHasResourceURL) {
  failures.push('client error payload must include resource_url for failed assets.');
}
if (!exportsGlobalHandlerUninstall || !boundarySource.includes('globalHandlersDisposerKey')) {
  failures.push('global error handlers must expose a disposer key and uninstall function.');
}
if (!installReplacesGlobalHandlers || !boundarySource.includes('removeEventListener')) {
  failures.push('installGlobalErrorHandlers must replace old global listeners and remove them on dispose.');
}
if (!mainSource.includes('disposeGlobalErrorHandlers') || !mainSource.includes('import.meta.hot.dispose')) {
  failures.push('main.jsx must dispose global error handlers during Vite HMR.');
}

if (failures.length > 0) {
  console.error('Error boundary check failed:');
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log('Error boundary resource and lifecycle checks passed.');
