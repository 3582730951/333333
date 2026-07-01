import React, { useEffect, useCallback, useRef } from 'react';
import { Form, Button, Toast, Banner } from '@douyinfe/semi-ui';
import { IconRefresh, IconSave } from '@douyinfe/semi-icons';
import { get, post } from '../api.js';
import LoadErrorBanner from './LoadErrorBanner.jsx';
import PageHeader from './PageHeader.jsx';
import { showErrorToast } from './ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';

// Generic config page: GET a config object, render each top-level key by JS type
// (bool→Switch, number→InputNumber, string→Input, array/object→JSON/lines textarea),
// and POST the reconstructed object back. Works for endpoints whose field set is dynamic.
export default function ConfigForm({ title, subtitle, url }) {
  const meta = useRef({}); // field -> {kind}
  const apiRef = useRef(null);

  const fetchConfig = useCallback(async ({ signal }) => {
    let d = await get(url, undefined, { signal });
    if (d && typeof d === 'object' && !Array.isArray(d) && d.config && typeof d.config === 'object') d = d.config;
    return d && typeof d === 'object' ? d : {};
  }, [url]);

  const { data, loading, error, reload: load } = useAsyncResource(fetchConfig, [fetchConfig], { initialData: null });

  useEffect(() => {
    if (!data || !apiRef.current) return;
    const init = {};
    for (const [k, v] of Object.entries(data)) {
      if (Array.isArray(v)) { meta.current[k] = 'array'; init[k] = v.join('\n'); }
      else if (v && typeof v === 'object') { meta.current[k] = 'json'; init[k] = JSON.stringify(v, null, 2); }
      else if (typeof v === 'boolean') { meta.current[k] = 'bool'; init[k] = v; }
      else if (typeof v === 'number') { meta.current[k] = 'number'; init[k] = v; }
      else { meta.current[k] = 'string'; init[k] = v ?? ''; }
    }
    apiRef.current.setValues(init, { isOverride: true });
  }, [data]);

  const { run: save, running: saving } = useAsyncAction(async () => {
    try {
      const v = apiRef.current.getValues();
      const out = {};
      for (const [k, kind] of Object.entries(meta.current)) {
        const raw = v[k];
        if (kind === 'array') out[k] = String(raw || '').split('\n').map((s) => s.trim()).filter(Boolean);
        else if (kind === 'json') { try { out[k] = JSON.parse(raw || 'null'); } catch { Toast.error(`${k} 不是合法 JSON`); throw new Error('json'); } }
        else if (kind === 'number') out[k] = Number(raw);
        else out[k] = raw;
      }
      await post(url, out);
      Toast.success('已保存');
      await load();
    } catch (e) { if (e.message !== 'json') showErrorToast(e); }
  });

  const entries = data ? Object.entries(data) : [];
  return (
    <div className="pool-config-page">
      <PageHeader title={title} subtitle={subtitle}
        actions={<>
          <Button icon={<IconRefresh />} onClick={load} loading={loading}>刷新</Button>
          <Button icon={<IconSave />} theme="solid" loading={saving} onClick={save}>保存</Button>
        </>} />
      <LoadErrorBanner error={error} onRetry={load} />
      {data && !entries.length && <Banner type="info" description="该配置当前为空。" />}
      <div className="pool-panel pool-config-panel">
        <Form getFormApi={(a) => { apiRef.current = a; }} labelPosition="left" labelWidth={176} className="pool-config-form">
          {entries.map(([k, v]) => {
            const kind = meta.current[k];
            if (kind === 'bool' || typeof v === 'boolean') return <Form.Switch key={k} field={k} label={k} className="pool-config-field" />;
            if (kind === 'number' || typeof v === 'number') return <Form.InputNumber key={k} field={k} label={k} className="pool-config-field pool-config-field--short" style={{ width: 'min(100%, 220px)' }} />;
            if (Array.isArray(v)) return <Form.TextArea key={k} field={k} label={`${k}（每行一项）`} autosize rows={3} className="pool-config-field pool-config-field--text" style={{ width: 'min(100%, 560px)' }} />;
            if (v && typeof v === 'object') return <Form.TextArea key={k} field={k} label={`${k}（JSON）`} autosize rows={4} className="pool-config-field pool-config-field--json pool-mono" style={{ width: '100%' }} />;
            return <Form.Input key={k} field={k} label={k} className="pool-config-field" style={{ width: 'min(100%, 480px)' }} />;
          })}
        </Form>
      </div>
    </div>
  );
}
