import React, { useState, useCallback, useRef } from 'react';
import { Button, Toast, Tag, Modal, Form } from '@douyinfe/semi-ui';
import { IconRefresh, IconEdit, IconPlus } from '@douyinfe/semi-icons';
import { get, post } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { ActionGroup, MetricRail, TextClamp } from '../components/DisplayPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { loadResourceGroup } from '../lib/resource.js';

// 代理模式常量：决定 cliproxy 取 IP 的方式。
const AUTH_MODES = [
  { value: '', label: '账号密码模式 (credential)', desc: '用户名内嵌 region/sid，网关自动轮换。每次 sid 轮换取新 IP。' },
  { value: 'api_whitelist', label: 'API 白名单模式 (api_whitelist)', desc: '调 api.cliproxy.io/white/api 提取 ip:port，无认证，按 region 锁国家。' },
];

const EMPTY_EGRESS = { profiles: [], pools: [], groups: [], error: null };

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
  const [policyEditing, setPolicyEditing] = useState(null);
  const formApi = useRef(null);
  const poolFormApi = useRef(null);
  const memberFormApi = useRef(null);
  const policyFormApi = useRef(null);

  const fetchRows = useCallback(async ({ signal }) => {
    const { values, error } = await loadResourceGroup({
      profiles: { label: '出口配置', load: () => get('/admin/egress-profiles', undefined, { signal }) },
      pools: { label: '出口池', load: () => get('/admin/egress-pools', undefined, { signal }) },
      groups: { label: '分组', load: () => get('/admin/groups', undefined, { signal }) },
    });
    return {
      profiles: Array.isArray(values.profiles) ? values.profiles : values.profiles?.profiles || values.profiles?.egress_profiles || [],
      pools: Array.isArray(values.pools) ? values.pools : values.pools?.pools || [],
      groups: Array.isArray(values.groups) ? values.groups : values.groups?.groups || [],
      error,
    };
  }, []);
  const { data = EMPTY_EGRESS, loading, error, lastRefresh, reload: load } = useAsyncResource(fetchRows, [fetchRows], { initialData: EMPTY_EGRESS });
  const rows = data.profiles || [];
  const pools = data.pools || [];
  const groups = data.groups || [];
  const healthyCount = rows.filter((row) => !row.health || row.health === 'healthy').length;
  const proxyCount = rows.filter((row) => row.type && row.type !== 'direct').length;
  const concurrencyTotal = rows.reduce((sum, row) => sum + (Number(row.max_concurrency) || 0), 0);
  const egressMetrics = [
    { label: '出口数', value: rows.length },
    { label: '健康出口', value: healthyCount, tone: healthyCount === rows.length ? 'success' : 'warning' },
    { label: '代理出口', value: proxyCount },
    { label: '出口池', value: pools.length },
    { label: '总并发', value: concurrencyTotal },
  ];

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
      // api_whitelist 模式：把 api_base/num/time 塞进 endpoint 让后端解析（或留空由后端默认）。
      // credential 模式：endpoint 即完整代理 URL。
      await post('/admin/egress-profiles', body);
      Toast.success('已保存');
      setEditing(null);
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
      await post('/admin/egress-pools', { ...poolEditing, ...vals });
      Toast.success('出口池已保存');
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

  const loadPolicy = useCallback(async (groupName) => {
    const name = groupName || groups[0]?.name || 'cyber';
    const policy = await get(`/admin/groups/${encodeURIComponent(name)}/egress-policy`);
    setPolicyEditing({
      group_name: name,
      registration_pool_id: policy.registration_pool_id || '',
      runtime_pool_id: policy.runtime_pool_id || '',
      assignment_strategy: policy.assignment_strategy || 'sticky_least_used',
    });
  }, [groups]);

  const { run: savePolicy, running: savingPolicy } = useAsyncAction(async (vals) => {
    try {
      const groupName = vals.group_name || policyEditing?.group_name || groups[0]?.name || 'cyber';
      await post(`/admin/groups/${encodeURIComponent(groupName)}/egress-policy`, {
        registration_pool_id: vals.registration_pool_id || '',
        runtime_pool_id: vals.runtime_pool_id || '',
        assignment_strategy: vals.assignment_strategy || 'sticky_least_used',
      });
      Toast.success('分组出口策略已保存');
      setPolicyEditing(null);
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
  const poolColumns = [
    {
      title: '出口池',
      key: 'pool',
      width: 260,
      render: (_, row) => (
        <div className="pool-resource-summary">
          <TextClamp strong>{row.name || row.id}</TextClamp>
          <div className="pool-resource-summary__meta">
            <Tag size="small" color={row.purpose === 'registration' ? 'orange' : row.purpose === 'runtime' ? 'blue' : 'grey'}>{row.purpose || 'custom'}</Tag>
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
        <ActionGroup minWidth={140} compact>
          <Button size="small" icon={<IconEdit />} onClick={() => setPoolEditing({ ...row })}>编辑</Button>
          <Button size="small" icon={<IconPlus />} onClick={() => setMemberEditing({ id: row.id, egress_id: '', enabled: true, capacity: 0 })}>成员</Button>
        </ActionGroup>
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

  const isApiMode = editing?.proxy_auth_mode === 'api_whitelist';

  return (
    <div>
      <PageHeader title="出口 / 代理" subtitle={`共 ${rows.length} 个出口 · curl_cffi sidecar / 住宅代理 / WARP / CLIPProxy`}
        actions={<>
          <Button icon={<IconEdit />} onClick={() => loadPolicy(groups[0]?.name || 'cyber')}>分组策略</Button>
          <Button icon={<IconPlus />} onClick={() => setPoolEditing({ id: '', name: '', purpose: 'runtime', assignment_strategy: 'sticky_least_used' })}>新建出口池</Button>
          <Button icon={<IconPlus />} onClick={() => openEdit(null)}>新建</Button>
          <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
        </>} />
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
        dataSource={pools}
        columns={poolColumns}
        rowKey="id"
        pagination={{ pageSize: 12 }}
        className="pool-egress-pools-table"
        density="compact"
        layout="fit"
        scroll={false}
        rowHeight={64}
        emptyTitle="暂无出口池"
        emptyDesc="创建 registration/runtime 池后，注册任务和账号绑定会按策略分配出口"
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
          <Form
            key={editing.id || 'new'}
            getFormApi={(api) => { formApi.current = api; }}
            initValues={editing}
            onSubmit={save}
            labelPosition="top"
          >
            <Form.Input field="id" label="ID" disabled={!!editing.id} placeholder="egress_xxx" />
            <Form.Input field="name" label="名称" placeholder="cliproxy BR residential" />
            <Form.Select field="type" label="类型" optionList={[
              { value: 'direct', label: 'direct (直连)' },
              { value: 'http_proxy', label: 'http_proxy (HTTP 代理)' },
              { value: 'https_proxy', label: 'https_proxy' },
              { value: 'socks5_proxy', label: 'socks5_proxy' },
              { value: 'socks5h_proxy', label: 'socks5h_proxy' },
              { value: 'curl_cffi_sidecar', label: 'curl_cffi_sidecar (JA3 伪装)' },
            ]} />
            <Form.Select
              field="proxy_auth_mode"
              label="代理模式（CLIPProxy 取 IP 方式）"
              optionList={AUTH_MODES.map(m => ({ value: m.value, label: m.label }))}
              help={AUTH_MODES.find(m => m.value === (editing.proxy_auth_mode || ''))?.desc}
            />

            {!isApiMode && (
              <Form.Input
                field="endpoint"
                label="Endpoint（账号密码模式）"
                placeholder="http://user-region-BR-sid-XXXX-t-5:pass@host:port"
                help="credential 模式：完整代理 URL，region/sid 在用户名里。"
              />
            )}
            {isApiMode && (
              <>
                <Form.Input field="api_base" label="CLIPProxy API Base URL" placeholder="https://api.cliproxy.io" />
                <Form.Input field="proxy_api_key" mode="password" label="CLIPProxy API Key" placeholder="账户 API token（调 /white/api 鉴权）" />
                <Form.InputNumber field="api_num" label="提取数量 (num)" min={1} max={10} />
                <Form.InputNumber field="api_time" label="轮转时长 (time, 分钟)" min={1} max={60} />
              </>
            )}

            <Form.Input field="region" label="地区/国家 (ISO-2 或 Rand)" placeholder="BR / US / Rand" help="号码国家锁定：BR 号码必须配 region-BR 代理。" />
            <Form.Input field="exit_ip" label="出口 IP（可选，自动检测填充）" />
            <Form.Input field="chain_proxy" label="Chain Proxy（上游，可选）" placeholder="socks5h://127.0.0.1:40000" />
            <Form.Select field="ip_mode" label="IP 模式" optionList={[
              { value: 'static_residential', label: '静态住宅 IP' },
              { value: 'dynamic_residential', label: '动态住宅 IP' },
              { value: 'datacenter', label: '机房 / 普通代理' },
              { value: 'local_sidecar', label: '本地 sidecar / cuff' },
            ]} />
            <Form.Input field="provider_key" label="服务商标识" placeholder="cliproxy / cuff / warp" />
            <Form.TextArea field="dynamic_config_json" label="动态代理配置 JSON" autosize placeholder='{ "rotation": "sid" }' />
            <Form.InputNumber field="max_concurrency" label="并发上限" min={1} max={128} />
          </Form>
        )}
      </Modal>

      <Modal
        title={poolEditing?.id ? `编辑出口池 ${poolEditing.id}` : '新建出口池'}
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
            <Form.Input field="id" label="ID" disabled={!!poolEditing.id} placeholder="pool_runtime_cuff" />
            <Form.Input field="name" label="名称" placeholder="运行期 cuff 出口池" />
            <Form.Select field="purpose" label="用途" optionList={[
              { value: 'runtime', label: 'runtime - 账号运行出口' },
              { value: 'registration', label: 'registration - 注册代理池' },
              { value: 'custom', label: 'custom' },
            ]} />
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

      <Modal
        title="分组出口策略"
        visible={!!policyEditing}
        onCancel={() => {
          if (savingPolicy) return;
          policyFormApi.current = null;
          setPolicyEditing(null);
        }}
        onOk={() => policyFormApi.current?.submitForm?.()}
        confirmLoading={savingPolicy}
        maskClosable={!savingPolicy}
        width={560}
      >
        {policyEditing && (
          <Form
            key={policyEditing.group_name}
            getFormApi={(api) => { policyFormApi.current = api; }}
            initValues={policyEditing}
            onSubmit={savePolicy}
            labelPosition="top"
          >
            <Form.Select field="group_name" label="分组" optionList={(groups.length ? groups : [{ name: 'cyber' }]).map((group) => ({
              label: group.name,
              value: group.name,
            }))} onChange={(v) => loadPolicy(v)} />
            <Form.Select field="registration_pool_id" label="注册代理池" optionList={[
              { label: '未设置', value: '' },
              ...pools.filter((p) => !p.purpose || p.purpose === 'registration').map((p) => ({ label: p.name || p.id, value: p.id })),
            ]} />
            <Form.Select field="runtime_pool_id" label="注册后账号出口池" optionList={[
              { label: '未设置', value: '' },
              ...pools.filter((p) => !p.purpose || p.purpose === 'runtime').map((p) => ({ label: p.name || p.id, value: p.id })),
            ]} />
            <Form.Select field="assignment_strategy" label="分配策略" optionList={[
              { label: 'sticky_least_used', value: 'sticky_least_used' },
            ]} />
          </Form>
        )}
      </Modal>
    </div>
  );
}
