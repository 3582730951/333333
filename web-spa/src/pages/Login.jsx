import React, { useState } from 'react';
import { Card, Form, Button, Toast, Typography, Avatar, Tabs, TabPane } from '../components/pool/index.jsx';
import { setToken, clearToken, get, login, registerUser } from '../api.js';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';

export default function Login({ onSuccess }) {
  const [mode, setMode] = useState('login'); // user tab: login | register
  const [adminError, setAdminError] = useState('');

  const { run: adminSubmit, running: adminLoading } = useAsyncAction(async (values) => {
    setAdminError('');
    setToken((values.token || '').trim());
    try {
      await get('/admin/config', undefined, { suppressUnauthorizedEvent: true });
      Toast.success('登录成功');
      onSuccess();
    } catch {
      clearToken();
      setAdminError('无法登录。Token 无效或已过期。');
      Toast.error('无法登录。Token 无效或已过期。');
    }
  });

  const { run: userSubmit, running: userLoading } = useAsyncAction(async (v) => {
    try {
      if (mode === 'register') await registerUser(v.email, v.password, v.name);
      else await login(v.email, v.password);
      Toast.success(mode === 'register' ? '注册成功' : '登录成功');
      onSuccess();
    } catch (e) {
      showErrorToast(e);
    }
  });

  return (
    <div className="pool-login-wrap">
      <Card className="pool-card pool-login-card">
        <div className="pool-login-brand">
          <Avatar size="default" style={{ background: 'var(--pool-accent)' }}>P</Avatar>
          <Typography.Title heading={3}>登录 Pool 控制台</Typography.Title>
          <Typography.Text type="tertiary" size="small">使用管理员凭据进入控制台。</Typography.Text>
        </div>
        <Tabs type="line" size="small">
          <TabPane tab="管理员" itemKey="admin">
            <Form onSubmit={adminSubmit} style={{ marginTop: 8 }}>
              <Form.Input
                field="token"
                label="Token"
                mode="password"
                placeholder="admin_token"
                rules={[{ required: true, message: '请输入 Token。' }]}
                allowReveal
                showClear
                onChange={() => setAdminError('')}
              />
              {adminError ? <div className="pool-login-error" role="alert">{adminError}</div> : null}
              <Button htmlType="submit" theme="solid" block loading={adminLoading} style={{ marginTop: 14 }}>登录</Button>
            </Form>
            <Typography.Text type="tertiary" size="small" style={{ display: 'block', marginTop: 12 }}>
              管理员 Token 由服务端配置生成。
            </Typography.Text>
          </TabPane>
          <TabPane tab="用户登录" itemKey="user">
            <Form onSubmit={userSubmit} style={{ marginTop: 8 }}>
              <Form.Input field="email" label="邮箱" placeholder="you@example.com"
                rules={[{ required: true, message: '请输入邮箱' }]} />
              {mode === 'register' && <Form.Input field="name" label="名称" placeholder="可选" />}
              <Form.Input field="password" label="密码" mode="password" placeholder={mode === 'register' ? '≥8 位' : '密码'}
                rules={[{ required: true, message: '请输入密码' }]} />
              <Button htmlType="submit" theme="solid" block loading={userLoading} style={{ marginTop: 14 }}>
                {mode === 'register' ? '注册并登录' : '登录'}
              </Button>
            </Form>
            <div style={{ textAlign: 'center', marginTop: 12 }}>
              <Typography.Text link size="small" onClick={() => setMode(mode === 'register' ? 'login' : 'register')}>
                {mode === 'register' ? '已有账号？去登录' : '没有账号？注册'}
              </Typography.Text>
            </div>
          </TabPane>
        </Tabs>
      </Card>
    </div>
  );
}
