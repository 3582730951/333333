import React, { useCallback, useDeferredValue, useMemo, useState } from 'react';
import { get } from '../api.js';
import * as PoolUI from './pool/index.jsx';
import { IconRefresh, IconSearch } from './pool/icons.jsx';
import LoadErrorBanner from './LoadErrorBanner.jsx';
import PageHeader from './PageHeader.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { t } from '../lib/i18n.js';

const { Button, EmptyState, Input, Tag } = PoolUI as any;

type ModelsResponse = { models?: string[]; generated_at?: number };

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
  const [query, setQuery] = useState('');
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

  return (
    <div>
      <PageHeader
        title={title}
        subtitle={subtitle}
        actions={<Button icon={<IconRefresh />} loading={loading} onClick={reload}>{t('common.refresh')}</Button>}
      />
      <LoadErrorBanner error={error} onRetry={reload} />
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
          <div className="pool-model-directory__list">
            {names.map((model) => (
              <div key={model} className="pool-model-directory__item">
                <span aria-hidden="true" />
                <code>{model}</code>
              </div>
            ))}
          </div>
        </section>
      ))}
    </div>
  );
}
