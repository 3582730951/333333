import React, { useState } from 'react';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import OrderedEgressSelect from '../src/components/OrderedEgressSelect.jsx';
import { blankUserGroup, normalizedUserGroupPayload, userGroupDraft } from '../src/pages/Groups.jsx';

const options = [{ label: 'Exit A', value: 'a' }, { label: 'Exit B', value: 'b' }];

describe('outlet balancing configuration', () => {
  it('lets the user reorder outlets by button and keyboard without losing selections', () => {
    const onChange = vi.fn();
    function Editor() {
      const [value, setValue] = useState(['a', 'b']);
      return <OrderedEgressSelect value={value} options={options} help="Order" onChange={(next: string[]) => {
        onChange(next);
        setValue(next);
      }} />;
    }
    render(<Editor />);
    fireEvent.click(screen.getByRole('button', { name: '上移 Exit B' }));
    expect(onChange).toHaveBeenLastCalledWith(['b', 'a']);
    let rows = within(screen.getByRole('list')).getAllByRole('listitem');
    expect(rows[0]).toHaveTextContent('Exit B');

    fireEvent.keyDown(screen.getByRole('button', { name: 'Exit B，使用上下方向键调整顺序' }), { key: 'ArrowDown' });
    expect(onChange).toHaveBeenLastCalledWith(['a', 'b']);
    rows = within(screen.getByRole('list')).getAllByRole('listitem');
    expect(rows[0]).toHaveTextContent('Exit A');
  });

  it('preserves unavailable selected outlets and ignores changes while disabled', () => {
    const onChange = vi.fn();
    render(<OrderedEgressSelect value={['b', 'missing', 'a']} options={options} disabled help="Order" onChange={onChange} />);
    const rows = within(screen.getByRole('list')).getAllByRole('listitem');
    expect(rows[0]).toHaveTextContent('Exit B');
    expect(rows[1]).toHaveTextContent('missing');
    expect(screen.getByRole('button', { name: '上移 missing' })).toBeDisabled();
    fireEvent.keyDown(screen.getByRole('button', { name: 'missing，使用上下方向键调整顺序' }), { key: 'ArrowUp' });
    expect(onChange).not.toHaveBeenCalled();
  });

  it('keeps the account switch off and preserves outlet order when a saved threshold remains', () => {
    const draft = {
      ...blankUserGroup(),
      name: 'outlet-only',
      dynamic_pool_balance_enabled: false,
      dynamic_pool_balance_rpm_threshold: 10,
      egress_rpm_balance_enabled: true,
      egress_rpm_balance_threshold: 20,
      egress_rpm_balance_egress_ids: ['b', 'a'],
    };
    const payload = normalizedUserGroupPayload(draft);
    const restored = userGroupDraft(payload);
    expect(restored.dynamic_pool_balance_enabled).toBe(false);
    expect(restored.dynamic_pool_balance_rpm_threshold).toBe(10);
    expect(restored.egress_rpm_balance_enabled).toBe(true);
    expect(restored.egress_rpm_balance_egress_ids).toEqual(['b', 'a']);
  });
});
