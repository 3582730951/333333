import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const noticeFile = path.join(root, 'src', 'components', 'AppUpdateNotice.jsx');
const networkFile = path.join(root, 'src', 'lib', 'browserNetwork.js');
const noticeSource = fs.readFileSync(noticeFile, 'utf8');
const networkSource = fs.readFileSync(networkFile, 'utf8');

const noticeAst = parser.parse(noticeSource, {
  sourceType: 'module',
  plugins: ['jsx', 'importMeta'],
  errorRecovery: true,
});

const networkAst = parser.parse(networkSource, {
  sourceType: 'module',
  plugins: ['jsx', 'importMeta'],
  errorRecovery: true,
});

let fetchLatestSignatureThrowsOnBadStatus = false;
let fetchLatestSignatureUsesFetchText = false;
let fetchTextThrowsOnBadStatus = false;
let updateNoticeListensForOnline = false;

function isBadStatusCheck(node, responseName) {
  return (
    node?.type === 'UnaryExpression' &&
    node.operator === '!' &&
    node.argument?.type === 'MemberExpression' &&
    node.argument.object?.name === responseName &&
    node.argument.property?.name === 'ok'
  );
}

function pathContainsThrow(pathRef) {
  let found = false;
  pathRef.traverse({
    ThrowStatement(pathRef) {
      found = true;
      pathRef.stop();
    },
  });
  return found;
}

traverse(noticeAst, {
  CallExpression(pathRef) {
    const args = pathRef.node.arguments || [];
    if (
      pathRef.node.callee?.name === 'addWindowListener'
      && args[0]?.type === 'StringLiteral'
      && args[0].value === 'online'
    ) {
      updateNoticeListensForOnline = true;
    }
  },
  FunctionDeclaration(pathRef) {
    if (pathRef.node.id?.name !== 'fetchLatestSignature') return;
    pathRef.traverse({
      CallExpression(callPath) {
        if (callPath.node.callee?.name === 'fetchText') {
          fetchLatestSignatureUsesFetchText = true;
        }
      },
      IfStatement(ifPath) {
        if (isBadStatusCheck(ifPath.node.test, 'res') && pathContainsThrow(ifPath.get('consequent'))) {
          fetchLatestSignatureThrowsOnBadStatus = true;
          ifPath.stop();
        }
      },
    });
  },
});

traverse(networkAst, {
  FunctionDeclaration(pathRef) {
    if (pathRef.node.id?.name !== 'fetchText') return;
    pathRef.traverse({
      IfStatement(ifPath) {
        if (isBadStatusCheck(ifPath.node.test, 'response') && pathContainsThrow(ifPath.get('consequent'))) {
          fetchTextThrowsOnBadStatus = true;
          ifPath.stop();
        }
      },
    });
  },
});

if (!fetchLatestSignatureThrowsOnBadStatus && !(fetchLatestSignatureUsesFetchText && fetchTextThrowsOnBadStatus)) {
  console.error('Update polling must throw on non-2xx /console/ responses so failures are reported.');
  process.exit(1);
}

if (!updateNoticeListensForOnline) {
  console.error('Update polling must re-check when the browser comes back online.');
  process.exit(1);
}

console.log('Update polling failure handling check passed.');
