import React, { useMemo } from 'react';
import { Button, Select, Tag } from './pool/index.jsx';
import { t } from '../lib/i18n.js';

function uniqueValues(values) {
  return [...new Set((Array.isArray(values) ? values : []).map((value) => String(value || '').trim()).filter(Boolean))];
}

/** @param {{ value?: string[], onChange?: (value: string[]) => void, options?: {label: string, value: string}[], disabled?: boolean, label?: string, help?: React.ReactNode }} props */
export default function OrderedEgressSelect({ value = [], onChange, options = [], disabled = false, label = t('groups.balance.egress_order'), help }) {
  const values = uniqueValues(value);
  const labels = useMemo(() => new Map(options.map((option) => [String(option.value), option.label])), [options]);

  const commit = (next) => { if (!disabled) onChange?.(uniqueValues(next)); };
  const move = (index, delta) => {
    const target = index + delta;
    if (disabled || target < 0 || target >= values.length) return;
    const next = [...values];
    [next[index], next[target]] = [next[target], next[index]];
    commit(next);
  };

  return (
    <div className="pool-ordered-select">
      <span className="pool-field__label">{label}</span>
      <Select
        multiple
        filter
        maxTagCount={6}
        value={values}
        onChange={commit}
        optionList={options}
        placeholder={t('groups.balance.egress_search')}
        aria-label={label}
        disabled={disabled}
        style={{ width: '100%' }}
      />
      {help ? <div className="pool-field__help">{help}</div> : null}
      {values.length ? (
        <ol className="pool-ordered-select__list" aria-label={label}>
          {values.map((id, index) => (
            <li key={id}>
              <button
                type="button"
                className="pool-ordered-select__handle"
                disabled={disabled}
                aria-label={t('groups.balance.egress_move_keys').replace('{name}', labels.get(id) || id)}
                onKeyDown={(event) => {
                  if (event.key !== 'ArrowUp' && event.key !== 'ArrowDown') return;
                  event.preventDefault();
                  move(index, event.key === 'ArrowUp' ? -1 : 1);
                }}
              >
                <span aria-hidden="true">⋮⋮</span>
                <span>{labels.get(String(id)) || id}</span>
              </button>
              <Tag size="small" color={index === 0 ? 'green' : 'blue'}>{t('groups.balance.egress_rank').replace('{rank}', String(index + 1))}</Tag>
              <div className="pool-ordered-select__actions">
                <Button size="small" disabled={disabled || index === 0} onClick={() => move(index, -1)} aria-label={t('groups.balance.egress_move_up').replace('{name}', labels.get(id) || id)}>↑</Button>
                <Button size="small" disabled={disabled || index === values.length - 1} onClick={() => move(index, 1)} aria-label={t('groups.balance.egress_move_down').replace('{name}', labels.get(id) || id)}>↓</Button>
              </div>
            </li>
          ))}
        </ol>
      ) : <div className="pool-ordered-select__empty">{t('groups.balance.egress_empty')}</div>}
    </div>
  );
}
