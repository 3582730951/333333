import { describe, expect, it } from 'vitest';
import { systemMetricsSchema } from '../src/features/observability/api/system';

describe('systemMetricsSchema', () => {
  it('accepts an idle backend snapshot with nullable collections', () => {
    const parsed = systemMetricsSchema.parse({
      supported: true,
      cpu: { scope: 'cgroup', cores: 2, usage_pct: 0, load1: 0 },
      mem: { scope: 'cgroup', total_kb: 2 * 1024 * 1024, used_kb: 1024, used_pct: 0.1 },
      disk: { path: '/', total_bytes: 100, used_bytes: 20, free_bytes: 80, used_pct: 20 },
      network: { interfaces: 0, interface_names: null, rx_bytes: 0, tx_bytes: 0, rx_bytes_per_sec: 0, tx_bytes_per_sec: 0, total_bytes_per_sec: 0 },
      registration: { node: 0, chrome: 0, xvfb: 0, total_rss_kb: 0, procs: null },
      go: { goroutines: 1, sys_bytes: 1024 },
      supervisor_events: null,
      supervisor_modules: null,
    });

    expect(parsed.registration?.procs).toEqual([]);
    expect(parsed.network?.interface_names).toEqual([]);
    expect(parsed.supervisor_events).toEqual([]);
    expect(parsed.supervisor_modules).toEqual([]);
  });
});
