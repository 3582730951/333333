import React, { useCallback, useMemo, useState } from 'react';
import { useNavigate } from 'react-router';
import { Button, Drawer, Tag, Toast, Typography } from '../components/pool/index.jsx';
import { IconPlay, IconRefresh, IconSetting } from '../components/pool/icons.jsx';
import { errMsg, get, post } from '../api.js';
import PageHeader, { Panel } from '../components/PageHeader.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import * as MicroCharts from '../components/MicroCharts.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { COLORS } from '../lib/chartTheme.js';
import { fmtDateTime, fmtInt, fmtRelative, fmtTokens } from '../lib/format.js';
import { heatCells, hourlyBuckets } from '../lib/timeSeries.js';

const { HeatStrip, RadialGauge, RankedBars, StackedMeter } = MicroCharts;

const STATE_META = {
  healthy: { label: '正常', color: 'green' },
  suspect: { label: '疑似降智', color: 'amber' },
  degraded: { label: '确认降智', color: 'red' },
  unavailable: { label: '暂不可用', color: 'grey' },
  unknown: { label: '未检测', color: 'grey' },
};

// The backend emits seven distinct outcome values across two enums, and this map used to
// cover three of them. `statuses[].last_outcome` is one of pass / false_alarm / error /
// inconclusive / confirmed_anomaly; `runs[].outcome` is one of pass / error /
// model_mismatch / incorrect. The four unmapped values fell through to the raw identifier
// rendered in neutral grey -- so a confirmed_anomaly row, the one state that means the model
// really is degraded, showed the English enum name in the same colour as a healthy one.
// `anomaly` was in this map but is never emitted by the API at all.
const OUTCOME_META = {
  pass: { label: '通过', color: 'green' },
  false_alarm: { label: '复核通过', color: 'green' },
  confirmed_anomaly: { label: '复核异常', color: 'red' },
  incorrect: { label: '答案错误', color: 'red' },
  model_mismatch: { label: '模型不符', color: 'amber' },
  inconclusive: { label: '复核失败', color: 'grey' },
  error: { label: '请求错误', color: 'grey' },
};

// Chart grouping for the same seven values. Kept separate from the tag metadata because the
// charts need three buckets, not seven colours: a false_alarm is a pass for rate purposes,
// and an inconclusive is an error because nothing was actually measured.
const OUTCOME_BUCKET = {
  pass: 'pass',
  false_alarm: 'pass',
  confirmed_anomaly: 'anomaly',
  incorrect: 'anomaly',
  model_mismatch: 'anomaly',
  inconclusive: 'error',
  error: 'error',
};

const STATE_COLOR = {
  healthy: COLORS.green,
  suspect: COLORS.amber,
  degraded: COLORS.red,
  unavailable: COLORS.grey,
};

// Composition order for the health meter, best state first. `unknown` is deliberately absent:
// it is the absence of a measurement rather than a health state, so it would need a fifth
// colour that means "no data" next to four that mean something. It is reported as a count
// under the meter instead.
const STATE_ORDER = ['healthy', 'suspect', 'degraded', 'unavailable'];

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

function modelFingerprintLabel(row) {
  return row.model_fingerprint || (row.metadata_probe_at ? '模型未返回身份' : '尚未询问');
}

function knowledgeBaseLabel(row) {
  return row.knowledge_base_updated_at || (row.metadata_probe_at ? '模型回答未知' : '尚未询问');
}

function QualityStatusDetails({ row, threshold }) {
  const details = [
    ['分组', row.group_name || '默认分组'],
    ['模型', <span className="pool-mono">{row.model || '—'}</span>],
    ['提供方', row.provider ? <Tag>{row.provider}</Tag> : '—'],
    ['智力状态', stateTag(row.state)],
    ['最近结果', outcomeTag(row.last_outcome)],
    ['连续异常', `${row.consecutive_anomalies || 0} / ${threshold}`],
    ['答案 / 标准', <span className="pool-mono">{answerSummary(row)}</span>],
    ['返回模型', row.last_returned_model ? <span className="pool-mono">{row.last_returned_model}</span> : '—'],
    ['模型指纹', <span className="pool-mono" title={row.model_fingerprint_source || ''}>{modelFingerprintLabel(row)}</span>],
    ['知识库更新时间', <span title={row.knowledge_base_source || ''}>{knowledgeBaseLabel(row)}</span>],
    ['模型询问时间', row.metadata_probe_at ? fmtDateTime(row.metadata_probe_at) : '尚未询问'],
    ['目录观测', row.catalog_observed_at ? fmtDateTime(row.catalog_observed_at) : '—'],
    ['累计 Token', row.total_tokens ? fmtTokens(row.total_tokens) : '—'],
    ['延迟', row.last_latency_ms ? `${row.last_latency_ms} ms` : '—'],
    ['最近检测', row.last_probe_at ? <span title={fmtDateTime(row.last_probe_at)}>{fmtRelative(row.last_probe_at)}</span> : '从未'],
  ];
  return (
    <div className="pool-task-detail">
      <dl className="pool-task-detail__grid">
        {details.map(([label, value]) => (
          <div key={label}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}

export default function ModelQuality() {
  const navigate = useNavigate();
  const [runningKey, setRunningKey] = useState('');
  const [selectedStatus, setSelectedStatus] = useState(null);
  const [statusPage, setStatusPage] = useState(1);
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

  // Everything below is derived client-side from the two arrays the endpoint already
  // returns. No backend change was needed to visualise any of it.
  const overview = useMemo(() => {
    const buckets = { pass: 0, anomaly: 0, error: 0 };
    for (const run of runs) {
      const bucket = OUTCOME_BUCKET[run.outcome];
      if (bucket) buckets[bucket] += 1;
    }
    const measured = buckets.pass + buckets.anomaly;
    // Errors are excluded from the denominator on purpose: a request that never reached the
    // model is not evidence either way about that model's quality, and the page already
    // states that network errors only mark a model unavailable.
    const passRate = measured > 0 ? buckets.pass / measured : null;

    const latency = statuses
      .filter((row) => Number(row.last_latency_ms) > 0)
      .sort((a, b) => Number(b.last_latency_ms) - Number(a.last_latency_ms))
      .slice(0, 7)
      .map((row) => ({
        key: qualityKey(row),
        name: row.model || '—',
        value: Number(row.last_latency_ms),
        color: STATE_COLOR[row.state] || COLORS.grey,
        meta: `${row.group_name || '默认分组'} · ${(STATE_META[row.state] || {}).label || '未检测'}`,
      }));

    // Anomalies and total volume share one time axis so a burst of anomalies confined to one
    // hour (an upstream incident) reads differently from anomalies spread evenly across the day
    // (a model actually degrading).
    const buckets24 = hourlyBuckets(runs, {
      timeOf: (run) => run.created_at,
      series: {
        volume: () => true,
        anomalies: (run) => OUTCOME_BUCKET[run.outcome] === 'anomaly',
      },
    });
    const timeline = buckets24 && {
      volume: heatCells(buckets24.counts.volume, buckets24.slots, '次检测'),
      anomalies: heatCells(buckets24.counts.anomalies, buckets24.slots, '次异常'),
      from: fmtDateTime(buckets24.from),
      to: fmtDateTime(buckets24.to),
      anomalyTotal: buckets24.totals.anomalies,
    };

    return { buckets, measured, passRate, latency, timeline };
  }, [runs, statuses]);

  if (error && !lastRefresh && !loading) {
    return (
      <div>
        <PageHeader title="模型质量" subtitle="模型可用性与质量检测" actions={<Button icon={<IconRefresh />} onClick={reload}>重试</Button>} />
        <LoadErrorBanner error={error} onRetry={reload} title="模型质量数据读取失败" />
      </div>
    );
  }

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
    { title: '模型指纹', dataIndex: 'model_fingerprint', width: 170, priority: 10, render: (_, r) => <span className="pool-mono" title={r.model_fingerprint_source || ''}>{modelFingerprintLabel(r)}</span> },
    { title: '知识库更新时间', dataIndex: 'knowledge_base_updated_at', width: 150, priority: 10, render: (_, r) => <span title={r.knowledge_base_source || ''}>{knowledgeBaseLabel(r)}</span> },
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

  const degradedThreshold = data?.degraded_threshold || 2;
  const renderMobileStatus = (row) => (
    <MobileResourceCell
      title={<span className="pool-mono">{row.model || '—'}</span>}
      subtitle={row.group_name || '默认分组'}
      badges={stateTag(row.state)}
      chips={row.provider ? <Tag>{row.provider}</Tag> : null}
      details={[
        { label: '最近结果', value: outcomeTag(row.last_outcome) },
        { label: '指纹', value: modelFingerprintLabel(row) },
        { label: '知识库', value: knowledgeBaseLabel(row) },
        { label: '模型询问', value: row.metadata_probe_at ? fmtRelative(row.metadata_probe_at) : '尚未询问' },
        { label: '连续异常', value: `${row.consecutive_anomalies || 0} / ${degradedThreshold}` },
        { label: '最近检测', value: row.last_probe_at ? fmtRelative(row.last_probe_at) : '从未检测' },
      ]}
      actions={(
        <Button
          size="small"
          aria-label={`查看 ${row.model || '模型'} 详情`}
          onClick={() => setSelectedStatus(row)}
        >
          详情
        </Button>
      )}
    />
  );

  const checked = statuses.length - (summary.unknown || 0);
  return (
    <div>
      <PageHeader
        title="模型智商 / 降智检测"
        subtitle="每个分组 × 模型每轮只抽样一条短题；GPT、Claude、GPT-6 使用分族题库，异常才追加独立复核"
        actions={<>
          <Button icon={<IconSetting />} onClick={() => navigate('/settings-v2')}>检测设置</Button>
          <Button icon={<IconRefresh />} loading={loading} onClick={reload}>刷新</Button>
        </>}
      />

      <Panel title={`短题探针 ${data?.suite_version || 'family-short-v2'}`} style={{ marginBottom: 18 }}>
        <Typography.Text type="tertiary">
          {data?.suite_policy || '短题只验证可重复的行为下限、答案一致性和返回模型身份；行为结果不能单独证明代理使用官方权重。'}
        </Typography.Text>
        {Array.isArray(data?.probe_catalog) ? (
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginTop: 10 }}>
            {data.probe_catalog.map((item) => (
              <Tag key={item.family}>{`${item.family} · ${(item.categories || []).join(' / ')}`}</Tag>
            ))}
          </div>
        ) : null}
      </Panel>

      {statuses.length ? (
        <section className="pool-mq-overview">
          <div className="pool-chart-card pool-mq-overview__health">
            <div className="head">
              <div>
                <div className="t">检测健康度</div>
                <div className="s">{`${data?.enabled ? '已启用' : '未启用'} · ${data?.interval_minutes || 60} 分钟一次 · 连续 ${degradedThreshold} 轮异常判定降智`}</div>
              </div>
            </div>
            <div className="pool-mq-overview__health-body">
              <RadialGauge
                value={overview.passRate ?? 0}
                size={128}
                color={overview.passRate == null ? COLORS.grey : overview.passRate >= 0.9 ? COLORS.green : overview.passRate >= 0.7 ? COLORS.amber : COLORS.red}
                valueText={overview.passRate == null ? '—' : undefined}
                caption="近期通过率"
                label={`${fmtInt(overview.measured)} 次有效检测`}
              />
              <div className="pool-mq-overview__states">
                <StackedMeter
                  segments={STATE_ORDER.map((state) => ({
                    key: state,
                    name: STATE_META[state].label,
                    value: summary[state] || 0,
                    color: STATE_COLOR[state],
                  }))}
                  valueFormatter={fmtInt}
                  ariaLabel="分组模型健康状态构成"
                />
                <p className="pool-mq-overview__note">
                  {summary.unknown
                    ? `${fmtInt(checked)} / ${fmtInt(statuses.length)} 个分组模型已检测，${fmtInt(summary.unknown)} 个待首轮抽样`
                    : `全部 ${fmtInt(statuses.length)} 个分组模型均已检测`}
                </p>
              </div>
            </div>
          </div>

          <div className="pool-chart-card pool-mq-overview__latency">
            <div className="head">
              <div>
                <div className="t">响应延迟对比</div>
                <div className="s">最近一次检测耗时，按状态着色</div>
              </div>
            </div>
            <RankedBars
              rows={overview.latency}
              valueFormatter={(value) => `${fmtInt(value)} ms`}
              emptyText="尚无延迟样本"
              ariaLabel="分组模型响应延迟对比"
            />
          </div>
        </section>
      ) : null}

      {overview.timeline ? (
        <section className="pool-chart-card pool-mq-timeline">
          <div className="head">
            <div>
              <div className="t">近 24 小时检测节奏</div>
              <div className="s">
                {overview.timeline.anomalyTotal
                  ? `共 ${fmtInt(overview.timeline.anomalyTotal)} 次异常，集中程度可判断是上游波动还是模型降智`
                  : '窗口内没有异常记录'}
              </div>
            </div>
          </div>
          <div className="pool-mq-timeline__rows">
            <div className="pool-mq-timeline__row">
              <span className="pool-mq-timeline__label">检测量</span>
              <HeatStrip cells={overview.timeline.volume} color={COLORS.blue} ariaLabel="近 24 小时每小时检测次数" />
            </div>
            <div className="pool-mq-timeline__row">
              <span className="pool-mq-timeline__label">异常量</span>
              <HeatStrip cells={overview.timeline.anomalies} color={COLORS.red} ariaLabel="近 24 小时每小时异常次数" />
            </div>
          </div>
          <div className="pool-mq-timeline__axis">
            <span>{overview.timeline.from}</span>
            <span>{overview.timeline.to}</span>
          </div>
        </section>
      ) : null}

      <Panel title="分组 × 模型状态" extra={<Typography.Text type="tertiary" size="small">网络错误只标记不可用，不计为降智</Typography.Text>}>
        <ResourceTable
          error={error}
          onRetry={reload}
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={statuses}
          columns={statusColumns}
          rowKey={qualityKey}
          pagination={{ pageSize: 20, currentPage: statusPage, onPageChange: setStatusPage }}
          onRow={(row) => ({ onClick: () => setSelectedStatus(row) })}
          emptyTitle="暂无可检测的分组模型"
          emptyDesc="请先添加活跃账号及其模型能力；启用定时检测后会自动生成状态。"
          skeletonRows={6}
          skeletonCols={12}
          mobileRenderer={renderMobileStatus}
          mobileListLabel="分组模型状态"
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
          emptyTitle="暂无检测记录"
          skeletonRows={6}
          skeletonCols={10}
        />
      </Panel>

      <Drawer
        visible={Boolean(selectedStatus)}
        onCancel={() => setSelectedStatus(null)}
        title={`模型质量 · ${selectedStatus?.model || '详情'}`}
        footer={selectedStatus ? (
          <Button
            theme="solid"
            icon={<IconPlay />}
            disabled={Boolean(runningKey) || Boolean(data?.running)}
            onClick={() => {
              const row = selectedStatus;
              setSelectedStatus(null);
              void runOne(row);
            }}
          >
            立即检测
          </Button>
        ) : null}
      >
        {selectedStatus ? <QualityStatusDetails row={selectedStatus} threshold={degradedThreshold} /> : null}
      </Drawer>
    </div>
  );
}
