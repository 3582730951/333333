// Minimal i18n layer: a single dictionary mapping logical keys to zh/en strings,
// plus a hook + LocaleProvider-friendly API. The backend's config/setting labels
// remain server-controlled; this layer covers the SPA chrome (nav, page titles,
// table column headers, buttons, toast messages).
//
// Scope is intentionally narrow — only the strings the operator sees in the
// static UI. Dynamic content (account labels, error bodies returned by the API)
// passes through unchanged.

import { getLocalItem, setLocalItem } from './browserStorage.js';
import { dispatchBrowserEvent } from './browserEvents.js';

const zh = {
  'app.title': 'Pool 控制台',
  'app.admin': '管理控制台',
  'app.portal': '用户门户',
  'app.logout': '退出',
  'app.theme': '切换主题',
  'app.language': '语言',

  'nav.dashboard': '总览',
  'nav.accounts': '账号',
  'nav.account_pool': '账号池',
  'nav.groups': '分组',
  'nav.egress': '出口/代理',
  'nav.providers': '模型提供商',
  'nav.registration': '注册',
  'nav.auto_register': '自动注册',
  'nav.automation': '自动化策略',
  'nav.lifecycle': '生命周期',
  'nav.monitor': '监控',
  'nav.usage': '用量',
  'nav.quota': '配额',
  'nav.system': '系统监控',
  'nav.cf_events': 'CF 事件',
  'nav.audit': '审计日志',
  'nav.sys': '系统',
  'nav.keys': 'API Keys',
  'nav.users': '用户',
  'nav.thinking': '思考配置',
  'nav.moderation': '合规',
  'nav.gopay': 'GoPay',
  'nav.settings': '设置中心',
  'nav.my_usage': '我的用量',
  'nav.my_keys': '我的 Key',
  'nav.my_profile': '我的资料',

  'common.refresh': '刷新',
  'common.save': '保存',
  'common.create': '创建',
  'common.delete': '删除',
  'common.cancel': '取消',
  'common.search': '搜索',
  'common.export': '导出 CSV',
  'common.start': '启动',
  'common.empty': '暂无数据',
  'common.loading': '加载中…',
};

const en = {
  'app.title': 'Pool Console',
  'app.admin': 'Admin Console',
  'app.portal': 'User Portal',
  'app.logout': 'Logout',
  'app.theme': 'Toggle theme',
  'app.language': 'Language',

  'nav.dashboard': 'Overview',
  'nav.accounts': 'Accounts',
  'nav.account_pool': 'Account Pool',
  'nav.groups': 'Groups',
  'nav.egress': 'Egress / Proxy',
  'nav.providers': 'Model Providers',
  'nav.registration': 'Registration',
  'nav.auto_register': 'Auto-Register',
  'nav.automation': 'Automation',
  'nav.lifecycle': 'Lifecycle',
  'nav.monitor': 'Monitor',
  'nav.usage': 'Usage',
  'nav.quota': 'Quota',
  'nav.system': 'System',
  'nav.cf_events': 'CF Events',
  'nav.audit': 'Audit Log',
  'nav.sys': 'System',
  'nav.keys': 'API Keys',
  'nav.users': 'Users',
  'nav.thinking': 'Thinking',
  'nav.moderation': 'Moderation',
  'nav.gopay': 'GoPay',
  'nav.settings': 'Settings',
  'nav.my_usage': 'My Usage',
  'nav.my_keys': 'My Keys',
  'nav.my_profile': 'Profile',

  'common.refresh': 'Refresh',
  'common.save': 'Save',
  'common.create': 'Create',
  'common.delete': 'Delete',
  'common.cancel': 'Cancel',
  'common.search': 'Search',
  'common.export': 'Export CSV',
  'common.start': 'Start',
  'common.empty': 'No data',
  'common.loading': 'Loading…',
};

const DICTS = { zh, en };

export function getLocale() {
  return getLocalItem('pool_locale', 'zh') === 'en' ? 'en' : 'zh';
}

export function setLocale(loc) {
  const normalized = loc === 'en' ? 'en' : 'zh';
  setLocalItem('pool_locale', normalized);
  dispatchBrowserEvent('pool-locale-change', normalized);
}

// t(key, fallback?) returns the translated string for the active locale.
export function t(key, fallback) {
  const loc = getLocale();
  const dict = DICTS[loc] || zh;
  return dict[key] || fallback || key;
}
