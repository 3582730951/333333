import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const MAX_STATIC_VARIANTS = 64;

export const OWNED_PREFIXES = ['pool-', 'upstream-rule', 'upstream-rules', 'public-chat'];
export const ownsClass = (name) => OWNED_PREFIXES.some((prefix) => name.startsWith(prefix));

function unwrapExpression(node) {
  let current = node;
  while (current && [
    'TSAsExpression',
    'TSSatisfiesExpression',
    'TypeCastExpression',
    'TSInstantiationExpression',
    'ParenthesizedExpression',
  ].includes(current.type)) current = current.expression;
  return current;
}

function stringValues(node) {
  const expression = unwrapExpression(node);
  if (!expression) return null;
  if (expression.type === 'StringLiteral') return [expression.value];
  if (expression.type === 'TemplateLiteral') {
    let values = [expression.quasis[0]?.value.cooked ?? expression.quasis[0]?.value.raw ?? ''];
    for (let index = 0; index < expression.expressions.length; index += 1) {
      const next = stringValues(expression.expressions[index]);
      if (!next) return null;
      const suffix = expression.quasis[index + 1]?.value.cooked ?? expression.quasis[index + 1]?.value.raw ?? '';
      const combined = [];
      for (const before of values) {
        for (const value of next) {
          if (combined.length >= MAX_STATIC_VARIANTS) return null;
          combined.push(`${before}${value}${suffix}`);
        }
      }
      values = combined;
    }
    return values;
  }
  if (expression.type === 'ConditionalExpression') {
    const consequent = stringValues(expression.consequent);
    const alternate = stringValues(expression.alternate);
    if (!consequent || !alternate) return null;
    return [...new Set([...consequent, ...alternate])].slice(0, MAX_STATIC_VARIANTS);
  }
  if (expression.type === 'LogicalExpression') {
    if (expression.operator === '&&') {
      const right = stringValues(expression.right);
      return right ? [...new Set(['', ...right])].slice(0, MAX_STATIC_VARIANTS) : null;
    }
    const left = stringValues(expression.left);
    const right = stringValues(expression.right);
    if (!left || !right) return null;
    return [...new Set([...left, ...right])].slice(0, MAX_STATIC_VARIANTS);
  }
  return null;
}

function classCombiner(callee) {
  const names = new Set(['clsx', 'classnames', 'classNames', 'cx', 'cn']);
  if (callee?.type === 'Identifier') return names.has(callee.name);
  if (callee?.type === 'MemberExpression' && !callee.computed && callee.property.type === 'Identifier') {
    return names.has(callee.property.name);
  }
  return false;
}

function arrayAssemblyCall(node) {
  const property = node?.callee?.type === 'MemberExpression' && !node.callee.computed
    ? node.callee.property
    : null;
  return property?.type === 'Identifier' && ['join', 'filter', 'flat', 'flatMap', 'concat'].includes(property.name);
}

function expressionText(source, node) {
  return source.slice(node.start ?? 0, node.end ?? 0).replace(/\s+/g, ' ').trim();
}

/**
 * Extracts owned class names that are provably emitted by JSX `className` attributes. Dynamic
 * expressions are not guessed: they are returned as `indeterminate`, with a source location for
 * the report. This preserves literal mining inside template holes while making `pool-${state}`
 * visible as a bounded static-analysis blind spot.
 */
export function collectOwnedClassesFromJsx(source, { file = '<source>' } = {}) {
  const ast = parser.parse(source, {
    sourceType: 'module',
    plugins: ['jsx', 'typescript', 'importMeta', 'dynamicImport'],
  });
  const classes = new Set();
  const indeterminate = [];
  const reported = new Set();

  const addClassList = (value) => {
    for (const name of String(value || '').split(/\s+/).filter(Boolean)) {
      if (ownsClass(name)) classes.add(name);
    }
  };
  const reportIndeterminate = (node, reason) => {
    const text = expressionText(source, node);
    const line = node.loc?.start.line || 0;
    const column = (node.loc?.start.column || 0) + 1;
    const key = `${line}:${column}:${text}:${reason}`;
    if (reported.has(key)) return;
    reported.add(key);
    indeterminate.push({ file, line, column, expression: text, reason });
  };

  const collectObject = (node, reportUnknown = true) => {
    for (const property of node.properties || []) {
      if (property.type === 'ObjectProperty') {
        const key = property.key.type === 'Identifier'
          ? property.key.name
          : property.key.type === 'StringLiteral'
            ? property.key.value
            : '';
        addClassList(key);
      } else if (property.type === 'SpreadElement' && reportUnknown) {
        reportIndeterminate(property.argument, 'spread class map cannot be statically determined');
      }
    }
  };

  const collectExpression = (candidate, { reportUnknown = true, resolveIdentifier, seenBindings = new Set() } = {}) => {
    const node = unwrapExpression(candidate);
    if (!node || node.type === 'JSXEmptyExpression') return;
    const exact = stringValues(node);
    if (exact) {
      for (const value of exact) addClassList(value);
      return;
    }

    switch (node.type) {
      case 'TemplateLiteral': {
        // Keep complete static tokens surrounding holes, but never turn `pool-` in
        // `pool-${state}` into a fictional class name. The whole template is reported once.
        for (let index = 0; index < node.quasis.length; index += 1) {
          const raw = node.quasis[index].value.cooked ?? node.quasis[index].value.raw ?? '';
          for (const token of raw.matchAll(/\S+/g)) {
            const start = token.index || 0;
            const end = start + token[0].length;
            const touchesPreviousHole = index > 0 && start === 0;
            const touchesNextHole = index < node.expressions.length && end === raw.length;
            if (!touchesPreviousHole && !touchesNextHole) addClassList(token[0]);
          }
        }
        if (reportUnknown) reportIndeterminate(node, 'template interpolation cannot be statically determined');
        for (const expression of node.expressions) collectExpression(expression, { reportUnknown: false, resolveIdentifier, seenBindings });
        return;
      }
      case 'ConditionalExpression':
        collectExpression(node.consequent, { reportUnknown, resolveIdentifier, seenBindings });
        collectExpression(node.alternate, { reportUnknown, resolveIdentifier, seenBindings });
        return;
      case 'LogicalExpression':
        if (node.operator === '&&') collectExpression(node.right, { reportUnknown, resolveIdentifier, seenBindings });
        else {
          collectExpression(node.left, { reportUnknown, resolveIdentifier, seenBindings });
          collectExpression(node.right, { reportUnknown, resolveIdentifier, seenBindings });
        }
        return;
      case 'ArrayExpression':
        for (const element of node.elements) {
          if (!element) continue;
          if (element.type === 'SpreadElement') {
            if (reportUnknown) reportIndeterminate(element.argument, 'spread class array cannot be statically determined');
          } else collectExpression(element, { reportUnknown, resolveIdentifier, seenBindings });
        }
        return;
      case 'CallExpression':
        if (classCombiner(node.callee)) {
          for (const argument of node.arguments) {
            if (argument.type === 'SpreadElement') {
              if (reportUnknown) reportIndeterminate(argument.argument, 'spread clsx argument cannot be statically determined');
            } else if (unwrapExpression(argument)?.type === 'ObjectExpression') {
              collectObject(unwrapExpression(argument), reportUnknown);
            } else collectExpression(argument, { reportUnknown, resolveIdentifier, seenBindings });
          }
          return;
        }
        if (arrayAssemblyCall(node)) {
          collectExpression(node.callee.object, { reportUnknown, resolveIdentifier, seenBindings });
          return;
        }
        if (reportUnknown) reportIndeterminate(node, 'class-producing call cannot be statically determined');
        return;
      case 'Identifier': {
        const binding = resolveIdentifier?.(node.name);
        if (binding?.init && !seenBindings.has(binding.binding)) {
          const nextSeenBindings = new Set(seenBindings);
          nextSeenBindings.add(binding.binding);
          collectExpression(binding.init, { reportUnknown, resolveIdentifier, seenBindings: nextSeenBindings });
        } else if (reportUnknown) reportIndeterminate(node, 'Identifier cannot be statically determined');
        return;
      }
      case 'ObjectExpression':
        if (reportUnknown) reportIndeterminate(node, 'object-valued className cannot be statically determined');
        return;
      default:
        if (reportUnknown) reportIndeterminate(node, `${node.type} cannot be statically determined`);
    }
  };

  traverse(ast, {
    JSXAttribute(pathRef) {
      const { node } = pathRef;
      if (node.name.type !== 'JSXIdentifier' || node.name.name !== 'className') return;
      const resolveIdentifier = (name) => {
        const binding = pathRef.scope.getBinding(name);
        if (!binding?.constant || !binding.path.isVariableDeclarator()) return null;
        return { binding, init: binding.path.node.init };
      };
      if (node.value?.type === 'StringLiteral') addClassList(node.value.value);
      else if (node.value?.type === 'JSXExpressionContainer') collectExpression(node.value.expression, { resolveIdentifier });
      else if (node.value && node.value.type !== 'JSXEmptyExpression') reportIndeterminate(node.value, 'unsupported JSX className value');
    },
  });

  return {
    classes: [...classes].sort(),
    indeterminate: indeterminate.sort((a, b) => a.file.localeCompare(b.file) || a.line - b.line || a.column - b.column),
  };
}
