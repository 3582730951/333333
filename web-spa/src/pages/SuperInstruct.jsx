import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Banner, Button, Card, Select, Switch, Tag, Toast } from '../components/pool/index.jsx';
import { IconRefresh, IconSave } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import { get, put } from '../api.js';
import { showErrorToast } from '../components/ErrorToast.jsx';

const FAMILIES = [
  { key: 'gpt', label: 'GPT / ChatGPT / Codex', help: 'ChatGPT、Codex、OpenAI Responses / Chat Completions 请求使用这一组配置。' },
  { key: 'claude', label: 'Claude', help: 'Claude、Claude Code，以及 Kiro / Antigravity 中的 Claude 模型使用这一组配置。' },
  { key: 'gemini', label: 'Gemini', help: 'Gemini 模型和 Antigravity Gemini 目标使用这一组配置。' },
];

function rowsOf(value, keys = []) {
  if (Array.isArray(value)) return value;
  if (value && typeof value === 'object') {
    for (const key of keys) if (Array.isArray(value[key])) return value[key];
    for (const key of ['items', 'rows', 'data']) if (Array.isArray(value[key])) return value[key];
  }
  return [];
}

function uniqueStrings(values) {
  const seen = new Set();
  return (Array.isArray(values) ? values : [])
    .map((value) => String(value || '').trim())
    .filter((value) => {
      if (!value || seen.has(value)) return false;
      seen.add(value);
      return true;
    });
}

function blankProfiles() {
  return Object.fromEntries(FAMILIES.map(({ key }) => [key, {
    enabled: false,
    skill_ids: [],
    response_rewrite_enabled: false,
    memory_enabled: false,
    monitor_enabled: false,
  }]));
}

function normalizeProfiles(group) {
  const configured = group?.super_instruct_profiles && typeof group.super_instruct_profiles === 'object'
    ? group.super_instruct_profiles
    : null;
  const hasProfiles = configured && Object.keys(configured).length > 0;
  return Object.fromEntries(FAMILIES.map(({ key }) => {
    const profile = hasProfiles ? configured[key] : null;
    return [key, {
      enabled: hasProfiles ? Boolean(profile?.enabled) : Boolean(group?.super_instruct_enabled),
      skill_ids: hasProfiles ? uniqueStrings(profile?.skill_ids) : uniqueStrings(group?.super_instruct_skill_ids),
      response_rewrite_enabled: hasProfiles ? Boolean(profile?.response_rewrite_enabled) : Boolean(group?.super_instruct_response_rewrite_enabled),
      memory_enabled: hasProfiles ? Boolean(profile?.memory_enabled) : Boolean(group?.super_instruct_memory_enabled),
      monitor_enabled: hasProfiles ? Boolean(profile?.monitor_enabled) : Boolean(group?.super_instruct_monitor_enabled),
    }];
  }));
}

function profileEnabled(profile) {
  return Boolean(profile?.enabled || profile?.response_rewrite_enabled || profile?.memory_enabled || profile?.monitor_enabled);
}

function groupEnabled(group) {
  return Object.values(normalizeProfiles(group)).some(profileEnabled);
}

function enabledSummary(group) {
  const profiles = normalizeProfiles(group);
  const parts = [];
  for (const family of FAMILIES) {
    const profile = profiles[family.key];
    if (!profileEnabled(profile)) continue;
    const enabled = [];
    if (profile.enabled) enabled.push('指令');
    if (profile.response_rewrite_enabled) enabled.push('改写');
    if (profile.memory_enabled) enabled.push('Memory');
    if (profile.monitor_enabled) enabled.push('Monitor');
    parts.push(`${family.label}: ${enabled.join('/')}`);
  }
  return parts;
}

function normalizeTargets(values) {
  return (Array.isArray(values) ? values : [])
    .map((target) => ({ kind: String(target?.kind || target?.target_type || '').trim(), id: String(target?.id || target?.target_ref || '').trim() }))
    .filter((target) => target.kind && target.id);
}

function savePayload(group, profiles) {
  return {
    name: String(group?.name || '').trim(),
    system_prompt: String(group?.system_prompt || ''),
    prompt_mode: group?.prompt_mode || 'prepend',
    system_prompt_apply_to_compaction: Boolean(group?.system_prompt_apply_to_compaction),
    model_instructions_enabled: Boolean(group?.model_instructions_enabled),
    model_instructions_files: uniqueStrings(group?.model_instructions_files),
    model_instruction_profiles: group?.model_instruction_profiles || {},
    super_instruct_enabled: false,
    super_instruct_skill_ids: [],
    super_instruct_profiles: Object.fromEntries(FAMILIES.map(({ key }) => {
      const profile = profiles?.[key] || {};
      return [key, {
        enabled: Boolean(profile.enabled),
        skill_ids: uniqueStrings(profile.skill_ids),
        response_rewrite_enabled: Boolean(profile.response_rewrite_enabled),
        memory_enabled: Boolean(profile.memory_enabled),
        monitor_enabled: Boolean(profile.monitor_enabled),
      }];
    })),
    super_instruct_response_rewrite_enabled: false,
    super_instruct_memory_enabled: false,
    super_instruct_monitor_enabled: false,
    force_model: String(group?.force_model || '').trim(),
    force_effort: String(group?.force_effort || '').trim(),
    block_claude_target_groups: uniqueStrings(group?.block_claude_target_groups),
    block_gpt_target_groups: uniqueStrings(group?.block_gpt_target_groups),
    traffic_fallback_groups: group?.traffic_fallback_groups || {},
    traffic_fallback_model_mappings: Array.isArray(group?.traffic_fallback_model_mappings) ? group.traffic_fallback_model_mappings : [],
    targets: normalizeTargets(group?.targets),
    model_routing: Array.isArray(group?.model_routing) ? group.model_routing : [],
  };
}

function metricValue(value) {
  const number = Number(value);
  return Number.isFinite(number) ? number : 0;
}

export default function SuperInstruct() {
  const [groups, setGroups] = useState([]);
  const [skills, setSkills] = useState([]);
  const [directory, setDirectory] = useState('');
  const [memory, setMemory] = useState(null);
  const [monitor, setMonitor] = useState(null);
  const [selectedID, setSelectedID] = useState('');
  const [profiles, setProfiles] = useState(blankProfiles());
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const selectedIDRef = useRef('');

  const selected = useMemo(() => groups.find((group) => group.id === selectedID) || null, [groups, selectedID]);
  const enabledGroups = useMemo(() => groups.filter(groupEnabled).length, [groups]);
  const usableSkills = useMemo(() => skills.filter((skill) => !skill.error).length, [skills]);

  useEffect(() => {
    selectedIDRef.current = selectedID;
  }, [selectedID]);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [groupData, skillData, memoryData, monitorData] = await Promise.all([
        get('/admin/user-groups'),
        get('/admin/super-instruct/skills'),
        get('/admin/super-instruct/memory'),
        get('/admin/super-instruct/monitor'),
      ]);
      const nextGroups = rowsOf(groupData, ['user_groups']);
      setGroups(nextGroups);
      setSkills(rowsOf(skillData, ['skills']));
      setDirectory(skillData?.directory || '');
      setMemory(memoryData || null);
      setMonitor(monitorData || null);
      const nextSelected = nextGroups.find((group) => group.id === selectedIDRef.current) || nextGroups[0] || null;
      setSelectedID(nextSelected?.id || '');
      setProfiles(normalizeProfiles(nextSelected));
    } catch (error) {
      showErrorToast(error);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { load(); }, [load]);

  const selectGroup = (group) => {
    setSelectedID(group.id);
    setProfiles(normalizeProfiles(group));
  };

  const updateProfile = (family, patch) => setProfiles((current) => ({
    ...current,
    [family]: { ...current[family], ...patch },
  }));

  const setAllFamilies = (value) => setProfiles(Object.fromEntries(FAMILIES.map(({ key }) => [key, {
    ...(profiles[key] || {}),
    enabled: value,
    response_rewrite_enabled: value,
    memory_enabled: value,
    monitor_enabled: value,
  }])));

  const save = async () => {
    if (!selected) {
      Toast.error('请选择用户分组');
      return;
    }
    const body = savePayload(selected, profiles);
    if (!body.targets.length) {
      Toast.error('该用户分组没有路由目标，请先在“分组”页补齐目标');
      return;
    }
    setSaving(true);
    try {
      const updated = await put(`/admin/user-groups/${encodeURIComponent(selected.id)}`, body);
      Toast.success('Super-Instruct 配置已保存');
      setGroups((current) => current.map((group) => group.id === selected.id ? updated : group));
      setSelectedID(updated.id);
      setProfiles(normalizeProfiles(updated));
    } catch (error) {
      showErrorToast(error);
    } finally {
      setSaving(false);
    }
  };

  const skillOptions = skills.map((skill) => ({
    label: skill.description ? `${skill.id} · ${skill.description}` : skill.id,
    value: skill.id,
    disabled: Boolean(skill.error),
  }));

  return (
    <div className="super-instruct-page">
      <PageHeader
        title="Super-Instruct"
        subtitle="管理员按用户分组、模型家族启用或关闭 Super-Instruct 热插拔能力。"
        actions={<><Button icon={<IconRefresh />} loading={loading} onClick={load}>刷新</Button><Button icon={<IconSave />} theme="solid" loading={saving} disabled={!selected} onClick={save}>保存当前分组</Button></>}
      />

      <Banner
        type="info"
        title="默认关闭，按用户分组生效"
        description="这里是 Super-Instruct-Codex-5.6 融合能力的独立管理入口。每个用户分组可分别为 GPT/ChatGPT/Codex、Claude、Gemini 启用指令文件系统、响应改写、Memory、Monitor。"
      />

      <div className="super-instruct-metrics">
        <Card title="启用分组" className="pool-card"><div className="super-instruct-metric">{enabledGroups}<small>/ {groups.length}</small></div></Card>
        <Card title="技能目录" className="pool-card"><div className="super-instruct-metric">{usableSkills}<small>/ {skills.length}</small></div></Card>
        <Card title="Memory 记录" className="pool-card"><div className="super-instruct-metric">{metricValue(memory?.stats?.total ?? memory?.successes?.length)}</div></Card>
        <Card title="Monitor 事件" className="pool-card"><div className="super-instruct-metric">{metricValue(monitor?.stats?.total ?? monitor?.history?.length)}</div></Card>
      </div>

      {directory ? <div className="pool-field__help super-instruct-dir">当前指令目录：<code>{directory}</code></div> : null}

      <div className="super-instruct-layout">
        <Card title="用户分组" className="pool-card super-instruct-groups">
          {groups.length ? groups.map((group) => {
            const active = group.id === selectedID;
            const summary = enabledSummary(group);
            return (
              <button key={group.id} type="button" className={`super-instruct-group ${active ? 'is-active' : ''}`} onClick={() => selectGroup(group)}>
                <span className="super-instruct-group__name">{group.name || group.id}</span>
                <span className="super-instruct-group__meta">{group.id}</span>
                <span className="super-instruct-group__tags">
                  <Tag size="small" color={summary.length ? 'green' : 'grey'}>{summary.length ? '已启用' : '关闭'}</Tag>
                  {summary.slice(0, 2).map((item) => <Tag key={item} size="small" color="blue">{item}</Tag>)}
                </span>
              </button>
            );
          }) : <div className="pool-muted">暂无用户分组。请先在“分组”页创建用户分组。</div>}
        </Card>

        <Card title={selected ? `配置 · ${selected.name || selected.id}` : '配置'} className="pool-card super-instruct-editor">
          {selected ? (
            <>
              <div className="super-instruct-toolbar">
                <Button onClick={() => setAllFamilies(true)}>全部模型启用全套</Button>
                <Button onClick={() => setAllFamilies(false)}>全部关闭</Button>
              </div>
              <div className="super-instruct-family-list">
                {FAMILIES.map((family) => {
                  const profile = profiles[family.key] || {};
                  return (
                    <div className="super-instruct-family" key={family.key}>
                      <div className="super-instruct-family__head">
                        <div>
                          <strong>{family.label}</strong>
                          <div className="pool-field__help">{family.help}</div>
                        </div>
                        <div className="super-instruct-family__actions">
                          <Button size="small" onClick={() => updateProfile(family.key, { enabled: true, response_rewrite_enabled: true, memory_enabled: true, monitor_enabled: true })}>启用全套</Button>
                          <Button size="small" onClick={() => updateProfile(family.key, { enabled: false, response_rewrite_enabled: false, memory_enabled: false, monitor_enabled: false })}>关闭</Button>
                        </div>
                      </div>
                      <div className="super-instruct-switches">
                        <label className="pool-inline-switch"><Switch checked={Boolean(profile.enabled)} onChange={(enabled) => updateProfile(family.key, { enabled })} /><span>指令文件系统</span></label>
                        <label className="pool-inline-switch"><Switch checked={Boolean(profile.response_rewrite_enabled)} onChange={(response_rewrite_enabled) => updateProfile(family.key, { response_rewrite_enabled })} /><span>响应改写</span></label>
                        <label className="pool-inline-switch"><Switch checked={Boolean(profile.memory_enabled)} onChange={(memory_enabled) => updateProfile(family.key, { memory_enabled })} /><span>Memory</span></label>
                        <label className="pool-inline-switch"><Switch checked={Boolean(profile.monitor_enabled)} onChange={(monitor_enabled) => updateProfile(family.key, { monitor_enabled })} /><span>Monitor</span></label>
                      </div>
                      <Select
                        multiple
                        filter
                        value={uniqueStrings(profile.skill_ids)}
                        onChange={(skill_ids) => updateProfile(family.key, { skill_ids })}
                        optionList={skillOptions}
                        placeholder={profile.enabled ? '留空 = 使用全部当前可用技能' : '开启“指令文件系统”后可选择技能'}
                        disabled={!profile.enabled || loading || !skills.length}
                        style={{ width: '100%' }}
                      />
                    </div>
                  );
                })}
              </div>
            </>
          ) : <div className="pool-muted">请选择一个用户分组。</div>}
        </Card>
      </div>
    </div>
  );
}
