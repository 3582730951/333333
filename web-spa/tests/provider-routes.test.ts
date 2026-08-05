import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { ProviderEditor, providerFormValues, providerRoutesPayload } from '../src/pages/Providers.jsx';

describe('provider invocation routes', () => {
  it('adapts stored routes into an isolated editable copy', () => {
    const source = {
      id: 'relay', base_url: 'https://default.example/v1',
      routes: [{ id: 'codex', downstream_path: '/v1/responses', base_url: 'https://responses.example/v1' }],
    };
    const values = providerFormValues(source);
    expect(values.routes).toEqual(source.routes);
    values.routes[0].base_url = 'https://changed.example/v1';
    expect(source.routes[0].base_url).toBe('https://responses.example/v1');
  });

  it('writes compact typed route payloads and drops blank rows', () => {
    expect(providerRoutesPayload([
      {
        id: ' codex-edge ', downstream_path: ' /v1/responses ',
        base_url: ' https://relay.example/v1 ', upstream_protocol: ' responses ',
        transport_profile: ' codex_cli ',
      },
      { id: '', downstream_path: '  ', base_url: '' },
    ])).toEqual([{
      id: 'codex-edge', downstream_path: '/v1/responses',
      base_url: 'https://relay.example/v1', upstream_protocol: 'responses',
      transport_profile: 'codex_cli',
    }]);
  });

  it('provides roving focus and standard radio keyboard selection for transport profiles', () => {
    render(React.createElement(ProviderEditor, {
      editor: { mode: 'create', values: providerFormValues() },
      egressOptions: [],
      saving: false,
      onCancel: () => undefined,
      onSave: () => undefined,
    }));

    const generic = screen.getByRole('radio', { name: /OpenAI Chat/ });
    const codex = screen.getByRole('radio', { name: /Codex CLI/ });
    const claude = screen.getByRole('radio', { name: /Claude Code/ });
    expect(generic).toHaveAttribute('tabindex', '0');
    expect(codex).toHaveAttribute('tabindex', '-1');

    generic.focus();
    fireEvent.keyDown(generic, { key: 'ArrowRight' });
    expect(codex).toHaveFocus();
    expect(codex).toHaveAttribute('aria-checked', 'true');
    expect(codex).toHaveAttribute('tabindex', '0');

    fireEvent.keyDown(codex, { key: 'End' });
    expect(claude).toHaveFocus();
    expect(claude).toHaveAttribute('aria-checked', 'true');

    fireEvent.keyDown(claude, { key: 'Home' });
    expect(generic).toHaveFocus();
    expect(generic).toHaveAttribute('aria-checked', 'true');
  });
});
