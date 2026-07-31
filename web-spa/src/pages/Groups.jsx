import React, { useCallback, useId, useMemo, useState } from 'react';
import * as PopoverPrimitive from '@radix-ui/react-popover';
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
const INSTRUCTION_FAMILIES = [
  { key: 'gpt', label: 'GPT / Codex', help: '用于 GPT 与 Codex 模型；Responses 入口写入 model instructions。' },
  { key: 'claude', label: 'Claude', help: '用于原生 Claude、Claude Code，以及 Kiro / Antigravity 中的 Claude 模型。' },
  { key: 'gemini', label: 'Gemini', help: '用于 Antigravity 等目标中的 Gemini 模型，并进入其 systemInstruction。' },
];
const FALLBACK_FAMILIES = INSTRUCTION_FAMILIES.map(({ key, label }) => ({ key, label }));

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

function blankTrafficFallbackGroups() {
  return Object.fromEntries(FALLBACK_FAMILIES.map(({ key }) => [key, []]));
}

function modelFamily(model) {
  const value = String(model || '').trim().toLowerCase();
  if (/^(claude(?:-|$)|opus$|sonnet$|haiku$|fable$)/.test(value)) return 'claude';
  if (/^gemini(?:-|$)/.test(value)) return 'gemini';
  if (/^(gpt(?:-|$)|chatgpt(?:-|$)|codex(?:-|$)|o[134])/.test(value)) return 'gpt';
  return '';
}

function modelsByFamily(models) {
  const output = { gpt: [], claude: [], gemini: [], other: [] };
  uniqueStrings(models).forEach((model) => {
    const family = modelFamily(model);
    output[family || 'other'].push(model);
  });
  return output;
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
    model_instruction_profiles: Object.fromEntries(INSTRUCTION_FAMILIES.map(({ key }) => [key, { enabled: false, files: [] }])),
    force_model: '',
    force_effort: '',
    block_claude_target_groups: [],
    block_gpt_target_groups: [],
    traffic_fallback_groups: blankTrafficFallbackGroups(),
    traffic_fallback_model_mappings: [],
    target_keys: [],
    model_routing: [],
  };
}

function normalizedInstructionProfiles(row) {
  const configured = row?.model_instruction_profiles && typeof row.model_instruction_profiles === 'object'
    ? row.model_instruction_profiles
    : null;
  const hasProfiles = configured && Object.keys(configured).length > 0;
  return Object.fromEntries(INSTRUCTION_FAMILIES.map(({ key }) => {
    const profile = hasProfiles ? configured[key] : null;
    return [key, {
      enabled: hasProfiles ? Boolean(profile?.enabled) : Boolean(row?.model_instructions_enabled),
      files: hasProfiles ? uniqueStrings(profile?.files) : uniqueStrings(row?.model_instructions_files),
    }];
  }));
}

function userGroupDraft(row) {
  if (!row) return blankUserGroup();
  return {
    ...blankUserGroup(),
    ...row,
    target_keys: (row.targets || []).map(targetKey),
    model_instructions_files: uniqueStrings(row.model_instructions_files),
    model_instruction_profiles: normalizedInstructionProfiles(row),
    block_claude_target_groups: uniqueStrings(row.block_claude_target_groups),
    block_gpt_target_groups: uniqueStrings(row.block_gpt_target_groups),
    traffic_fallback_groups: Object.fromEntries(FALLBACK_FAMILIES.map(({ key }) => [
      key,
      uniqueStrings(row.traffic_fallback_groups?.[key]),
    ])),
    traffic_fallback_model_mappings: (row.traffic_fallback_model_mappings || []).map((mapping) => ({
      family: String(mapping.family || '').trim().toLowerCase(),
      source_model: String(mapping.source_model || ''),
      target_user_group_id: String(mapping.target_user_group_id || ''),
      target_model: String(mapping.target_model || ''),
    })),
    model_routing: (row.model_routing || []).map((rule) => ({
      model: rule.model || '',
      tiers: (rule.tiers || []).map((tier) => (tier || []).map(targetKey)),
    })),
  };
}

function normalizedUserGroupPayload(draft, providers = []) {
  const selected = new Set(uniqueStrings(draft.target_keys));
  const targets = [...selected].map(parseTargetKey);
  const selectedAccountGroups = new Set(targets
    .filter((target) => target.kind === TARGET_ACCOUNT_GROUP)
    .map((target) => target.id));
  const profiles = Object.fromEntries(INSTRUCTION_FAMILIES.map(({ key }) => [key, {
    enabled: Boolean(draft.model_instruction_profiles?.[key]?.enabled),
    files: uniqueStrings(draft.model_instruction_profiles?.[key]?.files),
  }]));
  const enabledProfiles = Object.values(profiles).filter((profile) => profile.enabled);
  const legacyFiles = uniqueStrings(enabledProfiles.flatMap((profile) => profile.files));
  const trafficFallbackGroups = Object.fromEntries(FALLBACK_FAMILIES.map(({ key }) => [
    key,
    uniqueStrings(draft.traffic_fallback_groups?.[key]),
  ]));
  const trafficFallbackModelMappings = (draft.traffic_fallback_model_mappings || [])
    .map((mapping) => ({
      family: String(mapping.family || '').trim().toLowerCase(),
      source_model: String(mapping.source_model || '').trim(),
      target_user_group_id: String(mapping.target_user_group_id || '').trim(),
      target_model: String(mapping.target_model || '').trim(),
    }))
    .filter((mapping) => FALLBACK_FAMILIES.some(({ key }) => key === mapping.family)
      && trafficFallbackGroups[mapping.family].includes(mapping.target_user_group_id)
      && mapping.source_model
      && mapping.target_model);
  return {
    name: String(draft.name || '').trim(),
    system_prompt: String(draft.system_prompt || ''),
    prompt_mode: draft.prompt_mode || 'prepend',
    system_prompt_apply_to_compaction: Boolean(draft.system_prompt_apply_to_compaction),
    model_instructions_enabled: enabledProfiles.length > 0,
    model_instructions_files: legacyFiles,
    model_instruction_profiles: profiles,
    force_model: String(draft.force_model || '').trim(),
    force_effort: String(draft.force_effort || '').trim(),
    block_claude_target_groups: uniqueStrings(draft.block_claude_target_groups)
      .filter((groupName) => selectedAccountGroups.has(groupName)),
    block_gpt_target_groups: uniqueStrings(draft.block_gpt_target_groups)
      .filter((groupName) => selectedAccountGroups.has(groupName)),
    traffic_fallback_groups: trafficFallbackGroups,
    traffic_fallback_model_mappings: trafficFallbackModelMappings,
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

function fallbackConfigurationIssues(draft) {
  const mappings = Array.isArray(draft.traffic_fallback_model_mappings) ? draft.traffic_fallback_model_mappings : [];
  const issues = [];
  const seenMappings = new Set();
  FALLBACK_FAMILIES.forEach(({ key, label }) => {
    uniqueStrings(draft.traffic_fallback_groups?.[key]).forEach((groupID) => {
      const configured = mappings.some((mapping) => mapping.family === key
        && mapping.target_user_group_id === groupID
        && String(mapping.source_model || '').trim()
        && String(mapping.target_model || '').trim());
      if (!configured) issues.push(`${label} · ${groupID} 尚未配置模型转换`);
    });
  });
  mappings.forEach((mapping, index) => {
    const sourceModel = String(mapping.source_model || '').trim();
    const targetModel = String(mapping.target_model || '').trim();
    if (!mapping.family || !mapping.target_user_group_id || !sourceModel || !targetModel) {
      issues.push(`模型转换 ${index + 1} 尚未填写完整`);
      return;
    }
    if (!uniqueStrings(draft.traffic_fallback_groups?.[mapping.family]).includes(mapping.target_user_group_id)) {
      issues.push(`模型转换 ${index + 1} 的兜底分组未在 ${mapping.family} 家族中勾选`);
    }
    if (sourceModel.slice(0, -1).includes('*')) {
      issues.push(`模型转换 ${index + 1} 仅支持末尾通配符`);
    }
    if (sourceModel.length > 200 || targetModel.length > 200) {
      issues.push(`模型转换 ${index + 1} 的模型名不能超过 200 个字符`);
    }
    const key = `${mapping.family}\u0000${sourceModel.toLowerCase()}\u0000${mapping.target_user_group_id}`;
    if (seenMappings.has(key)) {
      issues.push(`模型转换 ${index + 1} 与已有规则重复`);
    }
    seenMappings.add(key);
  });
  return uniqueStrings(issues);
}

function TrafficFallbackGroupSelector({ value, onChange, options, disabled }) {
  const [open, setOpen] = useState(false);
  const [activeFamily, setActiveFamily] = useState('gpt');
  const [query, setQuery] = useState('');
  const triggerID = useId();
  const normalized = Object.fromEntries(FALLBACK_FAMILIES.map(({ key }) => [key, uniqueStrings(value?.[key])]));
  const selectedCount = FALLBACK_FAMILIES.reduce((sum, { key }) => sum + normalized[key].length, 0);
  const activeSelected = new Set(normalized[activeFamily]);
  const filteredOptions = options.filter((option) => {
    const needle = query.trim().toLowerCase();
    return !needle || `${option.label} ${option.id}`.toLowerCase().includes(needle);
  });
  const toggle = (id) => {
    const current = normalized[activeFamily];
    const next = activeSelected.has(id) ? current.filter((item) => item !== id) : [...current, id];
    onChange({ ...normalized, [activeFamily]: next });
  };

  return (
    <PopoverPrimitive.Root open={open} onOpenChange={(next) => {
      if (disabled) return;
      setOpen(next);
      if (!next) setQuery('');
    }}>
      <PopoverPrimitive.Trigger asChild>
        <button
          id={triggerID}
          type="button"
          className="pool-fallback-selector__trigger"
          aria-label="选择 GPT、Claude、Gemini 流量的兜底用户分组"
          aria-expanded={open}
          disabled={disabled}
        >
          <span className="pool-fallback-selector__summary">
            {selectedCount ? FALLBACK_FAMILIES.map(({ key, label }) => (
              <span className="pool-fallback-selector__pill" key={key}>{label} <b>{normalized[key].length}</b></span>
            )) : <span className="pool-fallback-selector__placeholder">选择流量家族与兜底用户分组</span>}
          </span>
          <span className="pool-fallback-selector__chevron" aria-hidden="true">⌄</span>
        </button>
      </PopoverPrimitive.Trigger>
      <PopoverPrimitive.Portal>
        <PopoverPrimitive.Content
          className="pool-fallback-selector__content"
          align="start"
          sideOffset={6}
          collisionPadding={16}
        >
          <div className="pool-fallback-selector__families" aria-label="模型家族">
            {FALLBACK_FAMILIES.map(({ key, label }) => (
              <button
                type="button"
                key={key}
                className={activeFamily === key ? 'is-active' : ''}
                aria-pressed={activeFamily === key}
                onClick={() => { setActiveFamily(key); setQuery(''); }}
              >
                <span>{label}</span>
                <b>{normalized[key].length}</b>
              </button>
            ))}
          </div>
          <div className="pool-fallback-selector__groups">
            <div className="pool-fallback-selector__groups-head">
              <strong>{FALLBACK_FAMILIES.find(({ key }) => key === activeFamily)?.label} 兜底分组</strong>
              <span>{activeSelected.size} 已选</span>
            </div>
            <input
              className="pool-input"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="搜索用户分组"
              aria-label="搜索兜底用户分组"
            />
            <div className="pool-fallback-selector__list" role="group" aria-label="可选兜底用户分组">
              {filteredOptions.length ? filteredOptions.map((option) => {
                const checked = activeSelected.has(option.id);
                return (
                  <button
                    type="button"
                    key={option.id}
                    className={checked ? 'is-selected' : ''}
                    role="checkbox"
                    aria-checked={checked}
                    onClick={() => toggle(option.id)}
                  >
                    <span className="pool-fallback-selector__option-copy">
                      <strong title={option.label}>{option.label}</strong>
                      <small title={option.id}>{option.id}</small>
                    </span>
                    <span className="pool-fallback-selector__check" aria-hidden="true">{checked ? '✓' : ''}</span>
                  </button>
                );
              }) : <div className="pool-fallback-selector__empty">{options.length ? '没有匹配的用户分组' : '请先创建另一个用户分组'}</div>}
            </div>
          </div>
        </PopoverPrimitive.Content>
      </PopoverPrimitive.Portal>
    </PopoverPrimitive.Root>
  );
}

function ModelCatalogInput({ label, value, onChange, models, placeholder, help }) {
  const listID = `pool-model-catalog-${useId().replace(/:/g, '')}`;
  return (
    <>
      <Form.Input
        label={label}
        value={value}
        onChange={onChange}
        list={listID}
        placeholder={placeholder}
        help={help}
        autoComplete="off"
      />
      <datalist id={listID}>
        {uniqueStrings(models).map((model) => <option key={model} value={model} />)}
      </datalist>
    </>
  );
}

function TrafficFallbackModelEditor({ draft, setDraft, userGroups, models }) {
  const catalog = useMemo(() => modelsByFamily(models), [models]);
  const mappings = draft.traffic_fallback_model_mappings || [];
  const updateMapping = (index, patchValue) => setDraft((current) => ({
    ...current,
    traffic_fallback_model_mappings: current.traffic_fallback_model_mappings.map((mapping, mappingIndex) => (
      mappingIndex === index ? { ...mapping, ...patchValue } : mapping
    )),
  }));
  const addMapping = () => setDraft((current) => {
    let family = 'gpt';
    let targetUserGroupID = '';
    for (const candidate of FALLBACK_FAMILIES) {
      const selected = uniqueStrings(current.traffic_fallback_groups?.[candidate.key]);
      const missing = selected.find((groupID) => !(current.traffic_fallback_model_mappings || []).some((mapping) => (
        mapping.family === candidate.key && mapping.target_user_group_id === groupID
      )));
      if (missing) {
        family = candidate.key;
        targetUserGroupID = missing;
        break;
      }
      if (!targetUserGroupID && selected[0]) {
        family = candidate.key;
        targetUserGroupID = selected[0];
      }
    }
    return {
      ...current,
      traffic_fallback_model_mappings: [...(current.traffic_fallback_model_mappings || []), {
        family,
        source_model: '',
        target_user_group_id: targetUserGroupID,
        target_model: '',
      }],
    };
  });
  const removeMapping = (index) => setDraft((current) => ({
    ...current,
    traffic_fallback_model_mappings: current.traffic_fallback_model_mappings.filter((_, mappingIndex) => mappingIndex !== index),
  }));
  const selectedCount = FALLBACK_FAMILIES.reduce((sum, { key }) => sum + uniqueStrings(draft.traffic_fallback_groups?.[key]).length, 0);

  return (
    <section className="pool-traffic-fallback-editor">
      <div className="pool-user-routing-editor__head">
        <div>
          <strong>模型流量转换</strong>
          <div className="pool-field__help">精确模型优先于前缀通配符；可从实时模型目录选择，也可直接输入尚未探测到的模型名。</div>
        </div>
        <Button onClick={addMapping} disabled={!selectedCount}>添加模型转换</Button>
      </div>
      {mappings.length ? mappings.map((mapping, index) => {
        const selectedGroups = uniqueStrings(draft.traffic_fallback_groups?.[mapping.family]);
        const groupOptions = selectedGroups.map((id) => {
          const group = userGroups.find((item) => item.id === id);
          return { label: group?.name || id, value: id };
        });
        const sourceModels = [...(catalog[mapping.family] || []), ...catalog.other];
        return (
          <Card key={`${index}:${mapping.family}:${mapping.target_user_group_id}`} className="pool-fallback-mapping-card">
            <div className="pool-fallback-mapping-card__index">
              <span>{String(index + 1).padStart(2, '0')}</span>
              <small>转换规则</small>
            </div>
            <div className="pool-fallback-mapping-card__fields">
              <Form.Select
                label="流量家族"
                value={mapping.family}
                onChange={(family) => updateMapping(index, {
                  family,
                  target_user_group_id: uniqueStrings(draft.traffic_fallback_groups?.[family])[0] || '',
                })}
                optionList={FALLBACK_FAMILIES.map(({ key, label }) => ({ label, value: key }))}
              />
              <ModelCatalogInput
                label="来源模型"
                value={mapping.source_model}
                onChange={(source_model) => updateMapping(index, { source_model })}
                models={sourceModels}
                placeholder="gpt-5.6-sol 或 gpt-5.*"
              />
              <div className="pool-fallback-mapping-card__arrow" aria-hidden="true">→</div>
              <Form.Select
                label="请求转入用户分组"
                value={mapping.target_user_group_id}
                onChange={(target_user_group_id) => updateMapping(index, { target_user_group_id })}
                optionList={groupOptions}
                placeholder="选择已勾选的兜底分组"
              />
              <ModelCatalogInput
                label="目标模型"
                value={mapping.target_model}
                onChange={(target_model) => updateMapping(index, { target_model })}
                models={[...catalog.gpt, ...catalog.claude, ...catalog.gemini, ...catalog.other]}
                placeholder="gpt-5.5（支持手动输入）"
              />
            </div>
            <Button size="small" type="danger" onClick={() => removeMapping(index)}>删除</Button>
          </Card>
        );
      }) : (
        <Banner
          type={selectedCount ? 'warning' : 'info'}
          title={selectedCount ? '为已选兜底分组添加模型转换' : '尚未启用流量兜底'}
          description={selectedCount
            ? '每个已勾选的模型家族与兜底分组至少需要一条来源模型 → 目标模型规则；使用 * 可覆盖该家族的所有模型。'
            : '先在上方二级下拉框中选择 GPT、Claude 或 Gemini 的兜底用户分组。'}
        />
      )}
    </section>
  );
}

function UserGroupEditor({
  editor,
  groups,
  userGroups,
  providers,
  instructionFiles,
  models,
  modelsError,
  catalogLoading,
  catalogError,
  onRetryCatalog,
  saving,
  onCancel,
  onSave,
}) {
  const [draft, setDraft] = useState(() => userGroupDraft(editor?.row));
  const targetOptions = useMemo(() => [
    ...groups.map((group) => ({ label: `账号池分组 · ${group.name}`, value: `${TARGET_ACCOUNT_GROUP}:${group.name}` })),
    ...providers.map((provider) => ({ label: `模型提供商 · ${provider.name || provider.id}`, value: `${TARGET_PROVIDER}:${provider.id}` })),
  ], [groups, providers]);
  const selectedTargetOptions = targetOptions.filter((option) => draft.target_keys.includes(option.value));
  const blockedAccountGroupOptions = groups
    .filter((group) => draft.target_keys.includes(`${TARGET_ACCOUNT_GROUP}:${group.name}`))
    .map((group) => ({ label: `账号池分组 · ${group.name}`, value: group.name }));
  const fallbackGroupOptions = userGroups
    .filter((group) => group.id !== editor?.row?.id)
    .map((group) => ({ id: group.id, label: group.name || group.id }));
  const payload = normalizedUserGroupPayload(draft, providers);
  const fallbackIssues = fallbackConfigurationIssues(draft);

  const setTargets = (targetKeys) => setDraft((current) => {
    const selected = new Set(targetKeys);
    const selectedAccountGroups = new Set(targetKeys
      .map(parseTargetKey)
      .filter((target) => target.kind === TARGET_ACCOUNT_GROUP)
      .map((target) => target.id));
    return {
      ...current,
      target_keys: targetKeys,
      block_claude_target_groups: uniqueStrings(current.block_claude_target_groups)
        .filter((groupName) => selectedAccountGroups.has(groupName)),
      block_gpt_target_groups: uniqueStrings(current.block_gpt_target_groups)
        .filter((groupName) => selectedAccountGroups.has(groupName)),
      model_routing: current.model_routing.map((rule) => ({
        ...rule,
        tiers: rule.tiers.map((tier) => tier.filter((key) => selected.has(key))),
      })),
    };
  });
  const updateInstructionProfile = (family, patchValue) => setDraft((current) => ({
    ...current,
    model_instruction_profiles: {
      ...current.model_instruction_profiles,
      [family]: { ...current.model_instruction_profiles?.[family], ...patchValue },
    },
  }));
  const setTrafficFallbackGroups = (traffic_fallback_groups) => setDraft((current) => ({
    ...current,
    traffic_fallback_groups,
    traffic_fallback_model_mappings: (current.traffic_fallback_model_mappings || []).filter((mapping) => (
      uniqueStrings(traffic_fallback_groups?.[mapping.family]).includes(mapping.target_user_group_id)
    )),
  }));

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
            disabled={catalogLoading && targetOptions.length === 0}
            style={{ width: '100%' }}
          />
          <div className="pool-field__help">可只选账号池分组、只选模型提供商，或任意混合；不能直接绑定账号或出口。</div>
        </div>
      </div>
      {catalogError ? (
        <Banner
          type="warning"
          title="部分目标目录加载失败"
          description="已保留成功加载的账号池分组或模型提供商；重试不会清空当前编辑内容。"
          actions={[<Button key="retry" size="small" loading={catalogLoading} onClick={onRetryCatalog}>重试目标目录</Button>]}
        />
      ) : null}
      <Card title="流量接收策略" className="pool-card">
        <Banner
          type="info"
          title="策略只属于当前用户分组"
          description="默认不屏蔽任何目标。勾选后会跳过当前用户分组里的指定账号池，并把请求转移给其他可承载目标；全部暂不可用时保持连接并动态等待最多 10 分钟。底层账号池本身不会被全局禁用。"
        />
        <div className="pool-user-group-grid">
          <div className="pool-field pool-field--top">
            <span className="pool-field__label">Claude 流量跳过的账号池分组</span>
            <Select
              multiple
              filter
              value={draft.block_claude_target_groups}
              onChange={(block_claude_target_groups) => setDraft((current) => ({ ...current, block_claude_target_groups }))}
              optionList={blockedAccountGroupOptions}
              placeholder="默认无；勾选当前用户分组内的账号池"
              style={{ width: '100%' }}
            />
          </div>
          <div className="pool-field pool-field--top">
            <span className="pool-field__label">GPT / Codex 流量跳过的账号池分组</span>
            <Select
              multiple
              filter
              value={draft.block_gpt_target_groups}
              onChange={(block_gpt_target_groups) => setDraft((current) => ({ ...current, block_gpt_target_groups }))}
              optionList={blockedAccountGroupOptions}
              placeholder="默认无；勾选当前用户分组内的账号池"
              style={{ width: '100%' }}
            />
          </div>
        </div>
      </Card>
      <Card title="流量兜底" className="pool-card pool-traffic-fallback-card">
        <Banner
          type="info"
          title="仅在当前分组全部候选失败后接管"
          description="按 GPT / Claude / Gemini 分别选择兜底用户分组，再配置来源模型到目标模型的转换。只重放尚未提交且不携带服务端会话状态的请求；循环引用会在保存时被拦截。"
        />
        <div className="pool-field pool-field--top">
          <span className="pool-field__label">流量兜底分组</span>
          <TrafficFallbackGroupSelector
            value={draft.traffic_fallback_groups}
            onChange={setTrafficFallbackGroups}
            options={fallbackGroupOptions}
            disabled={catalogLoading && fallbackGroupOptions.length === 0}
          />
          <div className="pool-field__help">一级选择模型家族，二级列表勾选有序兜底分组；同一家族按勾选顺序尝试。</div>
        </div>
        {modelsError ? (
          <Banner type="warning" title="模型目录暂不可用" description="手动输入仍可正常配置与保存，目录恢复后会自动补全候选项。" />
        ) : null}
        <TrafficFallbackModelEditor draft={draft} setDraft={setDraft} userGroups={userGroups} models={models} />
        {fallbackIssues.length ? (
          <div className="pool-fallback-validation" role="alert">
            <strong>还需完成 {fallbackIssues.length} 项配置</strong>
            <span>{fallbackIssues.slice(0, 3).join('；')}{fallbackIssues.length > 3 ? `；另有 ${fallbackIssues.length - 3} 项` : ''}</span>
          </div>
        ) : null}
      </Card>
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
        <div className="pool-instruction-profiles">
          {INSTRUCTION_FAMILIES.map((family) => {
            const profile = draft.model_instruction_profiles?.[family.key] || { enabled: false, files: [] };
            return (
              <div className="pool-instruction-profile" key={family.key}>
                <div className="pool-instruction-profile__label">
                  <label className="pool-inline-switch">
                    <Switch checked={profile.enabled} onChange={(enabled) => updateInstructionProfile(family.key, { enabled })} />
                    <strong>{family.label}</strong>
                  </label>
                  <span className="pool-field__help">{family.help}</span>
                </div>
                <Select
                  multiple
                  filter
                  value={profile.files}
                  onChange={(files) => updateInstructionProfile(family.key, { files })}
                  optionList={instructionFiles.map((file) => ({ label: file.error ? `${file.name}（异常）` : file.name, value: file.name, disabled: Boolean(file.error) }))}
                  placeholder={`选择 ${family.label} 指令文件`}
                  disabled={!profile.enabled}
                  style={{ width: '100%' }}
                />
              </div>
            );
          })}
        </div>
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
            {FALLBACK_FAMILIES.flatMap(({ key, label }) => (payload.traffic_fallback_groups[key] || []).map((groupID) => {
              const group = userGroups.find((item) => item.id === groupID);
              const mappings = payload.traffic_fallback_model_mappings.filter((mapping) => mapping.family === key && mapping.target_user_group_id === groupID);
              return (
                <div key={`fallback:${key}:${groupID}`}>
                  <strong>{label} 兜底</strong>：{group?.name || groupID} · {mappings.map((mapping) => `${mapping.source_model} → ${mapping.target_model}`).join('，')}
                </div>
              );
            }))}
          </>
        ) : <span className="pool-muted">至少选择一个目标后显示路由摘要。</span>}
      </Card>
      <div className="pool-modal-actions">
        <Button onClick={onCancel} disabled={saving}>取消</Button>
        <Button theme="solid" loading={saving} disabled={!payload.name || payload.targets.length === 0 || fallbackIssues.length > 0} onClick={() => onSave(payload)}>保存用户分组</Button>
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

  // Keep independent directories isolated: a failed optional endpoint must not
  // erase valid account groups or providers from the user-group target picker.
  const fetchGroups = useCallback(async ({ signal }) => rowsOf(await get('/admin/groups', undefined, { signal }), ['groups']), []);
  const fetchInstructions = useCallback(async ({ signal }) => rowsOf(await get('/admin/model-instructions', undefined, { signal }), ['files']), []);
  const fetchEgresses = useCallback(async ({ signal }) => rowsOf(await get('/admin/egress-profiles', undefined, { signal }), ['profiles', 'egress_profiles']), []);
  const fetchUserGroups = useCallback(async ({ signal }) => rowsOf(await get('/admin/user-groups', undefined, { signal }), ['user_groups']), []);
  const fetchProviders = useCallback(async ({ signal }) => rowsOf(await get('/admin/providers', undefined, { signal }), ['providers']), []);
  const fetchModels = useCallback(async ({ signal }) => rowsOf(await get('/admin/models', undefined, { signal }), ['models']), []);

  const groupsResource = useAsyncResource(fetchGroups, [fetchGroups], { initialData: [] });
  const instructionsResource = useAsyncResource(fetchInstructions, [fetchInstructions], { initialData: [] });
  const egressesResource = useAsyncResource(fetchEgresses, [fetchEgresses], { initialData: [] });
  const userGroupsResource = useAsyncResource(fetchUserGroups, [fetchUserGroups], { initialData: [] });
  const providersResource = useAsyncResource(fetchProviders, [fetchProviders], { initialData: [] });
  const modelsResource = useAsyncResource(fetchModels, [fetchModels], { initialData: [] });
  const resources = [groupsResource, instructionsResource, egressesResource, userGroupsResource, providersResource, modelsResource];
  const data = {
    groups: groupsResource.data || [],
    instructions: instructionsResource.data || [],
    egresses: egressesResource.data || [],
    userGroups: userGroupsResource.data || [],
    providers: providersResource.data || [],
    models: modelsResource.data || [],
  };
  const loading = resources.some((resource) => resource.loading);
  const lastRefresh = resources.reduce((latest, resource) => {
    if (!resource.lastRefresh) return latest;
    return !latest || resource.lastRefresh > latest ? resource.lastRefresh : latest;
  }, null);
  const load = useCallback(async () => {
    await Promise.allSettled([
      groupsResource.reload(),
      instructionsResource.reload(),
      egressesResource.reload(),
      userGroupsResource.reload(),
      providersResource.reload(),
      modelsResource.reload(),
    ]);
  }, [
    groupsResource.reload,
    instructionsResource.reload,
    egressesResource.reload,
    userGroupsResource.reload,
    providersResource.reload,
    modelsResource.reload,
  ]);
  const refreshUserGroupCatalog = useCallback(() => {
    void Promise.allSettled([
      groupsResource.reload(),
      providersResource.reload(),
      instructionsResource.reload(),
      modelsResource.reload(),
    ]);
  }, [groupsResource.reload, providersResource.reload, instructionsResource.reload, modelsResource.reload]);
  const openUserGroupEditor = useCallback((editor) => {
    setUserEditor(editor);
    refreshUserGroupCatalog();
  }, [refreshUserGroupCatalog]);
  const openAccountGroupEditor = useCallback((editor) => {
    setAccountEditor(editor);
    void egressesResource.reload();
  }, [egressesResource.reload]);
  const targetCatalogLoading = groupsResource.loading || providersResource.loading || instructionsResource.loading || modelsResource.loading;
  const targetCatalogError = groupsResource.error || providersResource.error || instructionsResource.error;

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
          { label: '编辑出口', disabled: savingAccountGroup || removingAccountGroup, onSelect: () => openAccountGroupEditor({ mode: 'edit', row }) },
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
        ...INSTRUCTION_FAMILIES.filter(({ key }) => row.model_instruction_profiles?.[key]?.enabled).map(({ label }) => `${label} 指令`),
        !row.model_instruction_profiles && row.model_instructions_enabled ? `兼容指令 ${row.model_instructions_files?.length || 0}` : '',
        row.system_prompt_apply_to_compaction ? '压缩时应用' : '',
        row.block_claude_target_groups?.length ? `Claude 跳过 ${row.block_claude_target_groups.length} 组` : '',
        row.block_gpt_target_groups?.length ? `GPT 跳过 ${row.block_gpt_target_groups.length} 组` : '',
        FALLBACK_FAMILIES.reduce((count, { key }) => count + (row.traffic_fallback_groups?.[key]?.length || 0), 0)
          ? `流量兜底 ${FALLBACK_FAMILIES.reduce((count, { key }) => count + (row.traffic_fallback_groups?.[key]?.length || 0), 0)} 组`
          : '',
      ].filter(Boolean)} max={3} />,
    },
    {
      title: '操作', key: 'ops', width: 100,
      render: (_, row) => (
        <ActionMenu label="用户分组操作" items={[
          { label: '编辑完整策略', disabled: savingUserGroup || removingUserGroup, onSelect: () => openUserGroupEditor({ mode: 'edit', row }) },
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
    { label: '流量兜底', value: data.userGroups.filter((row) => FALLBACK_FAMILIES.some(({ key }) => row.traffic_fallback_groups?.[key]?.length)).length, tone: 'success' },
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
              onClick={() => activeTab === 'user' ? openUserGroupEditor({ mode: 'create', row: null }) : openAccountGroupEditor({ mode: 'create', row: null })}
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
            <ResourceTable error={groupsResource.error} onRetry={groupsResource.reload} loading={groupsResource.loading} lastRefresh={groupsResource.lastRefresh} dataSource={data.groups} columns={accountColumns} rowKey="name" pagination={false} density="compact" layout="fit" scroll={false} rowHeight={68} emptyTitle="暂无账号池分组" skeletonRows={5} />
            {!groupsResource.error || groupsResource.lastRefresh ? <MetricRail items={accountMetrics} /> : null}
          </div>
        </TabPane>
        <TabPane key="user" tab="用户分组" itemKey="user">
          <div className="pool-resource-split pool-group-resource-split">
            <ResourceTable error={userGroupsResource.error} onRetry={userGroupsResource.reload} loading={userGroupsResource.loading} lastRefresh={userGroupsResource.lastRefresh} dataSource={data.userGroups} columns={userColumns} rowKey="id" pagination={false} density="compact" layout="fit" scroll={false} rowHeight={68} emptyTitle="暂无用户分组" emptyDescription="创建后可混合选择账号池分组与模型提供商，并按模型设置优先层级。" skeletonRows={5} />
            {!userGroupsResource.error || userGroupsResource.lastRefresh ? <MetricRail items={userMetrics} /> : null}
          </div>
        </TabPane>
      </Tabs>

      <Modal title={accountEditor?.mode === 'edit' ? `编辑账号池分组 · ${accountEditor.row.name}` : '新建账号池分组'} visible={Boolean(accountEditor)} onCancel={() => { if (!savingAccountGroup) setAccountEditor(null); }} footer={null} width={700} maskClosable={!savingAccountGroup}>
        {accountEditor ? <AccountGroupEditor key={`${accountEditor.mode}:${accountEditor.row?.name || 'new'}`} editor={accountEditor} profiles={data.egresses} saving={savingAccountGroup} onCancel={() => setAccountEditor(null)} onSave={saveAccountGroup} /> : null}
      </Modal>
      <Modal title={userEditor?.mode === 'edit' ? `编辑用户分组 · ${userEditor.row.name}` : '新建用户分组'} visible={Boolean(userEditor)} onCancel={() => { if (!savingUserGroup) setUserEditor(null); }} footer={null} width={960} maskClosable={!savingUserGroup}>
        {userEditor ? (
          <UserGroupEditor
            key={`${userEditor.mode}:${userEditor.row?.id || 'new'}`}
            editor={userEditor}
            groups={data.groups}
            userGroups={data.userGroups}
            providers={data.providers}
            instructionFiles={data.instructions}
            models={data.models}
            modelsError={modelsResource.error}
            catalogLoading={targetCatalogLoading}
            catalogError={targetCatalogError}
            onRetryCatalog={refreshUserGroupCatalog}
            saving={savingUserGroup}
            onCancel={() => setUserEditor(null)}
            onSave={saveUserGroup}
          />
        ) : null}
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

export {
  blankUserGroup,
  fallbackConfigurationIssues,
  modelFamily,
  modelsByFamily,
  normalizedUserGroupPayload,
  userGroupDraft,
};
