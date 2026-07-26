import React, { useMemo, useState } from 'react';
import { Button, Select, Tag } from './pool/index.jsx';

function uniqueValues(values) {
  const seen = new Set();
  return (Array.isArray(values) ? values : []).filter((value) => {
    const key = String(value || '').trim();
    if (!key || seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

export default function OrderedEgressSelect({ value = [], onChange, options = [], disabled = false, label = '出口', help }) {
  const [dragIndex, setDragIndex] = useState(null);
  const values = uniqueValues(value);
  const labels = useMemo(() => new Map(options.map((option) => [String(option.value), option.label])), [options]);

  const commit = (next) => onChange?.(uniqueValues(next));
  const move = (index, delta) => {
    const target = index + delta;
    if (target < 0 || target >= values.length) return;
    const next = [...values];
    [next[index], next[target]] = [next[target], next[index]];
    commit(next);
  };

  return (
    <div className="pool-ordered-select">
      <Select
        multiple
        filter
        maxTagCount={6}
        value={values}
        onChange={commit}
        optionList={options}
        placeholder={`搜索并选择${label}`}
        aria-label={`选择${label}`}
        disabled={disabled}
        style={{ width: '100%' }}
      />
      {help ? <div className="pool-field__help">{help}</div> : null}
      {values.length ? (
        <ol className="pool-ordered-select__list" aria-label={`${label}故障转移顺序`}>
          {values.map((id, index) => (
            <li
              key={id}
              draggable={!disabled}
              onDragStart={() => setDragIndex(index)}
              onDragEnd={() => setDragIndex(null)}
              onDragOver={(event) => event.preventDefault()}
              onDrop={() => {
                if (dragIndex === null || dragIndex === index) return;
                const next = [...values];
                const [moved] = next.splice(dragIndex, 1);
                next.splice(index, 0, moved);
                commit(next);
                setDragIndex(null);
              }}
            >
              <button
                type="button"
                className="pool-ordered-select__handle"
                disabled={disabled}
                aria-label={`${labels.get(String(id)) || id}，使用上下方向键调整顺序`}
                onKeyDown={(event) => {
                  if (event.key !== 'ArrowUp' && event.key !== 'ArrowDown') return;
                  event.preventDefault();
                  move(index, event.key === 'ArrowUp' ? -1 : 1);
                }}
              >
                <span aria-hidden="true">⋮⋮</span>
                <span>{labels.get(String(id)) || id}</span>
              </button>
              <Tag size="small" color={index === 0 ? 'green' : 'blue'}>{index === 0 ? '主出口' : `备用 ${index}`}</Tag>
              <div className="pool-ordered-select__actions">
                <Button size="small" disabled={disabled || index === 0} onClick={() => move(index, -1)} aria-label={`上移 ${labels.get(String(id)) || id}`}>↑</Button>
                <Button size="small" disabled={disabled || index === values.length - 1} onClick={() => move(index, 1)} aria-label={`下移 ${labels.get(String(id)) || id}`}>↓</Button>
              </div>
            </li>
          ))}
        </ol>
      ) : <div className="pool-ordered-select__empty">未配置显式出口，将使用系统默认路由。</div>}
    </div>
  );
}
