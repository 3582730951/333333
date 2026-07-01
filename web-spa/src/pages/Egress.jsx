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

// 代理模式常量：决定 cliproxy 取 IP 的方式。
const AUTH_MODES = [
  { value: '', label: '账号密码模式 (credential)', desc: '用户名内嵌 region/sid，网关自动轮换。每次 sid 轮换取新 IP。' },
  { value: 'api_whitelist', label: 'API 白名单模式 (api_whitelist)', desc: '调 api.cliproxy.io/white/api 提取 ip:port，无认证，按 region 锁国家。' },
];

export default function Egress() {
  const [editing, setEditing] = useState(null); // null | {} (new) | existing row
  const formApi = useRef(null);

  const fetchRows = useCallback(async ({ signal }) => {
    const e = await get('/admin/egress-profiles', undefined, { signal });
    return Array.isArray(e) ? e : e?.profiles || e?.egress_profiles || [];
  }, []);
  const { data: rows = [], loading, error, lastRefresh, reload: load } = useAsyncResource(fetchRows, [fetchRows], { initialData: [] });
  const healthyCount = rows.filter((row) => !row.health || row.health === 'healthy').length;
  const proxyCount = rows.filter((row) => row.type && row.type !== 'direct').length;
  const concurrencyTotal = rows.reduce((sum, row) => sum + (Number(row.max_concurrency) || 0), 0);
  const egressMetrics = [
    { label: '出口数', value: rows.length },
    { label: '健康出口', value: healthyCount, tone: healthyCount === rows.length ? 'success' : 'warning' },
    { label: '代理出口', value: proxyCount },
    { label: '总并发', value: concurrencyTotal },
  ];

  const openEdit = (row) => {
    setEditing(row ? { ...row } : {
      id: '', name: '', type: 'http_proxy', endpoint: '', region: '', exit_ip: '',
      chain_proxy: '', max_concurrency: 16, proxy_auth_mode: '', proxy_api_key: '',
      api_base: 'https://api.cliproxy.io', api_num: 1, api_time: 10,
    });
  };

  const { run: save, running: saving } = useAsyncAction(async (vals) => {
    try {
      const body = { ...editing, ...vals };
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
            <Form.InputNumber field="max_concurrency" label="并发上限" min={1} max={128} />
          </Form>
        )}
      </Modal>
    </div>
  );
}
