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
import CopyCodeBlock from '../components/CopyCodeBlock.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { loadResourceGroup } from '../lib/resource.js';

// 代理模式常量：决定 cliproxy 取 IP 的方式。
// `short` is what the table cell shows. It used to be derived by regex from `label`, which
// pulled out the parenthetical — the wire value, not a name a reader recognises — so the
// column read "credential" / "api_whitelist" in a UI that is otherwise Chinese throughout.
const AUTH_MODES = [
  { value: '', short: '账号密码', label: '账号密码模式 (credential)', desc: '用户名内嵌 region/sid，网关自动轮换。每次 sid 轮换取新 IP。' },
  { value: 'api_whitelist', short: 'API 白名单', label: 'API 白名单模式 (api_whitelist)', desc: '调 api.cliproxy.io/white/api 提取 ip:port，无认证，按 region 锁国家。' },
];

// storage.normalizeEgressPoolStrategy accepts any non-empty string and defaults to
// sticky_least_used, so this maps the one value it names and passes anything else through
// rather than hiding a strategy the backend accepted behind a missing translation.
const STRATEGY_LABELS = { sticky_least_used: '粘滞 · 最少占用' };

const EMPTY_EGRESS = { profiles: [], pools: [], config: [], error: null };

const RESIDENTIAL_QUICKSTART = `export POOL_URL='https://POOL_HOST'
export ADMIN_TOKEN='ADMIN_TOKEN'
read -rsp 'Residential proxy URL: ' RESIDENTIAL_PROXY_URL; echo

curl -fsS -X POST "$POOL_URL/admin/egress-profiles" \\
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \\
  --data "$(jq -n --arg endpoint "$RESIDENTIAL_PROXY_URL" '{
    id: "egress_residential_registration", name: "Registration residential",
    type: "http_proxy", endpoint: $endpoint, region: "US",
    ip_mode: "dynamic_residential", provider_key: "residential",
    max_concurrency: 1, detect_region: true
  }')"`;

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
  // Mirrors scheduler.EgressHealthy: an egress is schedulable unless it is disabled
  // or still cooling down. "健康" would be the wrong word for a tripped egress the
  // scheduler will still pick, so the rail reports schedulability instead.
  const now = Math.floor(Date.now() / 1000);
  const schedulableCount = rows.filter((row) => {
    if (Number(row.cooldown_until) > now) return false;
    return ['', 'healthy', 'cooldown', 'tripped'].includes(String(row.health || ''));
  }).length;
  const proxyCount = rows.filter((row) => row.type && row.type !== 'direct').length;
  const concurrencyTotal = rows.reduce((sum, row) => sum + (Number(row.max_concurrency) || 0), 0);
  const share = (part) => (rows.length ? part / rows.length : 0);
  const egressMetrics = [
    { label: '出口', value: rows.length },
    {
      label: '可调度',
      value: schedulableCount,
      share: share(schedulableCount),
      tone: schedulableCount === rows.length ? 'success' : 'warning',
    },
    { label: '走代理', value: proxyCount, share: share(proxyCount), tone: 'info' },
    { label: '注册池', value: registrationPools.length },
    { label: '并发上限', value: concurrencyTotal },
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

  // storage.EgressProfile.ProxyAuthMode only describes how a proxy's IPs are obtained, so
  // it says nothing about an egress that has no proxy endpoint. Those used to read
  // "credential", which is a claim about credentials the egress does not use.
  const renderMode = (row) => {
    if (!row.endpoint && !row.chain_proxy) return <span className="pool-muted">—</span>;
    const mode = AUTH_MODES.find((entry) => entry.value === (row.proxy_auth_mode || ''));
    return (
      <Tag color={row.proxy_auth_mode === 'api_whitelist' ? 'blue' : 'grey'} title={mode?.desc}>
        {mode ? mode.short : String(row.proxy_auth_mode)}
      </Tag>
    );
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
      key: 'proxy_auth_mode',
      width: 140,
      render: (_, row) => renderMode(row),
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
          {/* The table is titled 注册池 and only lists pools whose purpose is registration,
              so a `registration` tag on every row restated the heading four times over. */}
          <div className="pool-resource-summary__meta"><span>{row.id}</span></div>
        </div>
      ),
    },
    {
      title: '策略',
      dataIndex: 'assignment_strategy',
      width: 160,
      render: (v) => {
        const strategy = v || 'sticky_least_used';
        return <Tag color="blue" title={strategy}>{STRATEGY_LABELS[strategy] || strategy}</Tag>;
      },
    },
    {
      title: '成员',
      key: 'members',
      width: 320,
      render: (_, row) => (
        <div className="pool-resource-summary">
          {/* A member whose egress has neither name nor id contributed an empty string,
              so a pool of two unnamed members rendered as a bare ", ". */}
          <TextClamp>{(row.members || []).map((m) => m.egress?.name || m.egress_id).filter(Boolean).join(' · ') || '未添加成员'}</TextClamp>
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
            { label: '代理模式', value: renderMode(row) },
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
      <section className="pool-egress-quickstart">
        <div>
          <Tag color="blue">复制粘贴</Tag>
          <h2>住宅 IP 最短接入</h2>
          <p>替换地址与 Token，粘贴完整代理 URL。保存后在本页测试出口 IP，再点“加入默认注册池”。动态住宅注册建议并发为 1。</p>
        </div>
        <CopyCodeBlock code={RESIDENTIAL_QUICKSTART} label="复制住宅代理命令" />
      </section>
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
          scroll={false}
          rowHeight={64}
          emptyTitle="暂无出口配置"
          emptyType="egress"
          skeletonRows={6}
        />
        {!error || lastRefresh ? <MetricRail items={egressMetrics} /> : null}
      </div>
      {/* The default-pool setting only means anything in terms of the pools listed below,
          so it rides in this table's heading instead of a toolbar of its own. */}
      <section className="pool-egress-pools">
        <div className="pool-section-heading">
          <div>
            <span>注册出口</span>
            <h3>注册池</h3>
          </div>
          <div className="pool-section-heading__controls">
            <label htmlFor="egress-default-pool">默认池</label>
            <Select
              id="egress-default-pool"
              value={registrationPoolDraft}
              onChange={setRegistrationPoolDraft}
              optionList={[
                { label: '未设置', value: '' },
                ...(registrationPools || []).map((pool) => ({ label: `${pool.name || pool.id} (${pool.members?.length || 0})`, value: pool.id })),
              ]}
              style={{ width: 220 }}
            />
            <Button
              size="small"
              loading={savingRegistrationPool}
              disabled={registrationPoolDraft === registrationPoolSetting}
              onClick={saveRegistrationPool}
            >保存</Button>
          </div>
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
          scroll={false}
          rowHeight={64}
          emptyTitle="暂无注册池"
          emptyDesc="创建注册池后，注册任务可从池内选择代理出口"
          emptyType="egress"
          skeletonRows={4}
        />
      </section>

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
              { value: 'sticky_least_used', label: STRATEGY_LABELS.sticky_least_used },
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
