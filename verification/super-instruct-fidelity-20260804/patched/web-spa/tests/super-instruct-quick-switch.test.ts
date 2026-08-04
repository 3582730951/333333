import { describe, expect, it } from 'vitest';
import { setSuperInstructProfilesEnabled } from '../src/pages/Groups.jsx';

describe('user-group Super-Instruct quick switch', () => {
  it('toggles only instruction injection and preserves selected skills', () => {
    const profiles = {
      gpt: {
        enabled: false,
        skill_ids: ['skill-a', 'skill-a', 'skill-b'],
        response_rewrite_enabled: true,
        memory_enabled: true,
        monitor_enabled: true,
      },
    };

    const enabled = setSuperInstructProfilesEnabled(profiles, true);
    expect(enabled.gpt).toEqual({
      enabled: true,
      skill_ids: ['skill-a', 'skill-b'],
      response_rewrite_enabled: false,
      memory_enabled: false,
      monitor_enabled: false,
    });
    expect(enabled.claude).toEqual({
      enabled: true,
      skill_ids: [],
      response_rewrite_enabled: false,
      memory_enabled: false,
      monitor_enabled: false,
    });

    const disabled = setSuperInstructProfilesEnabled(enabled, false);
    expect(disabled.gpt.enabled).toBe(false);
    expect(disabled.gpt.skill_ids).toEqual(['skill-a', 'skill-b']);
    expect(disabled.gpt.response_rewrite_enabled).toBe(false);
    expect(disabled.gpt.memory_enabled).toBe(false);
    expect(disabled.gpt.monitor_enabled).toBe(false);
  });
});
