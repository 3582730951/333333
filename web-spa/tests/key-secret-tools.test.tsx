import React from 'react';
import { render, screen } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';

import { installCommand, KeyReveal } from '../src/components/KeySecretTools.jsx';
import { setLocale } from '../src/lib/i18n.js';

const Reveal = KeyReveal as any;

describe('API key one-click installer', () => {
  beforeEach(() => setLocale('zh'));

  it('keeps the API key in the generated /file command and explains the bounded Codex choice', () => {
    const command = installCommand('cap_slash/value');
    expect(command).toContain('/file/cap_slash%2Fvalue');
    expect(command).toMatch(/^curl -fsSL '.+' \| bash$/);

    render(<Reveal secret="cap_slash/value" />);
    expect(screen.getByText(/配置 Codex 时可选择 Super-Instruct/)).toHaveTextContent('仍受 API Key 所属用户分组策略限制');
  });

  it('states the same entitlement boundary in English', () => {
    setLocale('en');
    render(<Reveal secret="cap_english" />);
    expect(screen.getByText(/Codex setup offers a Super-Instruct choice/)).toHaveTextContent('bounded by the API key user-group policy');
  });
});
