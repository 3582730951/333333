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
    file: 'src/pages/Users.tsx',
    bannedStateNames: ['saving', 'setSaving'],
  },
  {
    file: 'src/pages/Groups.jsx',
    bannedStateNames: [],
  },
  {
    file: 'src/pages/Keys.tsx',
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
    file: 'src/components/ConfigForm.tsx',
    bannedStateNames: ['saving', 'setSaving'],
  },
  {
    file: 'src/components/ApiKeyCreateModal.tsx',
    bannedStateNames: ['submitting', 'setSubmitting'],
  },
  {
    file: 'src/pages/portal/PortalProfile.jsx',
    bannedStateNames: ['saving', 'setSaving'],
  },
  {
    file: 'src/pages/portal/PortalKeys.tsx',
    bannedStateNames: [],
  },
  {
    file: 'src/pages/SettingsV2.tsx',
    bannedStateNames: ['saving', 'setSaving'],
  },
  {
    file: 'src/pages/Registration.tsx',
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
    plugins: ['jsx', 'typescript', 'importMeta', 'dynamicImport'],
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
  let importsUseQueryMutation = false;
  let callsUseQueryMutation = false;

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
      if (/features\/[^/]+\/queries\//.test(String(pathRef.node.source.value))) {
        importsUseQueryMutation = pathRef.node.specifiers.some((specifier) => (
          specifier.imported?.name && /^use[A-Z].*Mutation$/.test(specifier.imported.name)
        )) || importsUseQueryMutation;
      }
    },
    CallExpression(pathRef) {
      if (pathRef.node.callee?.name === 'useAsyncAction' || pathRef.node.callee?.name === 'useKeyedAsyncAction') {
        callsUseAsyncAction = true;
      }
      if (pathRef.node.callee?.name && /^use[A-Z].*Mutation$/.test(pathRef.node.callee.name)) {
        callsUseQueryMutation = true;
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

  const usesGuardedMutation = (importsUseAsyncAction && callsUseAsyncAction)
    || (importsUseQueryMutation && callsUseQueryMutation);
  if (!usesGuardedMutation) {
    problems.push(`${path.relative(root, file)}:1:1 operation-heavy page must use useAsyncAction or a domain mutation for submit/dedup state`);
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
