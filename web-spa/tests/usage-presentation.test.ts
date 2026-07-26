import { describe, expect, it } from 'vitest';
import {
  reportedCacheMetric, usageDimensionKey, usageDisplayLabel,
} from '../src/features/observability/model/usage';

describe('Provider + Model usage presentation', () => {
  it('keeps the same model separate across providers even without server labels', () => {
    const codex = { provider_id: 'codex', provider_name: 'Codex', model: 'gpt-5.6-sol' };
    const kiro = { provider_id: 'kiro', provider_name: 'Kiro', model: 'gpt-5.6-sol' };
    expect(usageDimensionKey(codex)).not.toBe(usageDimensionKey(kiro));
    expect(usageDisplayLabel(codex)).toBe('Codex · gpt-5.6-sol');
    expect(usageDisplayLabel(kiro)).toBe('Kiro · gpt-5.6-sol');
  });

  it('uses stable server dimensions and never renders unreported cache as zero', () => {
    expect(usageDimensionKey({ dimension_key: 'provider:relay\u0000model:gpt', model: 'gpt' }))
      .toBe('provider:relay\u0000model:gpt');
    expect(reportedCacheMetric(
      { cache_reporting_state: 'unreported' },
      0,
      '上游未报告',
      (value) => String(value),
    )).toBe('上游未报告');
  });
});
