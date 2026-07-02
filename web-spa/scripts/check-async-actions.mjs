import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const actionFiles = [
  {
    file: 'src/pages/Egress.jsx',
    bannedStateNames: ['saving', 'setSaving'],
  },
  {
    file: 'src/pages/Providers.jsx',
    bannedStateNames: ['saving', 'setSaving'],
  },
  {
    file: 'src/pages/Users.jsx',
    bannedStateNames: ['saving', 'setSaving'],
  },
  {
    file: 'src/pages/Groups.jsx',
    bannedStateNames: [],
  },
  {
    file: 'src/pages/Keys.jsx',
    bannedStateNames: [],
  },
  {
    file: 'src/pages/Login.jsx',
    bannedStateNames: ['loading', 'setLoading'],
  },
  {
    file: 'src/components/OAuthLoginModal.jsx',
    bannedStateNames: ['generating', 'setGenerating', 'completing', 'setCompleting', 'manualLoading', 'setManualLoading'],
  },
  {
    file: 'src/components/ConfigForm.jsx',
    bannedStateNames: ['saving', 'setSaving'],
  },
  {
    file: 'src/components/ApiKeyCreateModal.jsx',
    bannedStateNames: ['submitting', 'setSubmitting'],
  },
  {
    file: 'src/pages/portal/PortalProfile.jsx',
    bannedStateNames: ['saving', 'setSaving'],
  },
  {
    file: 'src/pages/portal/PortalKeys.jsx',
    bannedStateNames: [],
  },
  {
    file: 'src/pages/Lifecycle.jsx',
    bannedStateNames: ['creating', 'setCreating'],
  },
  {
    file: 'src/pages/SettingsV2.jsx',
    bannedStateNames: ['saving', 'setSaving'],
  },
  {
    file: 'src/pages/Registration.jsx',
    bannedStateNames: ['starting', 'setStarting'],
  },
  {
    file: 'src/pages/Accounts.jsx',
    bannedStateNames: [],
  },
].map((entry) => ({ ...entry, fullPath: path.join(root, entry.file) }));

function parseFile(file) {
  return parser.parse(fs.readFileSync(file, 'utf8'), {
    sourceType: 'module',
    plugins: ['jsx', 'importMeta', 'dynamicImport'],
    errorRecovery: true,
  });
}

function addProblem(problems, file, node, message) {
  const loc = node.loc?.start || { line: 1, column: 0 };
  problems.push(`${path.relative(root, file)}:${loc.line}:${loc.column + 1} ${message}`);
}

function checkFile({ fullPath: file, bannedStateNames }) {
  const ast = parseFile(file);
  const problems = [];
  const bannedStateNameSet = new Set(bannedStateNames);
  let importsUseAsyncAction = false;
  let callsUseAsyncAction = false;

  traverse(ast, {
    ImportDeclaration(pathRef) {
      if (
        pathRef.node.source.value === '../hooks/useAsyncAction.js'
        || pathRef.node.source.value === '../../hooks/useAsyncAction.js'
        || pathRef.node.source.value === '../hooks/useKeyedAsyncAction.js'
        || pathRef.node.source.value === '../../hooks/useKeyedAsyncAction.js'
      ) {
        importsUseAsyncAction = true;
      }
    },
    CallExpression(pathRef) {
      if (pathRef.node.callee?.name === 'useAsyncAction' || pathRef.node.callee?.name === 'useKeyedAsyncAction') {
        callsUseAsyncAction = true;
      }
      if (pathRef.node.callee?.name !== 'useState') return;
      const parent = pathRef.parentPath?.node;
      if (parent?.type !== 'VariableDeclarator' || parent.id?.type !== 'ArrayPattern') return;
      const names = parent.id.elements
        .filter(Boolean)
        .map((element) => element.name)
        .filter(Boolean);
      if (names.some((name) => bannedStateNameSet.has(name))) {
        addProblem(problems, file, pathRef.node, 'use useAsyncAction for operation running state instead of hand-written saving state');
      }
    },
  });

  if (!importsUseAsyncAction || !callsUseAsyncAction) {
    problems.push(`${path.relative(root, file)}:1:1 operation-heavy page must use useAsyncAction for submit/dedup state`);
  }
  return problems;
}

const problems = actionFiles.flatMap(checkFile);

if (problems.length > 0) {
  console.error('Async action boundary check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Async action boundary check passed.');
