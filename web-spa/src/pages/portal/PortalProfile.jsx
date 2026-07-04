import React, { useCallback, useRef } from 'react';
import { Form, Button, Toast, Tag } from '../../components/pool/index.jsx';
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

  return (
    <div>
      <PageHeader title="我的资料" subtitle="账户信息与密码" />
      <LoadErrorBanner error={error} onRetry={load} />
      <div className="pool-panel" style={{ maxWidth: 560 }}>
        <div style={{ marginBottom: 16 }}>
          <div className="pool-muted" style={{ fontSize: 12 }}>邮箱</div>
          <div style={{ fontSize: 16, fontWeight: 600 }}>{loading && !user ? '读取中...' : user?.email || '—'} {user?.role && <Tag color={user.role === 'admin' ? 'violet' : 'blue'} style={{ marginLeft: 8 }}>{user.role}</Tag>}</div>
        </div>
        {user && (
          <Form getFormApi={(a) => { formApi.current = a; }} labelPosition="left" labelWidth={110} initValues={{ name: user.name || '' }}>
            <Form.Input field="name" label="名称" placeholder="显示名称" style={{ width: 320, maxWidth: '100%' }} />
            <div className="pool-profile-section">
              <div className="pool-profile-section__title">修改密码</div>
              <Form.Input field="old_password" label="当前密码" mode="password" placeholder="如已设置密码则必填" style={{ width: 320, maxWidth: '100%' }} />
              <Form.Input field="new_password" label="新密码" mode="password" placeholder="≥8 位，留空不修改" style={{ width: 320, maxWidth: '100%' }} />
            </div>
            <Button theme="solid" icon={<IconSave />} loading={saving} onClick={save} style={{ marginTop: 16 }}>保存</Button>
          </Form>
        )}
      </div>
    </div>
  );
}
