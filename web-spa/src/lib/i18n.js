// Locale dictionaries are separate lazy chunks. The selected dictionary is
// loaded before the SPA mounts, so normal rendering remains synchronous while
// the initial transfer no longer contains both 50KiB language tables.
import { getLocalItem, setLocalItem } from './browserStorage.js';
import { dispatchBrowserEvent } from './browserEvents.js';

const loaders = {
  zh: () => import('./locales/zh.js'),
  en: () => import('./locales/en.js'),
};

const core = {
  zh: {
    'common.close': '关闭',
    'common.copied': '已复制',
    'keys.legacy_rotate': '旧 Key，需轮换后复制',
    'keys.copy_key': '复制 Key',
    'keys.copy_command': '复制安装命令',
    'keys.copied_key': '已复制 API Key',
    'keys.copied_command': '已复制安装命令',
    'keys.copy_failed': '复制失败，请手动选择文本',
    'keys.created_panel': '新 Key 已创建',
    'keys.install_help': '安装命令会访问当前 VPS；配置 Codex 时可选择 Super-Instruct（仍受 API Key 所属用户分组策略限制），也可配置 Claude Code 网关与 rtk。',
  },
  en: {
    'common.close': 'Close',
    'common.copied': 'Copied',
    'keys.legacy_rotate': 'Legacy key; rotate it before copying',
    'keys.copy_key': 'Copy key',
    'keys.copy_command': 'Copy install command',
    'keys.copied_key': 'API key copied',
    'keys.copied_command': 'Install command copied',
    'keys.copy_failed': 'Copy failed. Select the text manually.',
    'keys.created_panel': 'New key created',
    'keys.install_help': 'The command connects to this VPS; Codex setup offers a Super-Instruct choice bounded by the API key user-group policy, and can also configure the Claude Code gateway and rtk.',
  },
};

const dictionaries = {};
const pending = {};

export function getLocale() {
  return getLocalItem('pool_locale', 'zh') === 'en' ? 'en' : 'zh';
}

export async function loadLocale(locale = getLocale()) {
  const normalized = locale === 'en' ? 'en' : 'zh';
  if (dictionaries[normalized]) return normalized;
  if (!pending[normalized]) {
    pending[normalized] = loaders[normalized]().then((module) => {
      dictionaries[normalized] = module.default || {};
      return normalized;
    }).finally(() => { delete pending[normalized]; });
  }
  return pending[normalized];
}

export function syncDocumentLanguage(locale = getLocale()) {
  const normalized = locale === 'en' ? 'en' : 'zh';
  if (typeof document !== 'undefined') {
    document.documentElement.lang = normalized === 'en' ? 'en' : 'zh-CN';
  }
  return normalized;
}

export function setLocale(loc) {
  const normalized = loc === 'en' ? 'en' : 'zh';
  setLocalItem('pool_locale', normalized);
  syncDocumentLanguage(normalized);
  const ready = loadLocale(normalized);
  ready.then(() => dispatchBrowserEvent('pool-locale-change', normalized));
  return ready;
}

export function t(key, fallback) {
  const loc = getLocale();
  const dict = dictionaries[loc] || core[loc] || core.zh;
  return dict[key] || fallback || key;
}
