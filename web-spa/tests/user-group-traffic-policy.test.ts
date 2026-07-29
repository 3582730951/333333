import { describe, expect, it } from 'vitest';
import {
  blankUserGroup,
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
