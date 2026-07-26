import React, { useCallback, useMemo, useState } from 'react';
import { ActionMenu, Banner, Button, Card, Form, Modal, Select, Switch, Tabs, TabPane, Tag, Toast } from '../components/pool/index.jsx';
import { IconPlus, IconRefresh } from '../components/pool/icons.jsx';
import { del, get, patch, post, put } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import { MetricRail, TagList, TextClamp } from '../components/DisplayPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import OrderedEgressSelect from '../components/OrderedEgressSelect.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useKeyedAsyncAction from '../hooks/useKeyedAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';

const TARGET_ACCOUNT_GROUP = 'account_pool_group';
const TARGET_PROVIDER = 'model_provider';
const EFFORTS = ['', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra'];

function rowsOf(value, keys = []) {
  if (Array.isArray(value)) return value;
  for (const key of keys) if (Array.isArray(value?.[key])) return value[key];
  return [];
}

function uniqueStrings(values) {
  const seen = new Set();
  return (Array.isArray(values) ? values : []).map((value) => String(value || '').trim()).filter((value) => {
    if (!value || seen.has(value)) return false;
    seen.add(value);
    return true;
  });
}

function groupEgressIDs(group) {
  if (Array.isArray(group?.egress_ids)) return uniqueStrings(group.egress_ids);
  return group?.default_egress_id ? [group.default_egress_id] : [];
}

function egressOptions(profiles) {
  const seen = new Set();
  const options = [];
  const add = (profile) => {
    const id = String(profile?.id || '').trim();
    if (!id || seen.has(id)) return;
    seen.add(id);
    options.push({ label: `${profile.name || id} (${profile.type || 'direct'})`, value: id });
  };
  add({ id: 'egress_direct', name: 'egress_direct', type: 'direct' });
  (profiles || []).forEach(add);
  return options;
}

function canonicalTarget(target) {
  if (target?.kind && target?.id) return { kind: target.kind, id: target.id };
  const legacy = target?.target_type;
  if (legacy === 'relay') return { kind: TARGET_PROVIDER, id: target.target_ref };
  return { kind: TARGET_ACCOUNT_GROUP, id: target?.target_ref || legacy || '' };
}

function targetKey(target) {
  const normalized = canonicalTarget(target);
  return `${normalized.kind}:${normalized.id}`;
}

function parseTargetKey(key) {
  const separator = String(key).indexOf(':');
  return separator > 0
    ? { kind: String(key).slice(0, separator), id: String(key).slice(separator + 1) }
    : { kind: TARGET_ACCOUNT_GROUP, id: String(key) };
}

function targetLabel(target, groups, providers) {
  const normalized = canonicalTarget(target);
  if (normalized.kind === TARGET_PROVIDER) {
    const provider = providers.find((item) => item.id === normalized.id);
    return provider?.name || provider?.display_name || normalized.id;
  }
  return groups.find((item) => item.name === normalized.id)?.name || normalized.id;
}

function providerSupportsModel(provider, model) {
  const requested = String(model || '').trim().toLowerCase();
  if (!requested || !provider || !Array.isArray(provider.models) || provider.models.length === 0) return true;
  return provider.models.some((raw) => {
    const candidate = String(raw || '').trim().toLowerCase();
    return candidate === '*' || candidate === requested || (candidate.endsWith('*') && requested.startsWith(candidate.slice(0, -1)));
  });
}

function targetSupportsModel(key, model, providers) {
  const target = parseTargetKey(key);
  return target.kind !== TARGET_PROVIDER || providerSupportsModel(providers.find((provider) => provider.id === target.id), model);
}

function blankUserGroup() {
  return {
    name: '',
    system_prompt: '',
    prompt_mode: 'prepend',
    system_prompt_apply_to_compaction: true,
    model_instructions_enabled: false,
    model_instructions_files: [],
    force_model: '',
    force_effort: '',
    target_keys: [],
    model_routing: [],
  };
}

function userGroupDraft(row) {
  if (!row) return blankUserGroup();
  return {
    ...blankUserGroup(),
    ...row,
    target_keys: (row.targets || []).map(targetKey),
    model_instructions_files: uniqueStrings(row.model_instructions_files),
    model_routing: (row.model_routing || []).map((rule) => ({
      model: rule.model || '',
      tiers: (rule.tiers || []).map((tier) => (tier || []).map(targetKey)),
    })),
  };
}

function normalizedUserGroupPayload(draft, providers = []) {
  const selected = new Set(uniqueStrings(draft.target_keys));
  const targets = [...selected].map(parseTargetKey);
  return {
    name: String(draft.name || '').trim(),
    system_prompt: String(draft.system_prompt || ''),
    prompt_mode: draft.prompt_mode || 'prepend',
    system_prompt_apply_to_compaction: Boolean(draft.system_prompt_apply_to_compaction),
    model_instructions_enabled: Boolean(draft.model_instructions_enabled),
    model_instructions_files: uniqueStrings(draft.model_instructions_files),
    force_model: String(draft.force_model || '').trim(),
    force_effort: String(draft.force_effort || '').trim(),
    targets,
    model_routing: (draft.model_routing || []).filter((rule) => String(rule.model || '').trim()).map((rule) => {
      const mentioned = new Set();
      const tiers = (rule.tiers || []).map((tier) => uniqueStrings(tier).filter((key) => {
        if (!selected.has(key) || mentioned.has(key) || !targetSupportsModel(key, rule.model, providers)) return false;
        mentioned.add(key);
        return true;
      }).map(parseTargetKey)).filter((tier) => tier.length);
      return { model: String(rule.model).trim(), tiers };
    }),
  };
}

function formatTime(value) {
  const timestamp = Number(value) || 0;
  if (!timestamp) return '尚无记录';
  return new Date(timestamp * 1000).toLocaleString();
}

function AccountGroupEditor({ editor, profiles, saving, onCancel, onSave }) {
  const [name, setName] = useState(editor?.row?.name || '');
  const [selectedEgresses, setSelectedEgresses] = useState(groupEgressIDs(editor?.row));
  const options = useMemo(() => egressOptions(profiles), [profiles]);
  const editing = editor?.mode === 'edit';
  return (
    <div className="pool-form pool-group-editor">
      <Form.Input label="分组名" value={name} onChange={setName} disabled={editing} placeholder="例如：codex-primary" />
      <div className="pool-field pool-field--top">
        <span className="pool-field__label">有序出口</span>
        <OrderedEgressSelect
          value={selectedEgresses}
          onChange={setSelectedEgresses}
          options={options}
          disabled={saving}
          help="账号导入或移动到此分组后，请求时动态继承这里的出口顺序；不会复制到账号记录。"
        />
      </div>
      {editing ? (
        <Banner
          type="info"
          title={`${editor.row.account_count || 0} 个账号动态继承`}
          description="首项为主出口，其余按顺序作为备用出口。修改后下一次请求立即使用新顺序。"
        />
      ) : null}
      <div className="pool-modal-actions">
        <Button onClick={onCancel} disabled={saving}>取消</Button>
        <Button theme="solid" loading={saving} disabled={!name.trim()} onClick={() => onSave({ name: name.trim(), egress_ids: selectedEgresses })}>保存</Button>
      </div>
    </div>
  );
}

function ModelRoutingEditor({ draft, setDraft, targetOptions, providers }) {
  const updateRule = (index, patchValue) => setDraft((current) => ({
    ...current,
    model_routing: current.model_routing.map((rule, ruleIndex) => ruleIndex === index ? { ...rule, ...patchValue } : rule),
  }));
  const updateRuleModel = (index, model) => setDraft((current) => ({
    ...current,
    model_routing: current.model_routing.map((rule, ruleIndex) => ruleIndex === index ? {
      ...rule,
      model,
      tiers: (rule.tiers || []).map((tier) => tier.filter((key) => targetSupportsModel(key, model, providers))),
    } : rule),
  }));
  const removeRule = (index) => setDraft((current) => ({ ...current, model_routing: current.model_routing.filter((_, ruleIndex) => ruleIndex !== index) }));

  return (
    <section className="pool-user-routing-editor">
      <div className="pool-user-routing-editor__head">
        <div>
          <strong>模型目标优先层级</strong>
          <div className="pool-field__help">层内目标等权；先穷尽当前层，再进入下一层。未写入规则的兼容目标会自动成为最后备用层。</div>
        </div>
        <Button onClick={() => setDraft((current) => ({ ...current, model_routing: [...current.model_routing, { model: '', tiers: [[]] }] }))}>添加模型规则</Button>
      </div>
      {draft.model_routing.length ? draft.model_routing.map((rule, ruleIndex) => {
        const compatibleOptions = targetOptions.map((option) => ({
          ...option,
          disabled: !targetSupportsModel(option.value, rule.model, providers),
        }));
        const incompatible = draft.target_keys.filter((key) => !targetSupportsModel(key, rule.model, providers));
        const assigned = new Set((rule.tiers || []).flat());
        const fallback = draft.target_keys.filter((key) => targetSupportsModel(key, rule.model, providers) && !assigned.has(key));
        return (
          <Card key={ruleIndex} className="pool-route-rule-card">
            <div className="pool-route-rule-card__head">
              <Form.Input label={`逻辑模型 ${ruleIndex + 1}`} value={rule.model} onChange={(model) => updateRuleModel(ruleIndex, model)} placeholder="gpt-5.3-codex" />
              <Button type="danger" onClick={() => removeRule(ruleIndex)}>删除规则</Button>
            </div>
            {(rule.tiers || []).map((tier, tierIndex) => (
              <div className="pool-route-tier" key={tierIndex}>
                <Tag color={tierIndex === 0 ? 'green' : 'blue'}>{tierIndex === 0 ? '优先层' : `备用层 ${tierIndex}`}</Tag>
                <Select
                  multiple
                  filter
                  value={tier}
                  onChange={(values) => updateRule(ruleIndex, {
                    tiers: rule.tiers.map((item, index) => index === tierIndex ? values : item.filter((key) => !values.includes(key))),
                  })}
                  optionList={compatibleOptions}
                  placeholder="选择本层等权目标"
                  style={{ width: '100%' }}
                />
                <div className="pool-route-tier__actions">
                  <Button size="small" disabled={tierIndex === 0} onClick={() => {
                    const tiers = [...rule.tiers];
                    [tiers[tierIndex - 1], tiers[tierIndex]] = [tiers[tierIndex], tiers[tierIndex - 1]];
                    updateRule(ruleIndex, { tiers });
                  }}>↑</Button>
                  <Button size="small" disabled={tierIndex === rule.tiers.length - 1} onClick={() => {
                    const tiers = [...rule.tiers];
                    [tiers[tierIndex + 1], tiers[tierIndex]] = [tiers[tierIndex], tiers[tierIndex + 1]];
                    updateRule(ruleIndex, { tiers });
                  }}>↓</Button>
                  <Button size="small" type="danger" onClick={() => updateRule(ruleIndex, { tiers: rule.tiers.filter((_, index) => index !== tierIndex) })}>移除层</Button>
                </div>
              </div>
            ))}
            <Button size="small" onClick={() => updateRule(ruleIndex, { tiers: [...(rule.tiers || []), []] })}>添加备用层</Button>
            <div className="pool-route-compatibility">
              {incompatible.map((key) => <Tag size="small" color="red" key={key}>{targetOptions.find((option) => option.value === key)?.label || key} 不兼容</Tag>)}
              {fallback.length ? <span className="pool-muted">自动末级备用：{fallback.map((key) => targetOptions.find((option) => option.value === key)?.label || key).join('、')}</span> : null}
            </div>
          </Card>
        );
      }) : <Banner type="info" title="默认等权路由" description="未配置模型规则时，所有兼容目标处于同一等权层。" />}
    </section>
  );
}

function UserGroupEditor({ editor, groups, providers, instructionFiles, saving, onCancel, onSave }) {
  const [draft, setDraft] = useState(() => userGroupDraft(editor?.row));
  const targetOptions = useMemo(() => [
    ...groups.map((group) => ({ label: `账号池分组 · ${group.name}`, value: `${TARGET_ACCOUNT_GROUP}:${group.name}` })),
    ...providers.map((provider) => ({ label: `模型提供商 · ${provider.name || provider.id}`, value: `${TARGET_PROVIDER}:${provider.id}` })),
  ], [groups, providers]);
  const selectedTargetOptions = targetOptions.filter((option) => draft.target_keys.includes(option.value));
  const payload = normalizedUserGroupPayload(draft, providers);

  const setTargets = (targetKeys) => setDraft((current) => {
    const selected = new Set(targetKeys);
    return {
      ...current,
      target_keys: targetKeys,
      model_routing: current.model_routing.map((rule) => ({
        ...rule,
        tiers: rule.tiers.map((tier) => tier.filter((key) => selected.has(key))),
      })),
    };
  });

  return (
    <div className="pool-user-group-editor">
      <div className="pool-user-group-grid">
        <Form.Input label="用户分组名" value={draft.name} onChange={(name) => setDraft((current) => ({ ...current, name }))} placeholder="例如：team-coding" />
        <div className="pool-field pool-field--top">
          <span className="pool-field__label">路由目标</span>
          <Select
            multiple
            filter
            maxTagCount={8}
            value={draft.target_keys}
            onChange={setTargets}
            optionList={targetOptions}
            placeholder="混合选择账号池分组和模型提供商"
            style={{ width: '100%' }}
          />
          <div className="pool-field__help">可只选账号池分组、只选模型提供商，或任意混合；不能直接绑定账号或出口。</div>
        </div>
      </div>
      <Card title="指令与模型策略" className="pool-card">
        <Form.TextArea label="系统提示词" value={draft.system_prompt} onChange={(system_prompt) => setDraft((current) => ({ ...current, system_prompt }))} rows={5} placeholder="留空则不注入额外系统提示" />
        <div className="pool-user-group-grid">
          <Form.Select label="提示词模式" value={draft.prompt_mode} onChange={(prompt_mode) => setDraft((current) => ({ ...current, prompt_mode }))} optionList={[{ label: '前置（prepend）', value: 'prepend' }, { label: '替换（replace）', value: 'replace' }]} />
          <label className="pool-inline-switch">
            <Switch checked={draft.system_prompt_apply_to_compaction} onChange={(value) => setDraft((current) => ({ ...current, system_prompt_apply_to_compaction: value }))} />
            <span>压缩上下文时继续应用</span>
          </label>
          <Form.Input label="强制模型（可选）" value={draft.force_model} onChange={(force_model) => setDraft((current) => ({ ...current, force_model }))} placeholder="尊重客户端模型" />
          <Form.Select label="强制 effort（可选）" value={draft.force_effort} onChange={(force_effort) => setDraft((current) => ({ ...current, force_effort }))} optionList={EFFORTS.map((value) => ({ label: value || '不强制', value }))} />
        </div>
        <label className="pool-inline-switch">
          <Switch checked={draft.model_instructions_enabled} onChange={(value) => setDraft((current) => ({ ...current, model_instructions_enabled: value }))} />
          <span>启用指令文件</span>
        </label>
        <Select
          multiple
          filter
          value={draft.model_instructions_files}
          onChange={(model_instructions_files) => setDraft((current) => ({ ...current, model_instructions_files }))}
          optionList={instructionFiles.map((file) => ({ label: file.error ? `${file.name}（异常）` : file.name, value: file.name, disabled: Boolean(file.error) }))}
          placeholder="选择指令文件"
          disabled={!draft.model_instructions_enabled}
          style={{ width: '100%', marginTop: 8 }}
        />
      </Card>
      <ModelRoutingEditor draft={draft} setDraft={setDraft} targetOptions={selectedTargetOptions} providers={providers} />
      <Card title="最终路由摘要" className="pool-card pool-routing-summary">
        {payload.targets.length ? (
          <>
            <div>默认：{payload.targets.map((target) => targetLabel(target, groups, providers)).join(' ⇄ ')}</div>
            {payload.model_routing.map((rule) => (
              <div key={rule.model}>
                <strong>{rule.model}</strong>：{rule.tiers.length
                  ? rule.tiers.map((tier) => tier.map((target) => targetLabel(target, groups, providers)).join(' ⇄ ')).join(' → ')
                  : '所有兼容目标等权'}
              </div>
            ))}
          </>
        ) : <span className="pool-muted">至少选择一个目标后显示路由摘要。</span>}
      </Card>
      <div className="pool-modal-actions">
        <Button onClick={onCancel} disabled={saving}>取消</Button>
        <Button theme="solid" loading={saving} disabled={!payload.name || payload.targets.length === 0} onClick={() => onSave(payload)}>保存用户分组</Button>
      </div>
    </div>
  );
}

export default function Groups() {
  const [activeTab, setActiveTab] = useState('account_pool');
  const [accountEditor, setAccountEditor] = useState(null);
  const [userEditor, setUserEditor] = useState(null);
  const [instructionLibraryOpen, setInstructionLibraryOpen] = useState(false);
  const [instructionName, setInstructionName] = useState('');
  const [instructionContent, setInstructionContent] = useState('');

  const fetchRows = useCallback(async ({ signal }) => {
    const [groups, instructions, egresses, userGroups, providers] = await Promise.all([
      get('/admin/groups', undefined, { signal }),
      get('/admin/model-instructions', undefined, { signal }),
      get('/admin/egress-profiles', undefined, { signal }),
      get('/admin/user-groups', undefined, { signal }),
      get('/admin/providers', undefined, { signal }),
    ]);
    return {
      groups: rowsOf(groups, ['groups']),
      instructions: rowsOf(instructions, ['files']),
      egresses: rowsOf(egresses, ['profiles', 'egress_profiles']),
      userGroups: rowsOf(userGroups, ['user_groups']),
      providers: rowsOf(providers, ['providers']),
    };
  }, []);
  const emptyData = { groups: [], instructions: [], egresses: [], userGroups: [], providers: [] };
  const { data = emptyData, loading, error, lastRefresh, reload: load } = useAsyncResource(fetchRows, [fetchRows], { initialData: emptyData });

  const { run: saveAccountGroup, running: savingAccountGroup } = useAsyncAction(async (values) => {
    try {
      if (accountEditor?.mode === 'edit') await patch(`/admin/groups/${encodeURIComponent(accountEditor.row.name)}`, { egress_ids: values.egress_ids });
      else await post('/admin/groups', values);
      Toast.success(accountEditor?.mode === 'edit' ? '账号池分组已更新' : '账号池分组已创建');
      setAccountEditor(null);
      void load();
    } catch (saveError) { showErrorToast(saveError); }
  });

  const { run: saveUserGroup, running: savingUserGroup } = useAsyncAction(async (payload) => {
    try {
      if (userEditor?.mode === 'edit') await put(`/admin/user-groups/${encodeURIComponent(userEditor.row.id)}`, payload);
      else await post('/admin/user-groups', payload);
      Toast.success(userEditor?.mode === 'edit' ? '用户分组已更新' : '用户分组已创建');
      setUserEditor(null);
      void load();
    } catch (saveError) { showErrorToast(saveError); }
  });

  const { run: removeAccountGroup, running: removingAccountGroup, isRunning: isRemovingAccountGroup } = useKeyedAsyncAction(async (name) => {
    try {
      await del(`/admin/groups/${encodeURIComponent(name)}`);
      Toast.success('账号池分组已删除');
      void load();
    } catch (removeError) { showErrorToast(removeError); }
  });

  const { run: removeUserGroup, running: removingUserGroup, isRunning: isRemovingUserGroup } = useKeyedAsyncAction(async (id) => {
    try {
      await del(`/admin/user-groups/${encodeURIComponent(id)}`);
      Toast.success('用户分组已删除');
      void load();
    } catch (removeError) { showErrorToast(removeError); }
  });

  const { run: saveInstruction, running: savingInstruction } = useAsyncAction(async () => {
    try {
      await post('/admin/model-instructions', { name: instructionName.trim(), content: instructionContent });
      Toast.success('指令文件已保存');
      setInstructionName('');
      setInstructionContent('');
      void load();
    } catch (saveError) { showErrorToast(saveError); }
  });

  const accountColumns = [
    {
      title: '账号池分组', dataIndex: 'name', width: 220,
      render: (value, row) => (
        <div className="pool-resource-summary">
          <TextClamp strong>{value || '默认分组'}</TextClamp>
          <div className="pool-resource-summary__meta">{row.account_count || 0} 个账号 · 请求时动态继承出口</div>
        </div>
      ),
    },
    {
      title: '健康', key: 'health', width: 150,
      render: (_, row) => {
        const total = Number(row.account_count) || 0;
        const active = Number(row.active_account_count) || 0;
        const rate = total ? Math.round(active / total * 100) : 0;
        return <Tag color={rate >= 80 ? 'green' : rate > 0 ? 'orange' : 'grey'}>{active}/{total} · {rate}%</Tag>;
      },
    },
    {
      title: '有序出口', key: 'egresses',
      render: (_, row) => {
        const ids = groupEgressIDs(row);
        return ids.length ? <TagList items={ids.map((id, index) => `${index === 0 ? '主' : `备${index}`} · ${id}`)} max={4} /> : <span className="pool-muted">系统默认</span>;
      },
    },
    { title: '最近测活/更新', key: 'updated', width: 190, render: (_, row) => <TextClamp muted>{formatTime(row.last_probe_at || row.updated_at)}</TextClamp> },
    {
      title: '操作', key: 'ops', width: 100,
      render: (_, row) => (
        <ActionMenu label="账号池分组操作" items={[
          { label: '编辑出口', disabled: savingAccountGroup || removingAccountGroup, onSelect: () => setAccountEditor({ mode: 'edit', row }) },
          {
            label: isRemovingAccountGroup(row.name) ? '删除中' : '删除', destructive: true,
            disabled: savingAccountGroup || (removingAccountGroup && !isRemovingAccountGroup(row.name)),
            confirm: { title: `删除账号池分组 ${row.name}？`, description: '请先移动该分组内的账号。出口配置不会被删除。', confirmText: '删除' },
            onSelect: () => removeAccountGroup(row.name),
          },
        ]} />
      ),
    },
  ];

  const userColumns = [
    {
      title: '用户分组', dataIndex: 'name', width: 210,
      render: (value, row) => (
        <div className="pool-resource-summary">
          <TextClamp strong>{value}</TextClamp>
          <div className="pool-resource-summary__meta">{row.force_model ? `强制 ${row.force_model}` : '尊重客户端模型'}{row.force_effort ? ` · ${row.force_effort}` : ''}</div>
        </div>
      ),
    },
    {
      title: '混合目标', dataIndex: 'targets',
      render: (targets) => (
        <TagList
          items={(targets || []).map((target) => ({
            label: `${canonicalTarget(target).kind === TARGET_PROVIDER ? '提供商' : '账号池'} · ${targetLabel(target, data.groups, data.providers)}`,
            color: canonicalTarget(target).kind === TARGET_PROVIDER ? 'violet' : 'blue',
          }))}
          max={4}
          renderItem={(item) => <Tag key={item.label} size="small" color={item.color}>{item.label}</Tag>}
        />
      ),
    },
    { title: '模型规则', dataIndex: 'model_routing', width: 110, render: (rules) => <Tag color={rules?.length ? 'green' : 'grey'}>{rules?.length || 0} 条</Tag> },
    {
      title: '策略', key: 'policy', width: 210,
      render: (_, row) => <TagList items={[
        row.system_prompt ? '系统提示词' : '',
        row.model_instructions_enabled ? `指令 ${row.model_instructions_files?.length || 0}` : '',
        row.system_prompt_apply_to_compaction ? '压缩时应用' : '',
      ].filter(Boolean)} max={3} />,
    },
    {
      title: '操作', key: 'ops', width: 100,
      render: (_, row) => (
        <ActionMenu label="用户分组操作" items={[
          { label: '编辑完整策略', disabled: savingUserGroup || removingUserGroup, onSelect: () => setUserEditor({ mode: 'edit', row }) },
          {
            label: isRemovingUserGroup(row.id) ? '删除中' : '删除', destructive: true,
            disabled: savingUserGroup || (removingUserGroup && !isRemovingUserGroup(row.id)),
            confirm: { title: `删除用户分组 ${row.name}？`, description: '关联此分组的 API Key 将无法继续使用该路由策略。', confirmText: '删除' },
            onSelect: () => removeUserGroup(row.id),
          },
        ]} />
      ),
    },
  ];

  const accountMetrics = [
    { label: '账号池分组', value: data.groups.length },
    { label: '账号总数', value: data.groups.reduce((sum, row) => sum + (Number(row.account_count) || 0), 0) },
    { label: '健康账号', value: data.groups.reduce((sum, row) => sum + (Number(row.active_account_count) || 0), 0), tone: 'success' },
    { label: '多出口分组', value: data.groups.filter((row) => groupEgressIDs(row).length > 1).length },
  ];
  const userMetrics = [
    { label: '用户分组', value: data.userGroups.length },
    { label: '混合目标', value: data.userGroups.filter((row) => new Set((row.targets || []).map((target) => canonicalTarget(target).kind)).size > 1).length },
    { label: '模型分层', value: data.userGroups.filter((row) => row.model_routing?.length).length, tone: 'success' },
  ];

  return (
    <div>
      <PageHeader
        title="分组"
        subtitle="账号池分组管理账号与出口；用户分组管理指令、模型策略与目标层级。"
        actions={(
          <>
            <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
            {activeTab === 'user' ? <Button onClick={() => setInstructionLibraryOpen(true)}>指令文件库</Button> : null}
            <Button
              icon={<IconPlus />}
              theme="solid"
              onClick={() => activeTab === 'user' ? setUserEditor({ mode: 'create', row: null }) : setAccountEditor({ mode: 'create', row: null })}
            >
              {activeTab === 'user' ? '新建用户分组' : '新建账号池分组'}
            </Button>
          </>
        )}
      />
      <Tabs activeKey={activeTab} onChange={setActiveTab} type="line">
        <TabPane key="account_pool" tab="账号池分组" itemKey="account_pool">
          <Banner type="info" title="动态出口继承" description="账号记录不保存分组出口副本。出口列表首项为主出口，其余按顺序备用；用户指令与模型策略请在用户分组中配置。" />
          <div className="pool-resource-split pool-group-resource-split">
            <ResourceTable error={error} onRetry={load} loading={loading} lastRefresh={lastRefresh} dataSource={data.groups} columns={accountColumns} rowKey="name" pagination={false} density="compact" layout="fit" scroll={false} rowHeight={68} emptyTitle="暂无账号池分组" skeletonRows={5} />
            {!error || lastRefresh ? <MetricRail items={accountMetrics} /> : null}
          </div>
        </TabPane>
        <TabPane key="user" tab="用户分组" itemKey="user">
          <div className="pool-resource-split pool-group-resource-split">
            <ResourceTable error={error} onRetry={load} loading={loading} lastRefresh={lastRefresh} dataSource={data.userGroups} columns={userColumns} rowKey="id" pagination={false} density="compact" layout="fit" scroll={false} rowHeight={68} emptyTitle="暂无用户分组" emptyDescription="创建后可混合选择账号池分组与模型提供商，并按模型设置优先层级。" skeletonRows={5} />
            {!error || lastRefresh ? <MetricRail items={userMetrics} /> : null}
          </div>
        </TabPane>
      </Tabs>

      <Modal title={accountEditor?.mode === 'edit' ? `编辑账号池分组 · ${accountEditor.row.name}` : '新建账号池分组'} visible={Boolean(accountEditor)} onCancel={() => { if (!savingAccountGroup) setAccountEditor(null); }} footer={null} width={700} maskClosable={!savingAccountGroup}>
        {accountEditor ? <AccountGroupEditor key={`${accountEditor.mode}:${accountEditor.row?.name || 'new'}`} editor={accountEditor} profiles={data.egresses} saving={savingAccountGroup} onCancel={() => setAccountEditor(null)} onSave={saveAccountGroup} /> : null}
      </Modal>
      <Modal title={userEditor?.mode === 'edit' ? `编辑用户分组 · ${userEditor.row.name}` : '新建用户分组'} visible={Boolean(userEditor)} onCancel={() => { if (!savingUserGroup) setUserEditor(null); }} footer={null} width={960} maskClosable={!savingUserGroup}>
        {userEditor ? <UserGroupEditor key={`${userEditor.mode}:${userEditor.row?.id || 'new'}`} editor={userEditor} groups={data.groups} providers={data.providers} instructionFiles={data.instructions} saving={savingUserGroup} onCancel={() => setUserEditor(null)} onSave={saveUserGroup} /> : null}
      </Modal>
      <Modal title="用户分组指令文件库" visible={instructionLibraryOpen} onCancel={() => { if (!savingInstruction) setInstructionLibraryOpen(false); }} footer={null} maskClosable={!savingInstruction}>
        <Form onSubmit={saveInstruction} labelPosition="top">
          <Form.Input label="文件名" value={instructionName} onChange={setInstructionName} placeholder="coding-style.md" />
          <Form.TextArea label="内容" value={instructionContent} onChange={setInstructionContent} rows={10} />
          <Button htmlType="submit" theme="solid" loading={savingInstruction} disabled={!instructionName.trim()}>保存文件</Button>
        </Form>
        <div className="pool-instruction-files">
          {data.instructions.length ? data.instructions.map((file) => <Tag key={file.name} color={file.error ? 'red' : 'blue'}>{file.name}</Tag>) : <span className="pool-muted">暂无指令文件</span>}
        </div>
      </Modal>
    </div>
  );
}
