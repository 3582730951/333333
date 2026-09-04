// Offline GLSL ES 3.0 gate for the progressive engine.
//
// Shader sources may be standalone .glsl files or raw template literals in shader.js/shader.ts.
// A template is treated as GLSL only when its raw contents begin with the GLSL ES 3.0 version
// directive. Extracted templates retain their position in the module, so diagnostics still name
// the shader.js/shader.ts line that contains the defect. A stage is inferred from a .frag/.vert/
// .fs/.vs filename, an @stage/pragma marker, a VERTEX_/FRAGMENT_ variable name, or fragment
// built-ins. `--root` and GLSL_ROOT are test-only escape hatches; the package check always uses
// src/engine.
//
// If glslangValidator, glslc, or shaderc is installed, it is used for the full offline parser.
// The built-in structural parser still runs in every environment so the gate can enforce the
// three P4 hazards without a GPU. A missing external compiler is reported as a skip, never as a
// silent or falsely labelled compiler pass.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const defaultShaderRoot = path.join(repoRoot, 'src', 'engine');
const shaderModuleNames = new Set(['shader.js', 'shader.ts']);

function optionValue(name) {
  const index = process.argv.indexOf(name);
  if (index >= 0) return process.argv[index + 1] || '';
  const prefix = `${name}=`;
  const inline = process.argv.find((argument) => argument.startsWith(prefix));
  return inline ? inline.slice(prefix.length) : '';
}

const shaderRoot = path.resolve(optionValue('--root') || process.env.GLSL_ROOT || defaultShaderRoot);
const configuredLimit = process.env.GLSL_MAX_TEXTURE_IMAGE_UNITS
  || process.env.GL_MAX_TEXTURE_IMAGE_UNITS
  || '8';
const maxTextureImageUnits = Number(configuredLimit);

if (!Number.isInteger(maxTextureImageUnits) || maxTextureImageUnits < 1) {
  console.error(`GLSL check failed: GLSL_MAX_TEXTURE_IMAGE_UNITS must be a positive integer (got ${configuredLimit})`);
  process.exit(1);
}

function walk(directory) {
  if (!fs.existsSync(directory)) return [];
  const files = [];
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const fullPath = path.join(directory, entry.name);
    if (entry.isDirectory()) files.push(...walk(fullPath));
    else if (entry.isFile() && (
      entry.name.toLowerCase().endsWith('.glsl') || shaderModuleNames.has(entry.name)
    )) files.push(fullPath);
  }
  return files.sort((left, right) => left.localeCompare(right));
}

function blankSourcePreservingLines(source) {
  return source.replace(/[^\r\n]/g, ' ');
}

function findQuotedStringEnd(source, openIndex, quote) {
  for (let index = openIndex + 1; index < source.length; index += 1) {
    if (source[index] === '\\') {
      index += 1;
      continue;
    }
    if (source[index] === quote) return index;
  }
  return -1;
}

function findTemplateEnd(source, openIndex) {
  for (let index = openIndex + 1; index < source.length; index += 1) {
    if (source[index] === '\\') {
      index += 1;
      continue;
    }
    if (source[index] === '`') return index;
  }
  return -1;
}

function startsWithGlslVersion(source) {
  return /^#\s*version\s+300\s+es(?:[\t\r\n ]|$)/.test(source);
}

function templateStageHint(moduleSource, openIndex) {
  const before = moduleSource.slice(Math.max(0, openIndex - 240), openIndex);
  const declaration = before.match(/(?:const|let|var)\s+([A-Za-z_]\w*)\s*(?::[^=]+)?=\s*$/);
  if (!declaration) return '';
  if (/fragment/i.test(declaration[1])) return 'frag';
  if (/vertex/i.test(declaration[1])) return 'vert';
  return '';
}

// Scan JavaScript/TypeScript lexically enough to ignore comments and quoted strings before
// looking at template literals. The P3 shader contract uses raw templates without interpolation;
// escaped backticks are still skipped so a literal can contain one without ending early.
function extractGlslTemplates(moduleSource) {
  const templates = [];
  const blankModuleSource = blankSourcePreservingLines(moduleSource);
  for (let index = 0; index < moduleSource.length; index += 1) {
    const current = moduleSource[index];
    const next = moduleSource[index + 1];
    if (current === '/' && next === '/') {
      const newline = moduleSource.indexOf('\n', index + 2);
      index = newline < 0 ? moduleSource.length : newline;
      continue;
    }
    if (current === '/' && next === '*') {
      const end = moduleSource.indexOf('*/', index + 2);
      index = end < 0 ? moduleSource.length : end + 1;
      continue;
    }
    if (current === '"' || current === "'") {
      const end = findQuotedStringEnd(moduleSource, index, current);
      index = end < 0 ? moduleSource.length : end;
      continue;
    }
    if (current !== '`') continue;

    const end = findTemplateEnd(moduleSource, index);
    if (end < 0) break;
    const rawTemplate = moduleSource.slice(index + 1, end);
    if (startsWithGlslVersion(rawTemplate)) {
      const virtualSource = `${blankModuleSource.slice(0, index + 1)}${rawTemplate}`
        + blankModuleSource.slice(end);
      templates.push({
        kind: 'template',
        source: moduleSource,
        compilerSource: virtualSource,
        templateOffset: index + 1,
        stageHint: templateStageHint(moduleSource, index),
      });
    }
    index = end;
  }
  return templates;
}

function collectShaderSources(directory) {
  const shaders = [];
  for (const file of walk(directory)) {
    if (file.toLowerCase().endsWith('.glsl')) {
      const source = fs.readFileSync(file, 'utf8');
      shaders.push({ kind: 'file', file, source, compilerSource: source, templateOffset: -1 });
      continue;
    }
    const moduleSource = fs.readFileSync(file, 'utf8');
    shaders.push(...extractGlslTemplates(moduleSource).map((shader) => ({ ...shader, file })));
  }
  return shaders.sort((left, right) => (
    left.file.localeCompare(right.file) || left.templateOffset - right.templateOffset
  ));
}

// Keep newlines and offsets while blanking comments/quoted preprocessor strings. This is enough
// for the structural checks below and keeps every diagnostic tied to the original line.
function maskCommentsAndStrings(source) {
  const chars = source.split('');
  let state = 'code';
  for (let index = 0; index < chars.length; index += 1) {
    const current = chars[index];
    const next = chars[index + 1];
    if (state === 'code') {
      if (current === '/' && next === '/') {
        chars[index] = ' ';
        chars[index + 1] = ' ';
        index += 1;
        state = 'line-comment';
      } else if (current === '/' && next === '*') {
        chars[index] = ' ';
        chars[index + 1] = ' ';
        index += 1;
        state = 'block-comment';
      } else if (current === '"' || current === "'") {
        chars[index] = ' ';
        state = 'string';
      }
    } else if (state === 'line-comment') {
      if (current === '\n') state = 'code';
      else chars[index] = ' ';
    } else if (state === 'block-comment') {
      if (current === '*' && next === '/') {
        chars[index] = ' ';
        chars[index + 1] = ' ';
        index += 1;
        state = 'code';
      } else if (current !== '\n') {
        chars[index] = ' ';
      }
    } else if (state === 'string') {
      if (current === '\\') {
        chars[index] = ' ';
        if (index + 1 < chars.length && chars[index + 1] !== '\n') {
          chars[index + 1] = ' ';
          index += 1;
        }
      } else if (current === '"' || current === "'") {
        chars[index] = ' ';
        state = 'code';
      } else if (current !== '\n') {
        chars[index] = ' ';
      }
    }
  }
  return chars.join('');
}

function lineAt(source, offset) {
  return source.slice(0, Math.max(0, offset)).split('\n').length;
}

function issue(source, offset, message) {
  return { line: lineAt(source, offset), message };
}

function findMatching(source, openIndex, open = '(', close = ')') {
  let depth = 0;
  for (let index = openIndex; index < source.length; index += 1) {
    if (source[index] === open) depth += 1;
    else if (source[index] === close) {
      depth -= 1;
      if (depth === 0) return index;
    }
  }
  return -1;
}

function splitTopLevel(source, delimiter) {
  const parts = [];
  let start = 0;
  let parentheses = 0;
  let brackets = 0;
  for (let index = 0; index < source.length; index += 1) {
    if (source[index] === '(') parentheses += 1;
    else if (source[index] === ')') parentheses -= 1;
    else if (source[index] === '[') brackets += 1;
    else if (source[index] === ']') brackets -= 1;
    else if (source[index] === delimiter && parentheses === 0 && brackets === 0) {
      parts.push(source.slice(start, index));
      start = index + 1;
    }
  }
  parts.push(source.slice(start));
  return parts;
}

function inferStage(file, source, masked, stageHint = '') {
  const basename = path.basename(file).toLowerCase();
  const marker = source.match(/@stage\s*[:=]\s*(vertex|vert|fragment|frag)\b/i)
    || source.match(/#\s*pragma\s+(?:shader_)?stage\s+(vertex|vert|fragment|frag)\b/i);
  if (marker) return marker[1].startsWith('frag') ? 'frag' : 'vert';
  if (stageHint) return stageHint;
  if (/(?:^|[._-])(?:frag|fragment|fs|pixel)(?:[._-]|$)/i.test(basename)) return 'frag';
  if (/(?:^|[._-])(?:vert|vertex|vs)(?:[._-]|$)/i.test(basename)) return 'vert';
  if (/\bgl_Position\b/.test(masked)) return 'vert';
  if (/\b(?:gl_FragCoord|gl_FragColor|gl_FragDepth)\b/.test(masked)) return 'frag';
  if (/(?:^|\n)\s*(?:layout\s*\([^)]*\)\s*)?out\s+(?:(?:lowp|mediump|highp)\s+)?vec4\b/.test(masked)) return 'frag';
  return '';
}

function collectConstants(masked) {
  const names = new Set(['true', 'false', 'gl_MaxTextureImageUnits']);
  const values = new Map();
  for (const match of masked.matchAll(/^\s*#\s*define\s+([A-Za-z_]\w*)\s+([^\n]+)/gm)) {
    names.add(match[1]);
    const number = match[2].trim().match(/^[+-]?\d+$/);
    if (number) values.set(match[1], Number(number[0]));
  }
  for (const match of masked.matchAll(/\bconst\s+(?:bool|int|uint|float)\s+([A-Za-z_]\w*)\s*=\s*([^;]+);/g)) {
    names.add(match[1]);
    const number = match[2].trim().match(/^[+-]?\d+$/);
    if (number) values.set(match[1], Number(number[0]));
  }
  return { names, values };
}

const constantTypeNames = new Set([
  'bool', 'int', 'uint', 'float',
]);

function isConstantExpression(expression, names, allowed = new Set()) {
  const identifiers = expression.match(/\b[A-Za-z_]\w*\b/g) || [];
  return identifiers.every((identifier) => (
    names.has(identifier) || allowed.has(identifier) || constantTypeNames.has(identifier)
  ));
}

function integerConstant(expression, values) {
  const substituted = expression.trim().replace(/\b[A-Za-z_]\w*\b/g, (name) => (
    values.has(name) ? String(values.get(name)) : name
  ));
  // Array sizes in the gate are intentionally restricted to integer arithmetic. Tokenize and
  // evaluate only that grammar; arbitrary shader text is never passed to eval or a JS parser.
  const tokens = [];
  let cursor = 0;
  while (cursor < substituted.length) {
    if (/\s/.test(substituted[cursor])) {
      cursor += 1;
      continue;
    }
    const number = substituted.slice(cursor).match(/^\d+/);
    if (number) {
      tokens.push(Number(number[0]));
      cursor += number[0].length;
      continue;
    }
    if ('()+-*/%'.includes(substituted[cursor])) {
      tokens.push(substituted[cursor]);
      cursor += 1;
      continue;
    }
    return undefined;
  }

  let position = 0;
  const parsePrimary = () => {
    if (tokens[position] === '(') {
      position += 1;
      const value = parseAdditive();
      if (tokens[position] !== ')') throw new Error('unclosed');
      position += 1;
      return value;
    }
    if (typeof tokens[position] === 'number') return tokens[position++];
    throw new Error('primary');
  };
  const parseMultiplicative = () => {
    let value = parsePrimary();
    while (['*', '/', '%'].includes(tokens[position])) {
      const operator = tokens[position++];
      const right = parsePrimary();
      if ((operator === '/' || operator === '%') && right === 0) throw new Error('zero');
      if (operator === '*') value *= right;
      else if (operator === '/') value = Math.trunc(value / right);
      else value %= right;
    }
    return value;
  };
  const parseAdditive = () => {
    let value = parseMultiplicative();
    while (tokens[position] === '+' || tokens[position] === '-') {
      const operator = tokens[position++];
      const right = parseMultiplicative();
      value = operator === '+' ? value + right : value - right;
    }
    return value;
  };
  try {
    const value = parseAdditive();
    if (position !== tokens.length || !Number.isInteger(value)) return undefined;
    return value;
  } catch {
    return undefined;
  }
}

function checkLoops(source, masked, constants) {
  const issues = [];
  for (const match of masked.matchAll(/\b(for|while)\b/g)) {
    const keyword = match[1];
    let open = match.index + keyword.length;
    while (/\s/.test(masked[open] || '')) open += 1;
    if (masked[open] !== '(') {
      issues.push(issue(source, match.index, `${keyword} 缺少条件括号`));
      continue;
    }
    const close = findMatching(masked, open);
    if (close < 0) {
      issues.push(issue(source, match.index, `${keyword} 条件括号未闭合`));
      continue;
    }
    const condition = masked.slice(open + 1, close);
    if (keyword === 'while') {
      if (!isConstantExpression(condition, constants.names)) {
        issues.push(issue(source, match.index, '循环边界必须是编译期常量；while 条件含有非常量标识符'));
      }
      continue;
    }

    const parts = splitTopLevel(condition, ';');
    if (parts.length !== 3) {
      issues.push(issue(source, match.index, 'for 循环必须具有可静态展开的 init、condition、step'));
      continue;
    }
    const init = parts[0].trim();
    const initMatch = init.match(/^(?:const\s+)?(?:int|uint)\s+([A-Za-z_]\w*)\s*=\s*(.+)$/);
    if (!initMatch || !isConstantExpression(initMatch[2], constants.names)) {
      issues.push(issue(source, match.index, '循环初值必须是编译期常量整数'));
      continue;
    }
    const loopVariable = initMatch[1];
    if (!isConstantExpression(parts[1], constants.names, new Set([loopVariable]))) {
      issues.push(issue(source, match.index, '循环边界必须是编译期常量；禁止 uniform/运行时边界'));
    }
    const step = parts[2].trim();
    const stepIsSimple = new RegExp(`^(?:${loopVariable}\\+\\+|\\+\\+${loopVariable}|${loopVariable}--|--${loopVariable})$`).test(step);
    const stepIsCompound = new RegExp(`^${loopVariable}\\s*(?:\\+=|-=)\\s*(.+)$`).exec(step);
    const stepIsAssignment = new RegExp(`^${loopVariable}\\s*=\\s*${loopVariable}\\s*[+-]\\s*(.+)$`).exec(step);
    if (!stepIsSimple && !(stepIsCompound && isConstantExpression(stepIsCompound[1], constants.names))
      && !(stepIsAssignment && isConstantExpression(stepIsAssignment[1], constants.names))) {
      issues.push(issue(source, match.index, '循环步长必须是编译期常量'));
    }
  }
  return issues;
}

const samplerTypePattern = /\b(?:[iu]?sampler(?:2DMSArray|2DMS|2DArray|2D|3D|Cube|Buffer|ExternalOES)(?:Shadow)?)\b/g;

function checkSamplers(source, masked, constants) {
  const issues = [];
  let samplerCount = 0;
  let firstSamplerOffset = 0;
  let sawSampler = false;
  // Leave the terminating semicolon for the next match's leading delimiter. Consuming it here
  // would skip every other declaration (including an immediately following sampler uniform).
  for (const statementMatch of masked.matchAll(/(?:^|[;{}])([^;{}]*)(?=;)/g)) {
    const statement = statementMatch[1];
    const type = samplerTypePattern.exec(statement);
    samplerTypePattern.lastIndex = 0;
    if (!type) continue;
    if (!sawSampler) {
      firstSamplerOffset = statementMatch.index + type.index;
      sawSampler = true;
    }
    const declarators = splitTopLevel(statement.slice(type.index + type[0].length), ',');
    for (const declarator of declarators) {
      const name = declarator.match(/^\s*[A-Za-z_]\w*\s*(?:\[\s*([^\]]*)\s*\])?/);
      if (!name) continue;
      const arrayExpression = name[1];
      if (arrayExpression === undefined) {
        samplerCount += 1;
        continue;
      }
      const size = integerConstant(arrayExpression, constants.values);
      if (size === undefined || size < 1) {
        issues.push(issue(source, statementMatch.index + type.index, `无法确定采样器数组 ${name[0].trim()} 的编译期大小`));
      } else {
        samplerCount += size;
      }
    }
  }
  if (samplerCount > maxTextureImageUnits) {
    issues.push(issue(source, firstSamplerOffset, `采样器声明 ${samplerCount} 个，超过 gl_MaxTextureImageUnits=${maxTextureImageUnits}`));
  }
  return issues;
}

function checkMediumpAndHighp(source, masked, stage) {
  const issues = [];
  const mediump = /\bmediump\s+(?:float|vec[234]|mat[234](?:x[234])?)\b/.test(masked);
  if (mediump) {
    const floatLiteral = /(?<![A-Za-z0-9_.])(?:\d+\.\d*|\.\d+|\d+[eE][+-]?\d+)(?:[fF])?(?![A-Za-z0-9_])/g;
    for (const match of masked.matchAll(floatLiteral)) {
      const value = Number(match[0].replace(/[fF]$/, ''));
      if (Number.isFinite(value) && Math.abs(value) > 16384) {
        issues.push(issue(source, match.index, `mediump float 字面量 ${match[0]} 超出 ±16384`));
      }
    }
  }

  if (stage === 'frag') {
    const declarations = new Set();
    for (const match of masked.matchAll(/\bprecision\s+highp\s+(float|int|uint)\s*;/g)) declarations.add(match[1]);
    for (const match of masked.matchAll(/\bhighp\s+(float|int|uint|vec[234]|ivec[234]|uvec[234]|mat[234](?:x[234])?)\b/g)) {
      const type = match[1];
      const base = type.startsWith('vec') || type.startsWith('mat') ? 'float'
        : type.startsWith('ivec') ? 'int' : type.startsWith('uvec') ? 'uint' : type;
      const before = masked.slice(0, match.index);
      if (/\bprecision\s*$/.test(before)) continue;
      if (!declarations.has(base)) {
        issues.push(issue(source, match.index, `fragment shader 使用 highp ${type}，但未声明 precision highp ${base};`));
      }
    }
  }
  return issues;
}

function structuralChecks(source, masked, stage) {
  const issues = [];
  const firstLine = (masked.split('\n').find((line) => line.trim().length > 0) || '').replace(/^\uFEFF/, '');
  if (!/^\s*#\s*version\s+300\s+es(?:\s|$)/.test(firstLine)) {
    issues.push(issue(source, 0, '必须以 #version 300 es 开始'));
  }

  const brackets = [];
  const pairs = { '(': ')', '[': ']', '{': '}' };
  for (let index = 0; index < masked.length; index += 1) {
    const current = masked[index];
    if (pairs[current]) brackets.push({ current, index });
    else if (Object.values(pairs).includes(current)) {
      const expected = brackets.pop();
      if (!expected || pairs[expected.current] !== current) {
        issues.push(issue(source, index, `括号不匹配：遇到 ${current}`));
        break;
      }
    }
  }
  if (brackets.length) issues.push(issue(source, brackets.at(-1).index, '括号未闭合'));

  const main = /\bvoid\s+main\s*\(\s*\)/.exec(masked);
  if (!main) {
    issues.push(issue(source, 0, '缺少 void main() 入口'));
  } else {
    let body = main.index + main[0].length;
    while (/\s/.test(masked[body] || '')) body += 1;
    if (masked[body] !== '{') issues.push(issue(source, body, 'void main() 必须有函数体'));
  }
  const constants = collectConstants(masked);
  issues.push(...checkLoops(source, masked, constants));
  issues.push(...checkSamplers(source, masked, constants));
  issues.push(...checkMediumpAndHighp(source, masked, stage));
  return issues;
}

function relativeName(file) {
  return path.relative(repoRoot, file) || path.basename(file);
}

function isInside(directory, file) {
  const relative = path.relative(directory, file);
  return relative !== '' && !relative.startsWith('..') && !path.isAbsolute(relative);
}

function checkEffectsContract(shaderSources) {
  const effectsDirectory = path.join(shaderRoot, 'effects');
  if (!fs.existsSync(effectsDirectory)) {
    return { exists: false, failures: [] };
  }
  if (!fs.statSync(effectsDirectory).isDirectory()) {
    return {
      exists: true,
      failures: [`effects 路径存在但不是目录: ${relativeName(effectsDirectory)}`],
    };
  }

  const failures = [];
  const effectDirectories = fs.readdirSync(effectsDirectory, { withFileTypes: true })
    .filter((entry) => entry.isDirectory())
    .map((entry) => path.join(effectsDirectory, entry.name));
  for (const effectDirectory of effectDirectories) {
    const hasShader = shaderSources.some((shader) => isInside(effectDirectory, shader.file));
    if (!hasShader) {
      failures.push(
        `特效目录 ${relativeName(effectDirectory)} 缺少 shader 源（需要 .glsl 或含 GLSL 的 shader.js/shader.ts）`,
      );
    }
  }
  return { exists: true, failures };
}

function findCompiler() {
  const forced = process.env.GLSL_COMPILER?.trim();
  const candidates = forced ? [forced] : ['glslangValidator', 'glslc', 'shaderc'];
  for (const candidate of candidates) {
    const probe = spawnSync(candidate, ['--version'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
    if (!probe.error) return candidate;
  }
  return '';
}

function compileWithExternalCompiler(compiler, shader, stage) {
  const compilerName = path.basename(compiler).toLowerCase();
  const resolvedStage = stage || 'frag';
  const outputPath = process.platform === 'win32' ? 'NUL' : '/dev/null';
  let compilerFile = shader.file;
  let temporaryDirectory = '';
  try {
    if (shader.kind === 'template') {
      temporaryDirectory = fs.mkdtempSync(path.join(os.tmpdir(), 'check-glsl-'));
      compilerFile = path.join(temporaryDirectory, 'shader.glsl');
      fs.writeFileSync(compilerFile, shader.compilerSource, 'utf8');
    }
    const args = compilerName.includes('glslang')
      ? ['-S', resolvedStage, '-e', 'main', compilerFile]
      : [`-fshader-stage=${resolvedStage}`, '-o', outputPath, compilerFile];
    const result = spawnSync(compiler, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
    if (result.error || result.status !== 0) {
      const output = `${result.stderr || ''}${result.stdout || ''}`.trim().split('\n').slice(0, 8).join('\n');
      return output || String(result.error || `exit ${result.status}`);
    }
    return '';
  } catch (error) {
    return String(error);
  } finally {
    if (temporaryDirectory) fs.rmSync(temporaryDirectory, { recursive: true, force: true });
  }
}

function compilerErrorLine(output) {
  for (const line of output.split('\n')) {
    const match = line.match(/(?:^|:)\s*(?:\d+:)?(\d+)\s*:/);
    if (match) return Number(match[1]);
  }
  return 1;
}

const shaders = collectShaderSources(shaderRoot);
const effectsContract = checkEffectsContract(shaders);
console.log(`GLSL: 扫描到 ${shaders.length} 个 shader (${path.relative(repoRoot, shaderRoot) || '.'})`);
if (!effectsContract.exists) console.log('GLSL: effects 目录不存在,跳过');
const compiler = findCompiler();
if (compiler) console.log(`GLSL: 离线编译器 ${compiler}`);
else console.log('GLSL: 离线编译器跳过,原因:未安装 (glslangValidator/glslc/shaderc)');

const contractFailures = [...effectsContract.failures];
if (!shaders.length && effectsContract.exists) {
  contractFailures.push('effects 目录存在但扫到 0 个 shader；配置错误');
}

if (!shaders.length) {
  if (contractFailures.length) {
    console.error(`GLSL check failed: ${contractFailures.length} 个源契约问题`);
    for (const failure of contractFailures) console.error(`  CONTRACT FAIL: ${failure}`);
    process.exit(1);
  }
  console.log('GLSL check passed: effects 目录不存在,跳过 GLSL 扫描');
  process.exit(0);
}

const failures = [];
for (const shader of shaders) {
  const { file, source } = shader;
  const masked = maskCommentsAndStrings(shader.kind === 'template' ? shader.compilerSource : source);
  const stage = inferStage(file, source, masked, shader.stageHint);
  const problems = structuralChecks(source, masked, stage);
  if (compiler) {
    const compilerError = compileWithExternalCompiler(compiler, shader, stage);
    if (compilerError) {
      problems.push({
        line: compilerErrorLine(compilerError),
        message: `离线编译器拒绝该 shader:\n${compilerError}`,
      });
    }
  }
  const relative = relativeName(file);
  const templateLocation = shader.kind === 'template'
    ? ` (GLSL 模板 L${lineAt(source, shader.templateOffset)})`
    : '';
  if (problems.length) failures.push({ file: relative, problems, templateLocation });
  else console.log(`  PASS ${relative}${templateLocation}${stage ? ` [${stage}]` : ''}`);
}

if (failures.length || contractFailures.length) {
  if (failures.length) {
    console.error(`GLSL check failed: ${failures.length}/${shaders.length} shader(s)`);
  }
  if (contractFailures.length) {
    console.error(`GLSL check failed: ${contractFailures.length} 个源契约问题`);
  }
  for (const failure of failures) {
    console.error(`  FAIL ${failure.file}${failure.templateLocation}`);
    for (const problem of failure.problems) {
      const [first, ...rest] = problem.message.split('\n');
      console.error(`    L${problem.line}: ${first}`);
      for (const line of rest) console.error(`      ${line}`);
    }
  }
  for (const failure of contractFailures) console.error(`  CONTRACT FAIL: ${failure}`);
  process.exit(1);
}

console.log(`GLSL check passed: ${shaders.length} shader(s); ES 3.0 structural/constraint checks passed`);
