import React, { useState, useCallback } from 'react';
import { ActionMenu, Button, Toast, Modal, Form, Tag, Switch } from '../components/pool/index.jsx';
import { IconPlus, IconRefresh } from '../components/pool/icons.jsx';
import { get, post, patch, del } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import { MetricRail, TagList, TextClamp } from '../components/DisplayPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useKeyedAsyncAction from '../hooks/useKeyedAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';

function groupPolicyTags(row) {
  const tags = [];
  if (row.force_model) tags.push({ label: row.force_model, color: 'blue' });
  if (row.force_effort) tags.push({ label: `effort ${row.force_effort}`, color: 'violet' });
  if (row.default_egress_id) tags.push({ label: `出口 ${row.default_egress_id}`, color: 'blue' });
  if (row.model_instructions_enabled) tags.push({ label: `指令文件 ${row.model_instructions_files?.length || 0}`, color: row.model_instructions_error ? 'red' : 'green' });
  if (!row.force_model && !row.force_effort) tags.unshift({ label: '继承默认', color: 'grey' });
  return tags;
}

function cleanGroupValues(values) {
  return {
    name: String(values.name || '').trim(),
    force_model: String(values.force_model || '').trim(),
    force_effort: String(values.force_effort || '').trim(),
    model_instructions_enabled: !!values.model_instructions_enabled,
    model_instructions_files: String(values.model_instructions_files_csv || '')
      .split(',')
      .map((item) => item.trim())
      .filter(Boolean),
    default_egress_id: String(values.default_egress_id || '').trim(),
  };
}

function cleanGroupPolicyValues(values) {
  return {
    name: String(values.name || '').trim(),
    force_model: String(values.force_model || '').trim(),
    force_effort: String(values.force_effort || '').trim(),
    default_egress_id: String(values.default_egress_id || '').trim(),
  };
}

const egressOptionList = (profiles = []) => {
  const out = [{ label: '不设置（未显式选择时导入回退 egress_direct）', value: '' }];
  const seen = new Set(['']);
  const add = (profile) => {
    const id = String(profile?.id || '').trim();
    if (!id || seen.has(id)) return;
    seen.add(id);
    out.push({ label: `${profile.name || id} (${profile.type || 'direct'})`, value: id });
  };
  add({ id: 'egress_direct', name: 'egress_direct', type: 'direct' });
  for (const profile of profiles || []) add(profile);
  return out;
};

function normalizedFileList(values = []) {
  const seen = new Set();
  const out = [];
  for (const raw of values || []) {
    const value = String(raw || '').trim();
    if (!value || seen.has(value)) continue;
    seen.add(value);
    out.push(value);
  }
  return out;
}

export default function Groups() {
  const [open, setOpen] = useState(false);
  const [instructionLibraryOpen, setInstructionLibraryOpen] = useState(false);
  const [instructionName, setInstructionName] = useState('');
  const [instructionContent, setInstructionContent] = useState('');
  const [editingGroup, setEditingGroup] = useState(null);
  const [policyEditor, setPolicyEditor] = useState(null);
  const [editFiles, setEditFiles] = useState([]);

  const fetchRows = useCallback(async ({ signal }) => {
    const [g, files, egresses] = await Promise.all([
      get('/admin/groups', undefined, { signal }),
      get('/admin/model-instructions', undefined, { signal }),
      get('/admin/egress-profiles', undefined, { signal }),
    ]);
    return {
      groups: Array.isArray(g) ? g : g?.groups || [],
      files: Array.isArray(files) ? files : files?.files || [],
      egresses: Array.isArray(egresses) ? egresses : egresses?.profiles || egresses?.egress_profiles || [],
    };
  }, []);
  const { data = { groups: [], files: [], egresses: [] }, loading, error, lastRefresh, reload: load } = useAsyncResource(fetchRows, [fetchRows], { initialData: { groups: [], files: [], egresses: [] } });
  const rows = data.groups || [];
  const instructionFiles = data.files || [];
  const egressOptions = egressOptionList(data.egresses || []);
  const groupMetrics = [
    { label: '分组数', value: rows.length },
    { label: '强制模型', value: rows.filter((row) => row.force_model).length },
    { label: '推理强度', value: rows.filter((row) => row.force_effort).length },
    { label: '默认出口', value: rows.filter((row) => row.default_egress_id).length },
    { label: '模型指令', value: rows.filter((row) => row.model_instructions_enabled).length, tone: 'success' },
  ];

  const { run: create, running: creating } = useAsyncAction(async (values) => {
    try { await post('/admin/groups', cleanGroupValues(values)); Toast.success('已创建'); setOpen(false); await load(); }
    catch (e) { showErrorToast(e); }
  });

  const { run: saveInstruction, running: savingInstruction } = useAsyncAction(async () => {
    try {
      await post('/admin/model-instructions', { name: instructionName, content: instructionContent });
      Toast.success('模型指令文件已保存');
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const openInstructionEditor = useCallback((group) => {
    setEditingGroup({ ...group });
    setEditFiles(normalizedFileList(group.model_instructions_files));
  }, []);

  const toggleEditFile = useCallback((name) => {
    setEditFiles((current) => (
      current.includes(name)
        ? current.filter((item) => item !== name)
        : [...current, name]
    ));
  }, []);

  const moveEditFile = useCallback((index, delta) => {
    setEditFiles((current) => {
      const next = [...current];
      const target = index + delta;
      if (target < 0 || target >= next.length) return current;
      [next[index], next[target]] = [next[target], next[index]];
      return next;
    });
  }, []);

  const { run: saveGroupInstructions, running: savingGroupInstructions } = useAsyncAction(async () => {
    if (!editingGroup?.name) return;
    try {
      await patch(`/admin/groups/${encodeURIComponent(editingGroup.name)}`, {
        model_instructions_enabled: !!editingGroup.model_instructions_enabled,
        model_instructions_files: editFiles,
      });
      Toast.success('分组模型指令已保存');
      setEditingGroup(null);
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const { run: saveGroupPolicy, running: savingGroupPolicy } = useAsyncAction(async (values) => {
    if (!policyEditor?.name) return;
    try {
      await patch(`/admin/groups/${encodeURIComponent(policyEditor.name)}`, cleanGroupPolicyValues(values));
      Toast.success('分组策略已保存');
      setPolicyEditor(null);
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const { run: remove, running: removing, isRunning: isRemoving } = useKeyedAsyncAction(async (name) => {
    try { await del(`/admin/groups/${encodeURIComponent(name)}`); Toast.success('已删除'); await load(); }
    catch (e) { showErrorToast(e); }
  });

  const columns = [
    {
      title: '分组',
      dataIndex: 'name',
      width: 260,
      render: (_, r) => (
          <div className="pool-resource-summary">
            <TextClamp strong>{r.name || '默认分组'}</TextClamp>
          <div className="pool-resource-summary__meta">
            {r.model_instructions_error ? `指令错误：${r.model_instructions_error}` : '账号策略按分组继承，可被账号或 Key 覆盖'}
          </div>
          </div>
      ),
    },
    {
      title: '策略',
      key: 'policy',
      width: 300,
      render: (_, r) => (
        <TagList
          items={groupPolicyTags(r)}
          max={4}
          renderItem={(item) => <Tag key={item.label} size="small" color={item.color}>{item.label}</Tag>}
        />
      ),
    },
    {
      title: '模型',
      dataIndex: 'force_model',
      width: 180,
      render: (v) => <TextClamp muted={!v}>{v || '继承默认'}</TextClamp>,
    },
    {
      title: '默认出口',
      dataIndex: 'default_egress_id',
      width: 180,
      render: (v) => <TextClamp muted={!v}>{v || '未设置'}</TextClamp>,
    },
    {
      title: '操作',
      key: 'ops',
      width: 116,
      render: (_, r) => (
        <ActionMenu
          label="分组操作"
          items={[
            {
              label: '编辑策略',
              disabled: creating || removing || savingGroupPolicy,
              onSelect: () => setPolicyEditor(r),
            },
            {
              label: '配置指令文件',
              disabled: creating || removing || savingGroupPolicy,
              onSelect: () => openInstructionEditor(r),
            },
            {
              label: isRemoving(r.name) ? '删除中' : '删除',
              destructive: true,
              disabled: creating || (removing && !isRemoving(r.name)),
              confirm: {
                title: `删除分组 ${r.name}?`,
                description: '删除分组会移除该分组配置，不会删除账号。',
                confirmText: '删除',
              },
              onSelect: () => remove(r.name),
            },
          ]}
        />
    ) },
  ];

  return (
    <div>
      <PageHeader title="分组" subtitle="按分组下发强制模型 / 推理强度 / Codex 模型指令文件"
        actions={<>
          <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
          <Button onClick={() => setInstructionLibraryOpen(true)}>模型指令文件</Button>
          <Button icon={<IconPlus />} theme="solid" disabled={removing} onClick={() => setOpen(true)}>新建分组</Button>
        </>} />
      <div className="pool-resource-split">
        <ResourceTable
          error={error}
          onRetry={load}
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={rows}
          columns={columns}
          rowKey="name"
          pagination={false}
          density="compact"
          layout="fit"
          className="pool-groups-table"
          scroll={false}
          rowHeight={68}
          emptyTitle="暂无分组"
          emptyType="groups"
          skeletonRows={5}
        />
        {!error || lastRefresh ? <MetricRail items={groupMetrics} /> : null}
      </div>
      <Modal title="模型指令文件" visible={instructionLibraryOpen} onCancel={() => { if (!savingInstruction) setInstructionLibraryOpen(false); }} footer={null} maskClosable={!savingInstruction}>
        <Form onSubmit={saveInstruction} labelPosition="top">
          <Form.Input field="instruction_name" label="保存名称" value={instructionName} onChange={setInstructionName} placeholder="coding-style.md" />
          <Form.TextArea field="instruction_content" label="内容" value={instructionContent} onChange={setInstructionContent} rows={10} />
          <Button htmlType="submit" theme="solid" loading={savingInstruction} disabled={!instructionName.trim()}>保存文件</Button>
        </Form>
        <div className="pool-instruction-files">
          {instructionFiles.length ? instructionFiles.map((file) => (
            <Tag key={file.name} size="small" color={file.error ? 'red' : 'blue'}>{file.name}</Tag>
          )) : <Tag size="small">暂无文件</Tag>}
        </div>
      </Modal>
      <Modal title="新建分组" visible={open} onCancel={() => { if (!creating) setOpen(false); }} footer={null} maskClosable={!creating}>
        <Form onSubmit={create}>
          <Form.Input field="name" label="分组名" rules={[{ required: true }]} />
          <Form.Input field="force_model" label="强制模型 (可选)" />
          <Form.Select field="force_effort" label="强制 effort (可选)" optionList={['', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra'].map((x) => ({ label: x || '不强制', value: x }))} />
          <Form.Select field="default_egress_id" label="默认出口" optionList={egressOptions} />
          <Form.Switch field="model_instructions_enabled" label="启用模型指令文件" />
          <Form.Input
            field="model_instructions_files_csv"
            label="指令文件顺序"
            placeholder={instructionFiles.map((file) => file.name).join(',') || 'coding-style.md,testing.txt'}
            help="逗号分隔，按填写顺序拼接；仅对 ChatGPT/Codex 路由生效。Codex 会话会固定创建时的快照，修改仅影响新会话。"
          />
          <Button htmlType="submit" theme="solid" loading={creating} style={{ marginTop: 12 }}>创建</Button>
        </Form>
      </Modal>
      <Modal
        title={`编辑分组策略 · ${policyEditor?.name || ''}`}
        visible={!!policyEditor}
        onCancel={() => { if (!savingGroupPolicy) setPolicyEditor(null); }}
        footer={null}
        maskClosable={!savingGroupPolicy}
      >
        {policyEditor ? (
          <Form
            initValues={{
              name: policyEditor.name,
              force_model: policyEditor.force_model || '',
              force_effort: policyEditor.force_effort || '',
              default_egress_id: policyEditor.default_egress_id || '',
            }}
            onSubmit={saveGroupPolicy}
          >
            <Form.Input field="name" label="分组名" disabled />
            <Form.Input field="force_model" label="强制模型 (可选)" />
            <Form.Select field="force_effort" label="强制 effort (可选)" optionList={['', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra'].map((x) => ({ label: x || '不强制', value: x }))} />
            <Form.Select field="default_egress_id" label="默认出口" optionList={egressOptions} />
            <Button htmlType="submit" theme="solid" loading={savingGroupPolicy} style={{ marginTop: 12 }}>保存</Button>
          </Form>
        ) : null}
      </Modal>
      <Modal
        title={`模型指令文件 · ${editingGroup?.name || ''}`}
        visible={!!editingGroup}
        onCancel={() => { if (!savingGroupInstructions) setEditingGroup(null); }}
        footer={null}
        maskClosable={!savingGroupInstructions}
      >
        <div className="pool-form">
          <label className="pool-field pool-field--left">
            <span className="pool-field__label">启用</span>
            <span>
              <Switch
                checked={!!editingGroup?.model_instructions_enabled}
                disabled={savingGroupInstructions}
                onChange={(checked) => setEditingGroup((current) => ({ ...(current || {}), model_instructions_enabled: checked }))}
              />
              <div className="pool-field__help">启用后覆盖 ChatGPT/Codex 的基础指令。Codex 会话在创建时固定快照，修改、删除文件或开关仅影响新会话。</div>
            </span>
          </label>
          <div className="pool-field pool-field--left">
            <span className="pool-field__label">多选文件</span>
            <span>
              <div style={{ display: 'grid', gap: 6 }}>
                {instructionFiles.length ? instructionFiles.map((file) => (
                  <label key={file.name} style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                    <input
                      type="checkbox"
                      checked={editFiles.includes(file.name)}
                      disabled={savingGroupInstructions}
                      onChange={() => toggleEditFile(file.name)}
                    />
                    <span>{file.name}</span>
                    {file.error ? <Tag size="small" color="red">{file.error}</Tag> : null}
                  </label>
                )) : <span className="pool-muted">暂无已保存文件，请先在右侧“模型指令文件”卡片保存 .md/.txt。</span>}
              </div>
              <div className="pool-field__help">勾选即挂载；下方列表顺序就是拼接顺序。</div>
            </span>
          </div>
          <div className="pool-field pool-field--left">
            <span className="pool-field__label">排序</span>
            <span>
              <div style={{ display: 'grid', gap: 6 }}>
                {editFiles.length ? editFiles.map((name, index) => (
                  <div key={name} style={{ display: 'flex', gap: 8, alignItems: 'center', justifyContent: 'space-between' }}>
                    <Tag size="small" color="blue">{index + 1}. {name}</Tag>
                    <span style={{ display: 'flex', gap: 6 }}>
                      <Button size="small" disabled={index === 0 || savingGroupInstructions} onClick={() => moveEditFile(index, -1)}>↑</Button>
                      <Button size="small" disabled={index === editFiles.length - 1 || savingGroupInstructions} onClick={() => moveEditFile(index, 1)}>↓</Button>
                    </span>
                  </div>
                )) : <span className="pool-muted">未选择文件</span>}
              </div>
            </span>
          </div>
          {editingGroup?.model_instructions_error ? (
            <Tag color="red">当前错误：{editingGroup.model_instructions_error}</Tag>
          ) : null}
          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 12 }}>
            <Button disabled={savingGroupInstructions} onClick={() => setEditingGroup(null)}>取消</Button>
            <Button theme="solid" loading={savingGroupInstructions} onClick={saveGroupInstructions}>保存</Button>
          </div>
        </div>
      </Modal>
    </div>
  );
}
