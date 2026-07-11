import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
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
});
