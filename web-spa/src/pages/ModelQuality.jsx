import React, { useCallback, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { Button, Tag, Toast, Typography } from '../components/pool/index.jsx';
import { IconPlay, IconRefresh, IconSetting } from '../components/pool/icons.jsx';
import { errMsg, get, post } from '../api.js';
import PageHeader, { Panel } from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import StatCard from '../components/StatCard.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { fmtDateTime, fmtInt, fmtRelative, fmtTokens } from '../lib/format.js';

const STATE_META = {
  healthy: { label: '正常', color: 'green' },
  suspect: { label: '疑似降智', color: 'amber' },
  degraded: { label: '确认降智', color: 'red' },
  unavailable: { label: '暂不可用', color: 'grey' },
  unknown: { label: '未检测', color: 'grey' },
};

const OUTCOME_META = {
  pass: { label: '通过', color: 'green' },
  anomaly: { label: '异常', color: 'red' },
  error: { label: '请求错误', color: 'grey' },
};

function stateTag(state) {
  const meta = STATE_META[state] || { label: state || '未知', color: 'grey' };
  return <Tag color={meta.color}>{meta.label}</Tag>;
}

function outcomeTag(outcome) {
  const meta = OUTCOME_META[outcome] || { label: outcome || '—', color: 'grey' };
  return outcome ? <Tag color={meta.color}>{meta.label}</Tag> : '—';
}

function qualityKey(row) {
  return `${row.group_name || ''}:${row.model || ''}:${row.provider || ''}`;
}

function answerSummary(row) {
  if (!row.last_expected && !row.last_actual) return '—';
  return `${row.last_actual || '∅'} / ${row.last_expected || '∅'}`;
}

export default function ModelQuality() {
  const navigate = useNavigate();
  const [runningKey, setRunningKey] = useState('');
  const fetchQuality = useCallback(async ({ signal }) => get('/admin/model-quality', { limit: 200 }, { signal }), []);
  const {
    data = {},
    loading,
    error,
    lastRefresh,
    reload,
  } = useAsyncResource(fetchQuality, [fetchQuality], { initialData: {} });

  const statuses = Array.isArray(data?.statuses) ? data.statuses : [];
  const runs = Array.isArray(data?.runs) ? data.runs : [];
  const summary = useMemo(() => statuses.reduce((acc, row) => {
    const state = row.state || 'unknown';
    acc[state] = (acc[state] || 0) + 1;
    return acc;
  }, {}), [statuses]);

  const runOne = async (row) => {
    const key = qualityKey(row);
    setRunningKey(key);
    try {
      const result = await post('/admin/model-quality/run', {
        group_name: row.group_name,
        model: row.model,
        provider: row.provider,
      }, { timeout: 180000 });
      Toast.success(result?.checked ? '分组模型检测完成' : '没有匹配的分组模型');
      await reload();
    } catch (e) {
      Toast.error(`检测失败：${errMsg(e)}`);
    } finally {
      setRunningKey('');
    }
  };

  const statusColumns = [
    { title: '分组', dataIndex: 'group_name', width: 130, render: (v) => <b>{v || '默认分组'}</b> },
    { title: '模型', dataIndex: 'model', width: 210, render: (v) => <span className="pool-mono">{v || '—'}</span> },
    { title: '提供方', dataIndex: 'provider', width: 100, render: (v) => v ? <Tag>{v}</Tag> : '—' },
    { title: '智力状态', dataIndex: 'state', width: 118, render: stateTag },
    { title: '最近结果', dataIndex: 'last_outcome', width: 110, render: outcomeTag },
    { title: '连续异常', dataIndex: 'consecutive_anomalies', width: 105, sorter: (a, b) => (a.consecutive_anomalies || 0) - (b.consecutive_anomalies || 0), render: (v, r) => `${v || 0} / ${data?.degraded_threshold || 2}` },
    { title: '答案 / 标准', key: 'answer', width: 160, render: (_, r) => <span className="pool-mono" title={answerSummary(r)}>{answerSummary(r)}</span> },
    { title: '返回模型', dataIndex: 'last_returned_model', width: 190, render: (v) => v ? <span className="pool-mono">{v}</span> : '—' },
    { title: '累计 Token', dataIndex: 'total_tokens', width: 105, sorter: (a, b) => (a.total_tokens || 0) - (b.total_tokens || 0), render: (v) => v ? fmtTokens(v) : '—' },
    { title: '延迟', dataIndex: 'last_latency_ms', width: 90, render: (v) => v ? `${v} ms` : '—' },
    { title: '最近检测', dataIndex: 'last_probe_at', width: 145, sorter: (a, b) => (a.last_probe_at || 0) - (b.last_probe_at || 0), render: (v) => v ? <span title={fmtDateTime(v)}>{fmtRelative(v)}</span> : '从未' },
    { title: '操作', key: 'actions', width: 125, render: (_, row) => (
      <Button
        size="small"
        icon={<IconPlay />}
        loading={runningKey === qualityKey(row)}
        disabled={Boolean(runningKey) || Boolean(data?.running)}
        onClick={() => runOne(row)}
      >立即检测</Button>
    ) },
  ];

  const runColumns = [
    { title: '时间', dataIndex: 'created_at', width: 145, render: (v) => <span title={fmtDateTime(v)}>{fmtRelative(v)}</span> },
    { title: '分组', dataIndex: 'group_name', width: 120, render: (v) => v || '默认分组' },
    { title: '模型', dataIndex: 'model', width: 190, render: (v) => <span className="pool-mono">{v}</span> },
    { title: '阶段', dataIndex: 'phase', width: 100, render: (v) => <Tag>{v === 'confirmation' ? '异常复核' : '主检测'}</Tag> },
    { title: '结果', dataIndex: 'outcome', width: 105, render: outcomeTag },
    { title: '答案 / 标准', key: 'answer', width: 160, render: (_, r) => <span className="pool-mono">{`${r.actual || '∅'} / ${r.expected || '∅'}`}</span> },
    { title: '返回模型', dataIndex: 'returned_model', width: 190, render: (v) => v ? <span className="pool-mono">{v}</span> : '—' },
    { title: 'Token', dataIndex: 'total_tokens', width: 85, render: (v) => fmtTokens(v) },
    { title: '延迟', dataIndex: 'latency_ms', width: 90, render: (v) => `${v || 0} ms` },
    { title: '错误', dataIndex: 'error_kind', width: 130, render: (v, r) => v ? <Typography.Text title={r.error_message || v}>{v}</Typography.Text> : '—' },
  ];

  const checked = statuses.length - (summary.unknown || 0);
  return (
    <div>
      <PageHeader
        title="模型智商 / 降智检测"
        subtitle="每个分组 × 模型每小时只抽样一次；主检测异常时才追加独立复核，不逐账号消耗 Token"
        actions={<>
          <Button icon={<IconSetting />} onClick={() => navigate('/settings-v2')}>检测设置</Button>
          <Button icon={<IconRefresh />} loading={loading} onClick={reload}>刷新</Button>
        </>}
      />

      <div className="pool-stat-grid" style={{ marginBottom: 18 }}>
        <StatCard label="监控状态" value={data?.enabled ? '已启用' : '未启用'} color={data?.enabled ? 'var(--pool-success)' : 'var(--pool-warning)'} sub={`${data?.interval_minutes || 60} 分钟一次 · ${data?.reasoning_effort || 'medium'} reasoning`} />
        <StatCard label="检测范围" value={`${fmtInt(checked)} / ${fmtInt(statuses.length)}`} color="var(--pool-accent)" sub="已检测 / 分组模型组合" />
        <StatCard label="疑似异常" value={fmtInt(summary.suspect || 0)} color="var(--pool-warning)" sub="需下一轮再次确认" />
        <StatCard label="确认降智" value={fmtInt(summary.degraded || 0)} color="var(--pool-danger)" sub={`连续 ${data?.degraded_threshold || 2} 轮复核异常`} />
      </div>

      <Panel title="分组 × 模型状态" extra={<Typography.Text type="tertiary" size="small">网络错误只标记不可用，不计为降智</Typography.Text>}>
        <ResourceTable
          error={error}
          onRetry={reload}
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={statuses}
          columns={statusColumns}
          rowKey={qualityKey}
          pagination={{ pageSize: 20 }}
          layout="fit"
          emptyTitle="暂无可检测的分组模型"
          emptyDesc="请先添加活跃账号及其模型能力；启用定时检测后会自动生成状态。"
          skeletonRows={6}
          skeletonCols={12}
        />
      </Panel>

      <Panel title="最近检测记录" style={{ marginTop: 18 }}>
        <ResourceTable
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={runs}
          columns={runColumns}
          rowKey="id"
          pagination={{ pageSize: 20 }}
          layout="fit"
          emptyTitle="暂无检测记录"
          skeletonRows={6}
          skeletonCols={10}
        />
      </Panel>
    </div>
  );
}
