import React, { useRef } from 'react';
import { Button, Form, Modal } from '@douyinfe/semi-ui';
import { IconPlus } from '@douyinfe/semi-icons';
import { showErrorToast } from './ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';

const EFFORT_OPTIONS = [
  { label: '不限制', value: '' },
  { label: 'minimal', value: 'minimal' },
  { label: 'low', value: 'low' },
  { label: 'medium', value: 'medium' },
  { label: 'high', value: 'high' },
  { label: 'xhigh', value: 'xhigh' },
];

function cleanValues(values, mode) {
  const cleaned = {
    label: String(values.label || '').trim(),
    force_model: String(values.force_model || '').trim(),
    force_effort: String(values.force_effort || '').trim(),
  };
  if (mode === 'admin') {
    cleaned.group_name = String(values.group_name || '').trim();
  }
  return cleaned;
}

export default function ApiKeyCreateModal({
  visible,
  mode = 'admin',
  onCancel,
  onCreate,
}) {
  const formApi = useRef(null);
  const admin = mode === 'admin';

  const { run: submit, running: submitting } = useAsyncAction(async (values) => {
    try {
      await onCreate(cleanValues(values, mode));
      formApi.current?.reset?.();
    } catch (error) {
      showErrorToast(error);
    }
  });

  return (
    <Modal
      title={admin ? '创建 API Key' : '新建 API Key'}
      visible={visible}
      onCancel={onCancel}
      footer={null}
      maskClosable={!submitting}
    >
      <Form
        getFormApi={(api) => { formApi.current = api; }}
        labelPosition="left"
        labelWidth={90}
        onSubmit={submit}
      >
        <Form.Input field="label" label={admin ? '标签' : '名称'} placeholder={admin ? '' : '例如：本地开发'} />
        {admin ? <Form.Input field="group_name" label="分组" placeholder="可选" /> : null}
        <Form.Input field="force_model" label="强制模型" placeholder="可选，如 gpt-5.5" />
        <Form.Select field="force_effort" label="推理强度" optionList={EFFORT_OPTIONS} initValue="" />
        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 12 }}>
          <Button onClick={onCancel} disabled={submitting}>取消</Button>
          <Button htmlType="submit" theme="solid" icon={<IconPlus />} loading={submitting}>创建</Button>
        </div>
      </Form>
    </Modal>
  );
}
