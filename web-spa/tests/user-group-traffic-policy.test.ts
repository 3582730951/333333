import { describe, expect, it } from 'vitest';
import {
  blankUserGroup,
  fallbackConfigurationIssues,
  modelFamily,
  modelsByFamily,
  normalizedUserGroupPayload,
  userGroupDraft,
} from '../src/pages/Groups.jsx';

describe('user-group target-family traffic policy', () => {
  it('defaults to no blocked account-pool targets', () => {
    const draft = blankUserGroup();
    expect(draft.block_claude_target_groups).toEqual([]);
    expect(draft.block_gpt_target_groups).toEqual([]);
  });

  it('persists only selected account-pool groups and never provider targets', () => {
    const payload = normalizedUserGroupPayload({
      ...blankUserGroup(),
      name: 'routing-policy',
      target_keys: [
        'account_pool_group:pool-a',
        'account_pool_group:pool-b',
        'model_provider:claude',
      ],
      block_claude_target_groups: ['pool-a', 'pool-a', 'not-selected'],
      block_gpt_target_groups: ['pool-b', 'claude'],
    });

    expect(payload.block_claude_target_groups).toEqual(['pool-a']);
    expect(payload.block_gpt_target_groups).toEqual(['pool-b']);
    expect(payload.targets).toEqual([
      { kind: 'account_pool_group', id: 'pool-a' },
      { kind: 'account_pool_group', id: 'pool-b' },
      { kind: 'model_provider', id: 'claude' },
    ]);
  });

  it('round-trips an existing per-target policy into the editor', () => {
    const draft = userGroupDraft({
      id: 'ug-policy',
      name: 'policy',
      targets: [
        { kind: 'account_pool_group', id: 'pool-a' },
        { kind: 'account_pool_group', id: 'pool-b' },
      ],
      block_claude_target_groups: ['pool-a'],
      block_gpt_target_groups: ['pool-b'],
    });

    expect(draft.target_keys).toEqual([
      'account_pool_group:pool-a',
      'account_pool_group:pool-b',
    ]);
    expect(draft.block_claude_target_groups).toEqual(['pool-a']);
    expect(draft.block_gpt_target_groups).toEqual(['pool-b']);
  });
});

describe('user-group cross-group traffic fallback policy', () => {
  it('defaults every supported family to an empty ordered fallback list', () => {
    const draft = blankUserGroup();
    expect(draft.traffic_fallback_groups).toEqual({ gpt: [], claude: [], gemini: [] });
    expect(draft.traffic_fallback_model_mappings).toEqual([]);
  });

  it('round-trips ordered groups and multiple model rewrites', () => {
    const draft = userGroupDraft({
      id: 'ug-primary',
      name: 'primary',
      targets: [{ kind: 'account_pool_group', id: 'pool-a' }],
      traffic_fallback_groups: {
        gpt: ['ug-gpt-b', 'ug-gpt-c'],
        claude: ['ug-claude'],
        gemini: [],
      },
      traffic_fallback_model_mappings: [
        { family: 'gpt', source_model: 'gpt-5.6-sol', target_user_group_id: 'ug-gpt-b', target_model: 'gpt-5.5' },
        { family: 'gpt', source_model: 'gpt-5.*', target_user_group_id: 'ug-gpt-c', target_model: 'gpt-5.4' },
        { family: 'claude', source_model: '*', target_user_group_id: 'ug-claude', target_model: 'claude-sonnet-4-5' },
      ],
    });

    expect(draft.traffic_fallback_groups.gpt).toEqual(['ug-gpt-b', 'ug-gpt-c']);
    expect(draft.traffic_fallback_model_mappings).toHaveLength(3);
    expect(fallbackConfigurationIssues(draft)).toEqual([]);
  });

  it('normalizes duplicates while retaining manual model names', () => {
    const payload = normalizedUserGroupPayload({
      ...blankUserGroup(),
      name: 'fallback-policy',
      target_keys: ['account_pool_group:pool-a'],
      traffic_fallback_groups: {
        gpt: ['ug-backup', 'ug-backup'],
        claude: [],
        gemini: [],
      },
      traffic_fallback_model_mappings: [
        {
          family: 'gpt',
          source_model: 'manual-gpt-edge',
          target_user_group_id: 'ug-backup',
          target_model: 'manually-entered-target',
        },
        {
          family: 'claude',
          source_model: 'claude-*',
          target_user_group_id: 'not-selected',
          target_model: 'claude-sonnet',
        },
      ],
    });

    expect(payload.traffic_fallback_groups.gpt).toEqual(['ug-backup']);
    expect(payload.traffic_fallback_model_mappings).toEqual([{
      family: 'gpt',
      source_model: 'manual-gpt-edge',
      target_user_group_id: 'ug-backup',
      target_model: 'manually-entered-target',
    }]);
  });

  it('reports an incomplete selected fallback before save', () => {
    const draft = {
      ...blankUserGroup(),
      traffic_fallback_groups: { gpt: ['ug-backup'], claude: [], gemini: [] },
    };
    expect(fallbackConfigurationIssues(draft)).toEqual(['GPT / ChatGPT / Codex · ug-backup 尚未配置模型转换']);
  });

  it('reports duplicate and malformed wildcard mappings before save', () => {
    const mapping = {
      family: 'gpt',
      source_model: 'gpt-*broken',
      target_user_group_id: 'ug-backup',
      target_model: 'gpt-5.5',
    };
    const draft = {
      ...blankUserGroup(),
      traffic_fallback_groups: { gpt: ['ug-backup'], claude: [], gemini: [] },
      traffic_fallback_model_mappings: [mapping, { ...mapping }],
    };
    expect(fallbackConfigurationIssues(draft)).toEqual([
      '模型转换 1 仅支持末尾通配符',
      '模型转换 2 仅支持末尾通配符',
      '模型转换 2 与已有规则重复',
    ]);
  });

  it('groups live catalog suggestions without blocking unknown manual models', () => {
    expect(modelFamily('gpt-5.6-sol')).toBe('gpt');
    expect(modelFamily('claude-sonnet-4-5')).toBe('claude');
    expect(modelFamily('gemini-3-pro')).toBe('gemini');
    expect(modelFamily('vendor-private-model')).toBe('');
    expect(modelsByFamily(['gpt-5.6-sol', 'claude-sonnet-4-5', 'gemini-3-pro', 'vendor-private-model'])).toEqual({
      gpt: ['gpt-5.6-sol'],
      claude: ['claude-sonnet-4-5'],
      gemini: ['gemini-3-pro'],
      other: ['vendor-private-model'],
    });
  });
});
