import React, { useState } from 'react';
import { Form, Button, Toast, Avatar } from '../components/pool/index.jsx';
import { IconCheckCircleStroked, IconKey, IconLineChartStroked, IconUserGroup } from '../components/pool/icons.jsx';
import { setToken, clearToken, get, login, registerUser } from '../api.js';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import { t } from '../lib/i18n.js';

export default function Login({ onSuccess }) {
  const [mode, setMode] = useState('login');
  const [adminMode, setAdminMode] = useState(false);
  const [adminError, setAdminError] = useState('');

  const { run: adminSubmit, running: adminLoading } = useAsyncAction(async (values) => {
    setAdminError('');
    setToken((values.token || '').trim());
    try {
      await get('/admin/config', undefined, { suppressUnauthorizedEvent: true });
      Toast.success(t('auth.success'));
      onSuccess();
    } catch {
      clearToken();
      setAdminError(t('auth.admin_invalid'));
      Toast.error(t('auth.admin_invalid'));
    }
  });

  const { run: userSubmit, running: userLoading } = useAsyncAction(async (values) => {
    try {
      if (mode === 'register') await registerUser(values.email, values.password, values.name);
      else await login(values.email, values.password);
      Toast.success(mode === 'register' ? t('auth.registered') : t('auth.success'));
      onSuccess();
    } catch (error) {
      showErrorToast(error);
    }
  });

  const switchToUser = () => {
    setAdminMode(false);
    setAdminError('');
  };

  return (
    <main id="main-content" tabIndex={-1} className="pool-login-wrap pool-auth-shell">
      <section className="pool-login-story" aria-labelledby="pool-login-heading">
        <div className="pool-login-wordmark"><Avatar size="small">P</Avatar><span>{t('app.title')}</span></div>
        <div className="pool-login-copy">
          <span className="pool-login-eyebrow">{t('auth.eyebrow')}</span>
          <h1 id="pool-login-heading" tabIndex={-1}>{t('auth.login_title')}</h1>
          <p>{t('auth.value')}</p>
        </div>

        <div className="pool-login-preview" aria-label={t('auth.preview_label')}>
          <div className="pool-login-preview__bar"><i /><span>{t('auth.preview_title')}</span><b>{t('auth.preview_live')}</b></div>
          <div className="pool-login-preview__body">
            <div className="pool-login-preview__usage">
              <span>{t('auth.preview_usage')}</span>
              <strong>128k · 256k · 1M</strong>
              <i><b /></i>
            </div>
            <div className="pool-login-preview__code">
              <span>POST /v1/responses</span>
              <code>Authorization: Bearer cap_••••••••</code>
            </div>
          </div>
        </div>

        <ul className="pool-login-trust">
          <li><IconKey /><span><b>{t('auth.trust_keys')}</b><small>{t('auth.trust_keys_desc')}</small></span></li>
          <li><IconLineChartStroked /><span><b>{t('auth.trust_usage')}</b><small>{t('auth.trust_usage_desc')}</small></span></li>
          <li><IconUserGroup /><span><b>{t('auth.trust_pool')}</b><small>{t('auth.trust_pool_desc')}</small></span></li>
        </ul>
      </section>

      <section className="pool-login-form-column" aria-label={adminMode ? t('auth.admin_entry') : t('auth.user_entry')}>
        <div className="pool-login-card">
          <div className="pool-login-form-head">
            <span className="pool-login-form-icon" aria-hidden="true">{adminMode ? <IconKey /> : <IconCheckCircleStroked />}</span>
            <div>
              <h2>{adminMode ? t('auth.admin_title') : mode === 'register' ? t('auth.register_title') : t('auth.user_title')}</h2>
              <p>{adminMode ? t('auth.admin_help') : mode === 'register' ? t('auth.register_help') : t('auth.user_help')}</p>
            </div>
          </div>

          {adminMode ? (
            <Form onSubmit={adminSubmit}>
              <Form.Input
                field="token"
                label={t('auth.admin_token')}
                mode="password"
                placeholder="admin_token"
                rules={[{ required: true, message: t('auth.admin_token_required') }]}
                allowReveal
                showClear
                onChange={() => setAdminError('')}
                aria-invalid={adminError ? true : undefined}
                aria-describedby={adminError ? 'pool-admin-login-error' : undefined}
              />
              {adminError ? <div id="pool-admin-login-error" className="pool-login-error" role="alert">{adminError}</div> : null}
              <Button htmlType="submit" theme="solid" block loading={adminLoading}>{t('auth.admin_submit')}</Button>
            </Form>
          ) : (
            <>
              <div className="pool-auth-mode" role="tablist" aria-label={t('auth.user_mode')}>
                <button type="button" role="tab" aria-selected={mode === 'login'} onClick={() => setMode('login')}>{t('auth.sign_in')}</button>
                <button type="button" role="tab" aria-selected={mode === 'register'} onClick={() => setMode('register')}>{t('auth.register')}</button>
              </div>
              <Form onSubmit={userSubmit}>
                <Form.Input
                  field="email"
                  label={t('auth.email')}
                  placeholder="you@example.com"
                  autoComplete="email"
                  rules={[
                    { required: true, message: t('auth.email_required') },
                    { type: 'email', message: t('auth.email_invalid') },
                  ]}
                />
                {mode === 'register' ? <Form.Input field="name" label={t('auth.name')} placeholder={t('auth.name_optional')} autoComplete="name" /> : null}
                <Form.Input
                  field="password"
                  label={t('auth.password')}
                  mode="password"
                  placeholder={mode === 'register' ? t('auth.password_hint') : t('auth.password')}
                  autoComplete={mode === 'register' ? 'new-password' : 'current-password'}
                  rules={[
                    { required: true, message: t('auth.password_required') },
                    ...(mode === 'register' ? [{ min: 8, message: t('auth.password_hint') }] : []),
                  ]}
                  allowReveal
                />
                <Button htmlType="submit" theme="solid" block loading={userLoading}>
                  {mode === 'register' ? t('auth.register_submit') : t('auth.user_submit')}
                </Button>
              </Form>
            </>
          )}

          <div className="pool-login-secondary">
            <span>{adminMode ? t('auth.user_entry_question') : t('auth.admin_question')}</span>
            <button type="button" onClick={adminMode ? switchToUser : () => setAdminMode(true)}>
              {adminMode ? t('auth.user_entry') : t('auth.admin_entry')}
            </button>
          </div>
        </div>
      </section>
    </main>
  );
}
