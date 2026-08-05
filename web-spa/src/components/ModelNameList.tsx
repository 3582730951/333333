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
    <div className="pool-panel pool-model-directory">
      {loading && models.length === 0 ? <div className="pool-model-directory__state pool-muted">读取中...</div> : null}
      {!loading && models.length === 0 ? <div className="pool-model-directory__state pool-muted">暂无可用模型</div> : null}
      <div className="pool-model-directory__list">
        {models.map((model) => <div key={model} className="pool-model-directory__item"><span aria-hidden="true" /><code>{model}</code></div>)}
      </div>
    </div>
  </div>;
}
