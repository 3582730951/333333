import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { RegistrationJobCard } from '../src/pages/Registration';
import { CompactSystemRecord } from '../src/pages/System';

describe('compact operational records', () => {
  it('keeps a long registration id accessible and opens the complete detail view', () => {
    const onOpen = vi.fn();
    const id = 'registration-production-long-running-job-with-a-very-long-identifier-20260731';

    render(
      <RegistrationJobCard
        job={{
          id,
          method: 'protocol_v2',
          identity_mode: 'email',
          status: 'running',
          total: 40,
          succeeded: 31,
          failed: 2,
          group_name: 'production-registration-group-with-a-long-name',
          egress_id: 'global-egress-route-with-a-long-name',
        }}
        onOpen={onOpen}
      />,
    );

    const trigger = screen.getByRole('button', { name: new RegExp(id) });
    expect(trigger).toHaveTextContent(id);
    expect(trigger).toHaveTextContent('protocol_v2');
    expect(trigger).toHaveTextContent('31');
    expect(trigger).toHaveTextContent('2');
    fireEvent.click(trigger);
    expect(onOpen).toHaveBeenCalledTimes(1);
  });

  it('summarizes lifecycle counters without removing the detail affordance', () => {
    const onOpen = vi.fn();

    render(
      <CompactSystemRecord
        title="diagnostic-artifact-expiry-with-a-long-module-name"
        titleLabel="模块 diagnostic-artifact-expiry-with-a-long-module-name"
        badge={<span>运行中</span>}
        stats={[
          ['重启', 3],
          ['panic', 1],
          ['异常退出', 2],
        ]}
        note="运行 4分钟 · module running"
        onOpen={onOpen}
      />,
    );

    const trigger = screen.getByRole('button', { name: /diagnostic-artifact-expiry/ });
    expect(trigger).toHaveTextContent('重启');
    expect(trigger).toHaveTextContent('panic');
    expect(trigger).toHaveTextContent('异常退出');
    fireEvent.click(trigger);
    expect(onOpen).toHaveBeenCalledTimes(1);
  });
});
