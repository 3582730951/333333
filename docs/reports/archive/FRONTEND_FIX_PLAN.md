# Frontend Fix Plan — Session 24b

## 问题清单

### P0 - 导航分组混乱
- **现状**: 17 个管理功能挤在一个"管理员"分组
- **问题**: 用户明确反馈"你觉得只有一个分组合理吗？"
- **方案**: 按功能逻辑重新分成 5 组（用户、核心、安全、运营、系统）

### P1 - iframe 页面 src 未设置
- **现状**: lifecycle/proxies 通过 `window.location.href` 跳转离开 SPA
- **问题**: 破坏 SPA 体验，浏览器前进/后退失效
- **方案**: 在 loadView() 中动态设置 iframe.src

### P2 - 每个页面布局问题
- **现状**: 用户明确指出"每个页面的布局也是存在问题的"
- **分析**: 
  - splitr 布局：420px 右栏在小屏会挤压
  - 表格在小屏可能横向溢出
  - 部分 panel 嵌套过深导致内边距累加
- **方案**: 
  - 优化 splitr 布局的响应式断点
  - 确保所有表格容器有正确的 overflow-x
  - 统一 panel 内边距

## 实施步骤

1. 修改 `i18n.js` - 添加新的分组 key
2. 修改 `app.js` - 重组 NAV_ADMIN 为多组结构 + iframe src 动态加载
3. 修改 `app.css` - 优化响应式布局
4. 构建测试

## 新分组结构

```javascript
const NAV_GROUPS = [
  { key: "grp.user", items: NAV_USER },
  { key: "grp.admin_core", items: [...] },      // 概览、账号、供应商、出口
  { key: "grp.admin_security", items: [...] },  // 隔离、审计、CF、内容审查
  { key: "grp.admin_ops", items: [...] },       // 用量、分组、Key、用户、租户
  { key: "grp.admin_system", items: [...] }     // GoPay、生命周期、代理、设置
];
```
