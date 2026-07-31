import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { filterConfigCategories, SettingsCategorySection, SettingsDisclosure } from '../src/pages/SettingsV2';
import type { ConfigField } from '../src/features/settings/model/settings';

function field(overrides: Partial<ConfigField>): ConfigField {
  return {
    key: 'example_key',
    label: '示例设置',
    category: '行为 / 缓存',
    type: 'bool',
    effect: 'runtime',
    options: [],
    help: '用于验证设置呈现',
    placement: 'system_settings',
    domain: null,
    scope: 'global',
    section: 'config',
    order: 1,
    value: false,
    overridden: false,
    settings_error: '',
    ...overrides,
  };
}

describe('settings information architecture', () => {
  it('finds fields by label, help text, and technical key while retaining their category', () => {
    const fields = [
      field({ key: 'leak_scrub', label: '泄漏擦除', help: '清理上游信号' }),
      field({ key: 'goal_retention_days', label: '目标保留天数', category: '限流 / 封禁', help: '运行目标保留周期' }),
    ];

    expect(filterConfigCategories(fields, 'goal_retention_days')).toEqual([
      ['限流 / 封禁', [fields[1]]],
    ]);
    expect(filterConfigCategories(fields, '清理上游')).toEqual([
      ['行为 / 缓存', [fields[0]]],
    ]);
    expect(filterConfigCategories(fields, '目标保留')).toEqual([
      ['限流 / 封禁', [fields[1]]],
    ]);
  });

  it('keeps advanced settings one click away and exposes state through aria-expanded', () => {
    const onChange = vi.fn();
    render(
      <SettingsCategorySection
        category="行为 / 缓存"
        fields={[field({ key: 'leak_scrub', label: '泄漏擦除' })]}
        pending={{}}
        onChange={onChange}
      />,
    );

    const trigger = screen.getByRole('button', { name: /行为 \/ 缓存/ });
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('switch', { name: '泄漏擦除' })).not.toBeInTheDocument();

    fireEvent.click(trigger);
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    const control = screen.getByRole('switch', { name: '泄漏擦除' });
    fireEvent.click(control);
    expect(onChange).toHaveBeenCalledWith('leak_scrub', true);
  });

  it('groups registrar providers without unmounting their form controls', () => {
    render(
      <SettingsDisclosure
        title="邮箱提供商"
        subtitle="已启用 1 / 3"
        badge={<span>3 个提供商</span>}
      >
        <label>
          Worker API URL
          <input aria-label="Worker API URL" defaultValue="https://mail.example.test" />
        </label>
      </SettingsDisclosure>,
    );

    const trigger = screen.getByRole('button', { name: /邮箱提供商/ });
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('textbox', { name: 'Worker API URL' })).not.toBeInTheDocument();
    fireEvent.click(trigger);
    expect(screen.getByRole('textbox', { name: 'Worker API URL' })).toHaveValue('https://mail.example.test');
  });
});
