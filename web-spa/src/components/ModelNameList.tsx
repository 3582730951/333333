import React, { useCallback, useDeferredValue, useMemo, useState } from 'react';
import { get } from '../api.js';
import * as PoolUI from './pool/index.jsx';
import { IconRefresh, IconSearch } from './pool/icons.jsx';
import LoadErrorBanner from './LoadErrorBanner.jsx';
import PageHeader from './PageHeader.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import * as MicroCharts from './MicroCharts.jsx';
import { fmtInt, fmtRelative, fmtTokens } from '../lib/format.js';
import { t } from '../lib/i18n.js';

const { Button, EmptyState, Input, Tag } = PoolUI as any;
// MicroCharts is untyped JSX, so `segments = []` infers never[] at a .tsx call site. Every other
// .tsx consumer destructures through `as any` for the same reason; matching that.
const { StackedMeter } = MicroCharts as any;

// `capabilities` is omitempty on the Go side and populated only by /admin/models. Its absence is
// the normal user-endpoint response, not a degraded one, so every capability branch below is gated
// on the row existing rather than on a role flag -- the component stays honest about what it was
// actually sent instead of guessing from the endpoint string.
type ModelCapability = {
  model: string;
  accounts: number;
  verified: number;
  unverified: number;
  unsupported: number;
  context_1m_supported: number;
  context_1m_unsupported: number;
  context_1m_unknown: number;
  max_context_window: number;
  last_probe_at: number;
};

type ModelsResponse = { models?: string[]; capabilities?: ModelCapability[]; generated_at?: number };

const AVAIL_COLORS = {
  verified: 'var(--pool-success)',
  unverified: 'var(--pool-warning)',
  unsupported: 'var(--pool-danger)',
} as const;

// A model name carries its family in its prefix, so grouping on that first segment turns
// a flat alphabetical list into the shape of what the pool can actually serve. Anything
// without a recognisable prefix falls into one bucket rather than inventing a group of one.
const FAMILIES: Array<[RegExp, string]> = [
  [/^(gpt|o\d|codex|text-|davinci)/i, 'OpenAI'],
  [/^claude/i, 'Claude'],
  [/^gemini/i, 'Gemini'],
  [/^(deepseek|qwen|glm|kimi|moonshot|yi-|ernie|hunyuan|doubao|minimax|step-|spark)/i, '国产模型'],
  [/^(llama|mistral|mixtral|gemma|phi|command|grok)/i, '开放权重'],
];

function familyOf(model: string) {
  for (const [pattern, name] of FAMILIES) {
    if (pattern.test(model)) return name;
  }
  return '其他';
}

export default function ModelNameList({ endpoint, title, subtitle }: { endpoint: string; title: string; subtitle: string }) {
  const fetchModels = useCallback(async ({ signal }: { signal?: AbortSignal }) => get(endpoint, undefined, { signal }), [endpoint]);
  const { data, loading, error, reload } = useAsyncResource(fetchModels, [fetchModels], { initialData: { models: [] } });
  const models = Array.isArray((data as ModelsResponse)?.models) ? (data as ModelsResponse).models! : [];
  const capabilities = Array.isArray((data as ModelsResponse)?.capabilities) ? (data as ModelsResponse).capabilities! : [];
  const [query, setQuery] = useState('');

  // Keyed on the lowercased name because the server dedupes on exactly that, so a row whose
  // stored spelling differs in case from the one in `models` still finds its capability.
  const capByModel = useMemo(() => {
    const map = new Map<string, ModelCapability>();
    for (const row of capabilities) {
      if (row && typeof row.model === 'string' && row.model) map.set(row.model.toLowerCase(), row);
    }
    return map;
  }, [capabilities]);

  const summary = useMemo(() => {
    if (!capabilities.length) return null;
    const totals = {
      pairs: 0, verified: 0, unverified: 0, unsupported: 0,
      ctx1m: 0, widest: 0, probed: 0, neverProbed: 0,
    };
    for (const row of capabilities) {
      totals.pairs += row.accounts || 0;
      totals.verified += row.verified || 0;
      totals.unverified += row.unverified || 0;
      totals.unsupported += row.unsupported || 0;
      if ((row.context_1m_supported || 0) > 0) totals.ctx1m += 1;
      if ((row.max_context_window || 0) > totals.widest) totals.widest = row.max_context_window;
      if (row.last_probe_at) totals.probed = Math.max(totals.probed, row.last_probe_at);
      else totals.neverProbed += 1;
    }
    return totals;
  }, [capabilities]);
  // Filtering runs over every name on each keystroke; deferring it keeps the field
  // responsive on the long lists this page is built for.
  const deferredQuery = useDeferredValue(query);

  const groups = useMemo(() => {
    const needle = deferredQuery.trim().toLowerCase();
    const matched = needle ? models.filter((model) => model.toLowerCase().includes(needle)) : models;
    const buckets = new Map<string, string[]>();
    for (const model of [...matched].sort((a, b) => a.localeCompare(b))) {
      const family = familyOf(model);
      const bucket = buckets.get(family);
      if (bucket) bucket.push(model);
      else buckets.set(family, [model]);
    }
    // Biggest family first: it is the one a reader is most likely looking in.
    return [...buckets.entries()].sort((a, b) => b[1].length - a[1].length || a[0].localeCompare(b[0]));
  }, [models, deferredQuery]);

  const matchCount = groups.reduce((sum, [, names]) => sum + names.length, 0);
  const filtering = Boolean(deferredQuery.trim());
  // StackedMeter returns null on a zero total, which would leave the labelled cell empty -- a
  // heading with nothing under it reads as a broken chart rather than as absent data.
  const availTotal = summary ? summary.verified + summary.unverified + summary.unsupported : 0;

  return (
    <div>
      <PageHeader
        title={title}
        subtitle={subtitle}
        actions={<Button icon={<IconRefresh />} loading={loading} onClick={reload}>{t('common.refresh')}</Button>}
      />
      <LoadErrorBanner error={error} onRetry={reload} />
      {summary ? (
        <div className="pool-panel pool-model-summary">
          <dl className="pool-model-summary__grid">
            <div className="pool-model-summary__cell">
              <dt>{t('models.summary_models')}</dt>
              <dd>{fmtInt(capabilities.length)}</dd>
              <small>{`${fmtInt(summary.pairs)} ${t('models.summary_pairs')}`}</small>
            </div>
            <div className="pool-model-summary__cell pool-model-summary__cell--wide">
              <dt>{t('models.summary_availability')}</dt>
              <dd className="pool-model-summary__meter">
                {availTotal > 0 ? (
                  <StackedMeter
                    ariaLabel={t('models.summary_availability')}
                    valueFormatter={(value: number) => fmtInt(value)}
                    segments={[
                      { key: 'verified', name: t('models.verified'), value: summary.verified, color: AVAIL_COLORS.verified },
                      { key: 'unverified', name: t('models.unverified'), value: summary.unverified, color: AVAIL_COLORS.unverified },
                      { key: 'unsupported', name: t('models.unsupported'), value: summary.unsupported, color: AVAIL_COLORS.unsupported },
                    ]}
                  />
                ) : '—'}
              </dd>
            </div>
            <div className="pool-model-summary__cell">
              <dt>{t('models.summary_context_1m')}</dt>
              <dd>{fmtInt(summary.ctx1m)}</dd>
              <small>{`${t('models.summary_widest')} ${fmtTokens(summary.widest)}`}</small>
            </div>
            <div className="pool-model-summary__cell">
              <dt>{t('models.summary_last_probe')}</dt>
              <dd>{summary.probed ? fmtRelative(summary.probed) : '—'}</dd>
              <small>
                {summary.neverProbed > 0
                  ? `${fmtInt(summary.neverProbed)} ${t('models.summary_never_probed')}`
                  : t('models.summary_all_probed')}
              </small>
            </div>
          </dl>
        </div>
      ) : null}
      {models.length > 0 ? (
        <div className="pool-model-directory__filter">
          <Input
            prefix={<IconSearch />}
            value={query}
            onChange={setQuery}
            placeholder="筛选模型名称"
            aria-label="筛选模型名称"
            showClear
          />
          <span className="pool-model-directory__count">
            {filtering ? `${matchCount} / ${models.length}` : `${models.length} 个模型`}
          </span>
        </div>
      ) : null}
      {loading && models.length === 0 ? (
        <div className="pool-panel pool-model-directory">
          <div className="pool-model-directory__list">
            {Array.from({ length: 6 }, (_, index) => (
              <div key={index} className="pool-model-directory__item pool-model-directory__item--skeleton" aria-hidden="true">
                <span />
                <i />
              </div>
            ))}
          </div>
          <span className="pool-sr-only" role="status">读取模型列表…</span>
        </div>
      ) : null}
      {!loading && models.length === 0 ? (
        <EmptyState
          title="暂无可用模型"
          desc="账号池还没有上报能力快照。接入账号并完成一次探测后，模型名称会出现在这里。"
          action={<Button icon={<IconRefresh />} onClick={reload}>重新读取</Button>}
        />
      ) : null}
      {models.length > 0 && matchCount === 0 ? (
        <EmptyState
          title="没有匹配的模型"
          desc={`“${deferredQuery.trim()}” 不在这 ${models.length} 个模型名称中。`}
          action={<Button onClick={() => setQuery('')}>清除筛选</Button>}
        />
      ) : null}
      {groups.map(([family, names]) => (
        <section key={family} className="pool-panel pool-model-directory">
          <header className="pool-model-directory__head">
            <h3>{family}</h3>
            <Tag>{names.length}</Tag>
          </header>
          <div className={`pool-model-directory__list ${capByModel.size ? 'pool-model-directory__list--rich' : ''}`}>
            {names.map((model) => {
              const cap = capByModel.get(model.toLowerCase());
              if (!cap) {
                return (
                  <div key={model} className="pool-model-directory__item">
                    <span aria-hidden="true" />
                    <code>{model}</code>
                  </div>
                );
              }
              // One derivation, not two parallel ternaries: the tag's colour and its label have to
              // agree, and computing the same three-way test twice is how they stop agreeing.
              const ctx1m = (cap.context_1m_supported || 0) > 0
                ? { color: 'green', label: t('models.ctx_1m_yes') }
                : (cap.context_1m_unsupported || 0) > 0
                  ? { color: 'grey', label: t('models.ctx_1m_no') }
                  : { color: 'amber', label: t('models.ctx_1m_unknown') };
              return (
                <div key={model} className="pool-model-directory__item pool-model-directory__item--rich">
                  <div className="pool-model-directory__name">
                    <code>{model}</code>
                    {cap.max_context_window > 0 ? <b>{fmtTokens(cap.max_context_window)}</b> : null}
                  </div>
                  {/* legend={false}: the counts are spelled out on the metadata line below, so a
                      legend here would repeat them once per row across a multi-column grid. */}
                  <StackedMeter
                    legend={false}
                    ariaLabel={`${model} · ${t('models.verified')} ${cap.verified} · ${t('models.unverified')} ${cap.unverified} · ${t('models.unsupported')} ${cap.unsupported}`}
                    valueFormatter={(value: number) => fmtInt(value)}
                    segments={[
                      { key: 'verified', name: t('models.verified'), value: cap.verified, color: AVAIL_COLORS.verified },
                      { key: 'unverified', name: t('models.unverified'), value: cap.unverified, color: AVAIL_COLORS.unverified },
                      { key: 'unsupported', name: t('models.unsupported'), value: cap.unsupported, color: AVAIL_COLORS.unsupported },
                    ]}
                  />
                  <div className="pool-model-directory__meta">
                    <span>{`${fmtInt(cap.accounts)} ${t('models.row_accounts')}`}</span>
                    <Tag size="small" color={ctx1m.color}>{ctx1m.label}</Tag>
                    <time dateTime={cap.last_probe_at ? new Date(cap.last_probe_at * 1000).toISOString() : undefined}>
                      {fmtRelative(cap.last_probe_at)}
                    </time>
                  </div>
                </div>
              );
            })}
          </div>
        </section>
      ))}
    </div>
  );
}
