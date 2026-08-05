import React, { useCallback, useRef } from 'react';
import { Avatar, Form, Button, Toast, Tag } from '../../components/pool/index.jsx';
import { IconSave } from '../../components/pool/icons.jsx';
import { me, patch } from '../../api.js';
import LoadErrorBanner from '../../components/LoadErrorBanner.jsx';
import PageHeader from '../../components/PageHeader.jsx';
import { showErrorToast } from '../../components/ErrorToast.jsx';
import useAsyncAction from '../../hooks/useAsyncAction.js';
import useAsyncResource from '../../hooks/useAsyncResource.js';

export default function PortalProfile() {
  const formApi = useRef(null);

  const fetchProfile = useCallback(async ({ signal }) => me({ signal }), []);
  const { data: user, loading, error, reload: load } = useAsyncResource(fetchProfile, [fetchProfile], { initialData: null });

  const { run: save, running: saving } = useAsyncAction(async () => {
    try {
      const v = formApi.current.getValues();
      const body = { name: v.name };
      if (v.new_password) { body.old_password = v.old_password || ''; body.new_password = v.new_password; }
      await patch('/user/profile', body);
      Toast.success('已保存');
      formApi.current.setValue('old_password', ''); formApi.current.setValue('new_password', '');
      await load();
    } catch (e) { showErrorToast(e); }
  });

  if (error && !user && !loading) {
    return (
      <div>
        <PageHeader title="我的资料" subtitle="账户信息与密码" />
        <LoadErrorBanner error={error} onRetry={load} title="资料读取失败" />
      </div>
    );
  }

  return (
    <div>
      <PageHeader title="我的资料" subtitle="账户信息与密码" />
      <LoadErrorBanner error={error} onRetry={load} />
      <div className="pool-panel pool-portal-profile-card">
        <div className="pool-portal-profile-identity">
          <Avatar size="large">{String(user?.name || user?.email || 'U').charAt(0).toUpperCase()}</Avatar>
          <div>
            <div className="pool-muted">邮箱</div>
            <div className="pool-portal-profile-email">{loading && !user ? '读取中...' : user?.email || '—'} {user?.role && <Tag color={user.role === 'admin' ? 'violet' : 'blue'}>{user.role}</Tag>}</div>
          </div>
        </div>
        {user && (
          <Form className="pool-portal-profile-form" getFormApi={(a) => { formApi.current = a; }} labelPosition="top" initValues={{ name: user.name || '' }}>
            <div className="pool-portal-profile-section">
              <div className="pool-profile-section__title">个人信息</div>
              <Form.Input field="name" label="名称" placeholder="显示名称" />
            </div>
            <div className="pool-profile-section">
              <div className="pool-profile-section__title">修改密码</div>
              <div className="pool-portal-profile-passwords">
                <Form.Input field="old_password" label="当前密码" mode="password" placeholder="如已设置密码则必填" />
                <Form.Input field="new_password" label="新密码" mode="password" placeholder="≥8 位，留空不修改" />
              </div>
            </div>
            <div className="pool-portal-profile-actions"><Button theme="solid" icon={<IconSave />} loading={saving} onClick={save}>保存</Button></div>
          </Form>
        )}
      </div>
    </div>
  );
}
