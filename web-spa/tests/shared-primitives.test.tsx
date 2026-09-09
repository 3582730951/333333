import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Form, TabPane, Tabs } from '../src/components/pool/index.jsx';
import { TagList, TextClamp } from '../src/components/DisplayPrimitives.jsx';

describe('shared form and navigation primitives', () => {
  it('switches panes when Tabs is uncontrolled', async () => {
    const user = userEvent.setup();
    render(
      <Tabs defaultActiveKey="first">
        <TabPane itemKey="first" tab="第一项"><div>第一面板</div></TabPane>
        <TabPane itemKey="second" tab="第二项"><div>第二面板</div></TabPane>
      </Tabs>,
    );

    expect(screen.getByText('第一面板')).toBeVisible();
    await user.click(screen.getByRole('tab', { name: '第二项' }));
    expect(screen.getByText('第二面板')).toBeVisible();
    expect(screen.getByRole('tab', { name: '第二项' })).toHaveAttribute('data-state', 'active');
  });

  it('treats whitespace and empty arrays as missing required values', async () => {
    const onSubmit = vi.fn();
    const { container } = render(
      <Form onSubmit={onSubmit} initValues={{ targets: [] }}>
        <Form.Input field="name" label="名称" rules={[{ required: true, message: '请输入名称' }]} />
        <Form.Select field="targets" label="目标" multiple optionList={[{ label: 'A', value: 'a' }]} rules={[{ required: true, message: '请选择目标' }]} />
        <button type="submit">保存</button>
      </Form>,
    );

    fireEvent.change(screen.getByLabelText('名称'), { target: { value: '   ' } });
    fireEvent.submit(container.querySelector('form')!);
    expect(await screen.findByText('请输入名称')).toBeVisible();
    expect(await screen.findByText('请选择目标')).toBeVisible();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('forwards field error semantics to searchable selects', async () => {
    render(
      <Form>
        <Form.Select field="provider" label="提供商" filter optionList={[{ label: 'OpenAI', value: 'openai' }]} rules={[{ required: true, message: '请选择提供商' }]} />
      </Form>,
    );

    const control = screen.getByRole('combobox', { name: '提供商' });
    expect(control).not.toHaveAttribute('aria-invalid');
    fireEvent.submit(control.closest('form')!);
    expect(await screen.findByText('请选择提供商')).toBeVisible();
    expect(control).toHaveAttribute('aria-invalid', 'true');
    expect(control).toHaveAttribute('aria-describedby');
  });

  it('keeps overflow tags discoverable and clickable text keyboard accessible', async () => {
    const user = userEvent.setup();
    const onClick = vi.fn();
    render(<><TagList items={['A', 'B', 'C']} max={1} /><TextClamp onClick={onClick}>详情</TextClamp></>);
    const more = screen.getByText('+2').closest('button');
    expect(more).toBeInTheDocument();
    expect(more).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByText('B')).toBeNull();
    await user.click(screen.getByText('+2'));
    expect(more).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByText('B')).toBeVisible();
    const detail = screen.getByRole('button', { name: '详情' });
    detail.focus();
    await user.keyboard('{Enter}');
    expect(onClick).toHaveBeenCalledTimes(1);
  });
});
