import React, { useCallback } from 'react';
import { get } from '../api.js';
import LoadErrorBanner from './LoadErrorBanner.jsx';
import PageHeader from './PageHeader.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';

type ModelsResponse = { models?: string[]; generated_at?: number };

export default function ModelNameList({ endpoint, title, subtitle }: { endpoint: string; title: string; subtitle: string }) {
  const fetchModels = useCallback(async ({ signal }: { signal?: AbortSignal }) => get(endpoint, undefined, { signal }), [endpoint]);
  const { data, loading, error, reload } = useAsyncResource(fetchModels, [fetchModels], { initialData: { models: [] } });
  const models = Array.isArray((data as ModelsResponse)?.models) ? (data as ModelsResponse).models! : [];
  return <div>
    <PageHeader title={title} subtitle={subtitle} actions={null} />
    <LoadErrorBanner error={error} onRetry={reload} />
    <div className="pool-panel" style={{ maxWidth: 760 }}>
      {loading && models.length === 0 ? <div className="pool-muted">读取中...</div> : null}
      {!loading && models.length === 0 ? <div className="pool-muted">暂无可用模型</div> : null}
      <div style={{ display: 'grid', gap: 8 }}>
        {models.map((model) => <div key={model} className="pool-mono" style={{ padding: '10px 12px', borderBottom: '1px solid var(--semi-color-border)' }}>{model}</div>)}
      </div>
    </div>
  </div>;
}
