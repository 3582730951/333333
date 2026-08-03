import React, { useState } from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { Form } from '../src/components/pool/Form.jsx';

const TestForm = Form as any;

describe('form validation interaction', () => {
  it('blocks invalid email and short password before submit', () => {
    const onSubmit = vi.fn();
    render(
      <TestForm onSubmit={onSubmit}>
        <TestForm.Input
          field="email"
          label="邮箱"
          rules={[
            { required: true, message: '请输入邮箱' },
            { type: 'email', message: '请输入有效邮箱' },
          ]}
        />
        <TestForm.Input field="password" label="密码" rules={[{ min: 8, message: '密码至少需要 8 位' }]} />
        <button type="submit">保存</button>
      </TestForm>,
    );

    fireEvent.change(screen.getByLabelText('邮箱'), { target: { value: 'invalid' } });
    fireEvent.change(screen.getByLabelText('密码'), { target: { value: 'short' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    expect(screen.getByText('请输入有效邮箱')).toBeVisible();
    expect(screen.getByText('密码至少需要 8 位')).toBeVisible();
    expect(onSubmit).not.toHaveBeenCalled();

    fireEvent.change(screen.getByLabelText('邮箱'), { target: { value: 'user@example.com' } });
    fireEvent.change(screen.getByLabelText('密码'), { target: { value: 'password' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    expect(onSubmit).toHaveBeenCalledWith({ email: 'user@example.com', password: 'password' });
  });

  it('filters a single select and commits the keyboard-selected value', async () => {
    const onSubmit = vi.fn();
    render(
      <TestForm onSubmit={onSubmit}>
        <TestForm.Select
          field="country"
          label="手动国家"
          filter
          placeholder="搜索国家或 ISO 代码"
          emptyContent="没有匹配的国家"
          rules={[{ required: true }]}
          optionList={[
            { label: 'US - 美国 (United States)', value: 'US' },
            { label: 'BR - 巴西 (Brazil)', value: 'BR' },
            { label: 'CO - 哥伦比亚 (Colombia)', value: 'CO' },
          ]}
        />
        <button type="submit">开始</button>
      </TestForm>,
    );

    const trigger = screen.getByRole('combobox', { name: '手动国家' });
    expect(trigger.tagName).toBe('BUTTON');
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    await waitFor(() => expect(trigger).toHaveAttribute('aria-expanded', 'true'));

    const search = await screen.findByRole('combobox', { name: '搜索国家或 ISO 代码' });
    fireEvent.change(search, { target: { value: 'br' } });
    expect(screen.getByRole('option', { name: 'BR - 巴西 (Brazil)' })).toBeVisible();
    expect(screen.queryByRole('option', { name: 'US - 美国 (United States)' })).not.toBeInTheDocument();
    fireEvent.keyDown(search, { key: 'Enter' });

    await waitFor(() => expect(trigger).toHaveAttribute('aria-expanded', 'false'));
    expect(trigger).toHaveTextContent('BR - 巴西 (Brazil)');
    fireEvent.click(screen.getByRole('button', { name: '开始' }));
    expect(onSubmit).toHaveBeenCalledWith({ country: 'BR' });
  });

  it('keeps unfiltered single selects native for compatibility', () => {
    render(<TestForm.Select label="普通选择" optionList={[{ label: '一', value: 1 }]} />);
    expect(screen.getByRole('combobox', { name: '普通选择' }).tagName).toBe('SELECT');
  });

  it('keeps edits when a parent rerenders with equivalent initial values', () => {
    const onValueChange = vi.fn();
    function Host() {
      const [dirty, setDirty] = useState(false);
      const initial = { name: 'initial', enabled: false };
      return (
        <TestForm
          initValues={initial}
          onValueChange={(values: unknown, changed: unknown) => {
            onValueChange(values, changed);
            setDirty(true);
          }}
        >
          <TestForm.Input field="name" label="名称" />
          <TestForm.Switch field="enabled" label="启用" />
          <span>{dirty ? '有未保存更改' : '已保存'}</span>
        </TestForm>
      );
    }

    render(<Host />);
    fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'edited' } });
    expect(screen.getByLabelText('名称')).toHaveValue('edited');
    expect(screen.getByText('有未保存更改')).toBeVisible();

    fireEvent.click(screen.getByRole('switch', { name: '启用' }));
    expect(screen.getByRole('switch', { name: '启用' })).toHaveAttribute('aria-checked', 'true');
    expect(onValueChange).toHaveBeenLastCalledWith(
      expect.objectContaining({ name: 'edited', enabled: true }),
      { enabled: true },
    );
  });
});
