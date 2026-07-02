import React, { useState, useCallback, useRef, useEffect } from 'react';
import { ActionMenu, Button, Toast, Tag, Modal, Form, Select, Typography } from '../components/pool/index.jsx';
import { IconRefresh, IconEdit, IconPlus } from '../components/pool/icons.jsx';
import { get, post } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { ActionGroup, MetricRail, TextClamp } from '../components/DisplayPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import EgressProfileForm from '../components/EgressProfileForm.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { loadResourceGroup } from '../lib/resource.js';

// 代理模式常量：决定 cliproxy 取 IP 的方式。
const AUTH_MODES = [
  { value: '', label: '账号密码模式 (credential)', desc: '用户名内嵌 region/sid，网关自动轮换。每次 sid 轮换取新 IP。' },
  { value: 'api_whitelist', label: 'API 白名单模式 (api_whitelist)', desc: '调 api.cliproxy.io/white/api 提取 ip:port，无认证，按 region 锁国家。' },
];

const EMPTY_EGRESS = { profiles: [], pools: [], config: [], error: null };

const formatDynamicConfig = (value) => {
  if (!value) return '{}';
  if (typeof value === 'string') {
    try { return JSON.stringify(JSON.parse(value), null, 2); }
    catch { return value; }
  }
  try { return JSON.stringify(value, null, 2); }
  catch { return '{}'; }
};

const parseDynamicConfig = (value) => {
  const text = String(value || '').trim();
  if (!text) return {};
  try { return JSON.parse(text); }
  catch { return text; }
};

export default function Egress() {
  const [editing, setEditing] = useState(null); // null | {} (new) | existing row
  const [poolEditing, setPoolEditing] = useState(null);
  const [memberEditing, setMemberEditing] = useState(null);
  const [savedProfile, setSavedProfile] = useState(null);
  const [registrationPoolDraft, setRegistrationPoolDraft] = useState('');
  const formApi = useRef(null);
  const poolFormApi = useRef(null);
  const memberFormApi = useRef(null);

  const fetchRows = useCallback(async ({ signal }) => {
    const { values, error } = await loadResourceGroup({
      profiles: { label: '出口配置', load: () => get('/admin/egress-profiles', undefined, { signal }) },
      pools: { label: '注册池', load: () => get('/admin/egress-pools', undefined, { signal }) },
      config: { label: '系统配置', load: () => get('/admin/config', undefined, { signal }) },
    });
    return {
      profiles: Array.isArray(values.profiles) ? values.profiles : values.profiles?.profiles || values.profiles?.egress_profiles || [],
      pools: Array.isArray(values.pools) ? values.pools : values.pools?.pools || [],
      config: Array.isArray(values.config) ? values.config : [],
      error,
    };
  }, []);
  const { data = EMPTY_EGRESS, loading, error, lastRefresh, reload: load } = useAsyncResource(fetchRows, [fetchRows], { initialData: EMPTY_EGRESS });
  const rows = data.profiles || [];
  const pools = data.pools || [];
  const configRows = data.config || [];
  const registrationPoolSetting = configRows.find((row) => row.key === 'registration_egress_pool_id')?.value || '';
  const registrationPools = pools.filter((pool) => !pool.purpose || pool.purpose === 'registration');
  const healthyCount = rows.filter((row) => !row.health || row.health === 'healthy').length;
  const proxyCount = rows.filter((row) => row.type && row.type !== 'direct').length;
  const concurrencyTotal = rows.reduce((sum, row) => sum + (Number(row.max_concurrency) || 0), 0);
  const egressMetrics = [
    { label: '出口数', value: rows.length },
    { label: '健康出口', value: healthyCount, tone: healthyCount === rows.length ? 'success' : 'warning' },
    { label: '代理出口', value: proxyCount },
    { label: '注册池', value: registrationPools.length },
    { label: '总并发', value: concurrencyTotal },
  ];

  useEffect(() => {
    setRegistrationPoolDraft(registrationPoolSetting);
  }, [registrationPoolSetting]);

  const openEdit = (row) => {
    setEditing(row ? { ...row, dynamic_config_json: formatDynamicConfig(row.dynamic_config_json) } : {
      id: '', name: '', type: 'http_proxy', endpoint: '', region: '', exit_ip: '',
      chain_proxy: '', max_concurrency: 16, proxy_auth_mode: '', proxy_api_key: '',
      ip_mode: 'dynamic_residential', provider_key: '', dynamic_config_json: '{}',
      api_base: 'https://api.cliproxy.io', api_num: 1, api_time: 10,
    });
  };

  const { run: save, running: saving } = useAsyncAction(async (vals) => {
    try {
      const body = { ...editing, ...vals, dynamic_config_json: parseDynamicConfig(vals.dynamic_config_json) };
      const saved = await post('/admin/egress-profiles', body);
      Toast.success('已保存');
      setEditing(null);
      setSavedProfile(saved || body);
      await load();
    } catch (err) {
      showErrorToast(err);
    }
  });

  const submitEdit = useCallback(() => {
    formApi.current?.submitForm?.();
  }, []);

  const { run: savePool, running: savingPool } = useAsyncAction(async (vals) => {
    try {
      await post('/admin/egress-pools', { ...poolEditing, ...vals, purpose: 'registration' });
      Toast.success('注册池已保存');
      setPoolEditing(null);
      await load();
    } catch (err) {
      showErrorToast(err);
    }
  });

  const { run: saveMember, running: savingMember } = useAsyncAction(async (vals) => {
    try {
      await post(`/admin/egress-pools/${encodeURIComponent(memberEditing.id)}/members`, {
        egress_id: vals.egress_id,
        enabled: vals.enabled !== false,
        capacity: Number(vals.capacity) || 0,
      });
      Toast.success('成员已保存');
      setMemberEditing(null);
      await load();
    } catch (err) {
      showErrorToast(err);
    }
  });

  const { run: saveRegistrationPool, running: savingRegistrationPool } = useAsyncAction(async () => {
    try {
      await post('/admin/settings-center', [{
        section: 'config',
        values: { registration_egress_pool_id: registrationPoolDraft || '' },
      }]);
      Toast.success('默认注册池已保存');
      await load();
    } catch (err) {
      showErrorToast(err);
    }
  });

  const { run: joinSavedProfileToRegistrationPool, running: joiningSavedProfile } = useAsyncAction(async () => {
    if (!savedProfile?.id) return;
    try {
      const fallbackPoolID = 'pool_registration_default';
      let poolID = registrationPoolSetting || registrationPools[0]?.id || fallbackPoolID;
      const poolExists = registrationPools.some((pool) => pool.id === poolID);
      if (!poolExists) {
        await post('/admin/egress-pools', {
          id: poolID,
          name: '默认注册池',
          purpose: 'registration',
          assignment_strategy: 'sticky_least_used',
        });
      }
      if (!registrationPoolSetting) {
        await post('/admin/settings-center', [{
          section: 'config',
          values: { registration_egress_pool_id: poolID },
        }]);
        setRegistrationPoolDraft(poolID);
      }
      await post(`/admin/egress-pools/${encodeURIComponent(poolID)}/members`, {
        egress_id: savedProfile.id,
        enabled: true,
        capacity: 0,
      });
      Toast.success('已加入默认注册池');
      setSavedProfile(null);
      await load();
    } catch (err) {
      showErrorToast(err);
    }
  });

  const renderHealth = (v) => {
    const ok = !v || v === 'healthy';
    const warning = ['warning', 'degraded', 'pending'].includes(v);
    return <Tag color={ok ? 'green' : warning ? 'orange' : 'red'}>{v || 'healthy'}</Tag>;
  };

  const renderMode = (v) => {
    const m = AUTH_MODES.find(m => m.value === v);
    return <Tag color={v === 'api_whitelist' ? 'blue' : 'grey'}>{m ? m.label.split(' ')[0] : 'credential'}</Tag>;
  };

  const renderActions = (row) => (
    <ActionGroup minWidth={88} compact>
      <Button size="small" icon={<IconEdit />} onClick={() => openEdit(row)}>编辑</Button>
    </ActionGroup>
  );

  const columns = [
    {
      title: '出口',
      key: 'summary',
      width: 260,
      render: (_, row) => (
        <div className="pool-resource-summary">
          <TextClamp strong>{row.name || row.id || 'Direct'}</TextClamp>
          <div className="pool-resource-summary__meta">
            <Tag size="small">{row.type || 'direct'}</Tag>
            <span>{row.id || 'direct'}</span>
          </div>
        </div>
      ),
    },
    {
      title: '网络',
      key: 'network',
      width: 220,
      render: (_, row) => (
        <div className="pool-resource-summary">
          <TextClamp>{row.exit_ip || '—'}</TextClamp>
          <div className="pool-resource-summary__meta">{row.region || '未指定地区'}</div>
        </div>
      ),
    },
    {
      title: '代理模式',
      dataIndex: 'proxy_auth_mode',
      width: 140,
      render: renderMode,
    },
    { title: '健康', dataIndex: 'health', width: 120, render: renderHealth },
    { title: '并发', dataIndex: 'max_concurrency', width: 96, render: (v) => v ?? '—' },
    {
      title: '操作',
      key: 'ops',
      width: 124,
      render: (_, row) => renderActions(row),
    },
  ];
  const registrationPoolColumns = [
    {
      title: '注册池',
      key: 'pool',
      width: 260,
      render: (_, row) => (
        <div className="pool-resource-summary">
          <TextClamp strong>{row.name || row.id}</TextClamp>
          <div className="pool-resource-summary__meta">
            <Tag size="small" color="orange">registration</Tag>
            <span>{row.id}</span>
          </div>
        </div>
      ),
    },
    {
      title: '策略',
      dataIndex: 'assignment_strategy',
      width: 160,
      render: (v) => <Tag color="blue">{v || 'sticky_least_used'}</Tag>,
    },
    {
      title: '成员',
      key: 'members',
      width: 320,
      render: (_, row) => (
        <div className="pool-resource-summary">
          <TextClamp>{(row.members || []).map((m) => m.egress?.name || m.egress_id).join(', ') || '未添加成员'}</TextClamp>
          <div className="pool-resource-summary__meta">{(row.members || []).length} 个出口</div>
        </div>
      ),
    },
    {
      title: '操作',
      key: 'ops',
      width: 160,
      render: (_, row) => (
        <ActionMenu
          label="注册池操作"
          items={[
            { label: '编辑', icon: <IconEdit />, onSelect: () => setPoolEditing({ ...row }) },
            { label: '成员', icon: <IconPlus />, onSelect: () => setMemberEditing({ id: row.id, egress_id: '', enabled: true, capacity: 0 }) },
          ]}
        />
      ),
    },
  ];
  const mobileColumns = [
    {
      title: '出口',
      key: 'mobile',
      render: (_, row) => (
        <MobileResourceCell
          title={row.name || row.id || 'Direct'}
          subtitle={`${row.exit_ip || '—'} · ${row.region || '未指定地区'}`}
          badges={<><Tag>{row.type || 'direct'}</Tag>{renderHealth(row.health)}</>}
          details={[
            { label: '代理模式', value: renderMode(row.proxy_auth_mode) },
            { label: '并发', value: row.max_concurrency ?? '—' },
          ]}
          actions={renderActions(row)}
        />
      ),
    },
  ];

  return (
    <div>
      <PageHeader title="出口 / 代理" subtitle={`共 ${rows.length} 个出口 · curl_cffi sidecar / 住宅代理 / WARP / CLIPProxy`}
        actions={<>
          <Button icon={<IconPlus />} onClick={() => setPoolEditing({ id: '', name: '', purpose: 'registration', assignment_strategy: 'sticky_least_used' })}>新建注册池</Button>
          <Button icon={<IconPlus />} onClick={() => openEdit(null)}>新建</Button>
          <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
        </>} />
      <div className="pool-toolbar pool-egress-registration-toolbar">
        <Typography.Text strong>默认注册池</Typography.Text>
        <Select
          value={registrationPoolDraft}
          onChange={setRegistrationPoolDraft}
          optionList={[
            { label: '未设置', value: '' },
            ...(registrationPools || []).map((pool) => ({ label: `${pool.name || pool.id} (${pool.members?.length || 0})`, value: pool.id })),
          ]}
          style={{ width: 260 }}
        />
        <Button
          size="small"
          loading={savingRegistrationPool}
          disabled={registrationPoolDraft === registrationPoolSetting}
          onClick={saveRegistrationPool}
        >保存</Button>
      </div>
      <div className="pool-resource-split">
        <ResourceTable
          error={error}
          onRetry={load}
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={rows}
          columns={columns}
          rowKey="id"
          pagination={{ pageSize: 20 }}
          className="pool-mobile-table pool-egress-table"
          mobileColumns={mobileColumns}
          mobileScroll={false}
          density="compact"
          layout="fit"
          scroll={false}
          rowHeight={64}
          emptyTitle="暂无出口配置"
          emptyType="egress"
          skeletonRows={6}
        />
        <MetricRail items={egressMetrics} />
      </div>
      <ResourceTable
        error={data.error}
        onRetry={load}
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={registrationPools}
        columns={registrationPoolColumns}
        rowKey="id"
        pagination={{ pageSize: 12 }}
        className="pool-egress-pools-table"
        density="compact"
        layout="fit"
        scroll={false}
        rowHeight={64}
        emptyTitle="暂无注册池"
        emptyDesc="创建注册池后，注册任务可从池内选择代理出口"
        emptyType="egress"
        skeletonRows={4}
      />

      <Modal
        title={editing?.id ? `编辑出口 ${editing.id}` : '新建出口'}
        visible={!!editing}
        onCancel={() => {
          if (saving) return;
          formApi.current = null;
          setEditing(null);
        }}
        onOk={submitEdit}
        confirmLoading={saving}
        maskClosable={!saving}
        width={640}
      >
        {editing && (
          <EgressProfileForm
            initialValues={editing}
            saving={saving}
            onSubmit={save}
            getFormApi={(api) => { formApi.current = api; }}
          />
        )}
      </Modal>

      <Modal
        title="加入注册池"
        visible={!!savedProfile}
        onCancel={() => {
          if (!joiningSavedProfile) setSavedProfile(null);
        }}
        maskClosable={!joiningSavedProfile}
        width={520}
        footer={(
          <>
            <Button disabled={joiningSavedProfile} onClick={() => setSavedProfile(null)}>稍后处理</Button>
            <Button theme="solid" loading={joiningSavedProfile} onClick={joinSavedProfileToRegistrationPool}>加入默认注册池</Button>
          </>
        )}
      >
        <div className="pool-egress-next-step">
          <Typography.Text strong>{savedProfile?.name || savedProfile?.id}</Typography.Text>
          <Typography.Text type="tertiary">保存成功。注册任务只会从注册池选择出口，建议现在把这个出口加入默认注册池。</Typography.Text>
          <div className="pool-resource-summary__meta">
            <Tag color="orange">{registrationPoolSetting || registrationPools[0]?.id || 'pool_registration_default'}</Tag>
            <span>{savedProfile?.type || 'direct'} · {savedProfile?.region || '未指定地区'}</span>
          </div>
        </div>
      </Modal>

      <Modal
        title={poolEditing?.id ? `编辑注册池 ${poolEditing.id}` : '新建注册池'}
        visible={!!poolEditing}
        onCancel={() => {
          if (savingPool) return;
          poolFormApi.current = null;
          setPoolEditing(null);
        }}
        onOk={() => poolFormApi.current?.submitForm?.()}
        confirmLoading={savingPool}
        maskClosable={!savingPool}
        width={520}
      >
        {poolEditing && (
          <Form
            key={poolEditing.id || 'new-pool'}
            getFormApi={(api) => { poolFormApi.current = api; }}
            initValues={poolEditing}
            onSubmit={savePool}
            labelPosition="top"
          >
            <Form.Input field="id" label="ID" disabled={!!poolEditing.id} placeholder="pool_registration_proxy" />
            <Form.Input field="name" label="名称" placeholder="自动注册代理池" />
            <Form.Select field="assignment_strategy" label="分配策略" optionList={[
              { value: 'sticky_least_used', label: 'sticky_least_used' },
            ]} />
          </Form>
        )}
      </Modal>

      <Modal
        title={memberEditing ? `添加成员到 ${memberEditing.id}` : '添加成员'}
        visible={!!memberEditing}
        onCancel={() => {
          if (savingMember) return;
          memberFormApi.current = null;
          setMemberEditing(null);
        }}
        onOk={() => memberFormApi.current?.submitForm?.()}
        confirmLoading={savingMember}
        maskClosable={!savingMember}
        width={520}
      >
        {memberEditing && (
          <Form
            key={memberEditing.id}
            getFormApi={(api) => { memberFormApi.current = api; }}
            initValues={memberEditing}
            onSubmit={saveMember}
            labelPosition="top"
          >
            <Form.Select field="egress_id" label="出口" filter optionList={rows.map((row) => ({
              label: `${row.name || row.id} (${row.type || 'direct'})`,
              value: row.id,
            }))} />
            <Form.Switch field="enabled" label="启用成员" />
            <Form.InputNumber field="capacity" label="容量（0=使用出口并发）" min={0} max={10000} />
          </Form>
        )}
      </Modal>
    </div>
  );
}
