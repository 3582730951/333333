import { useEffect, useMemo, useRef, useState } from 'react';
import { Modal } from './pool/index.jsx';
import { IconSearch } from './pool/icons.jsx';

export interface CommandPaletteItem {
  key: string;
  label: string;
  group: string;
  keywords?: string;
  onSelect: () => void;
}

interface CommandPaletteProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  items: ReadonlyArray<CommandPaletteItem>;
  title: string;
  placeholder: string;
  emptyText: string;
}

export default function CommandPalette({
  open,
  onOpenChange,
  items,
  title,
  placeholder,
  emptyText,
}: CommandPaletteProps) {
  const [query, setQuery] = useState('');
  const [activeIndex, setActiveIndex] = useState(0);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const normalized = query.trim().toLocaleLowerCase();
  const filtered = useMemo(() => normalized
    ? items.filter((item) => `${item.label} ${item.group} ${item.keywords || ''}`.toLocaleLowerCase().includes(normalized))
    : [...items], [items, normalized]);

  useEffect(() => {
    if (!open) {
      setQuery('');
      setActiveIndex(0);
      return;
    }
    queueMicrotask(() => inputRef.current?.focus());
  }, [open]);

  useEffect(() => {
    if (activeIndex >= filtered.length) setActiveIndex(Math.max(0, filtered.length - 1));
  }, [activeIndex, filtered.length]);

  const select = (index: number) => {
    const item = filtered[index];
    if (!item) return;
    onOpenChange(false);
    item.onSelect();
  };

  return (
    <Modal
      visible={open}
      onCancel={() => onOpenChange(false)}
      title={title}
      description={placeholder}
      footer={null}
      width={620}
      className="pool-command-palette"
    >
      <div className="pool-command-search">
        <IconSearch aria-hidden="true" />
        <input
          ref={inputRef}
          type="search"
          value={query}
          onChange={(event) => { setQuery(event.target.value); setActiveIndex(0); }}
          onKeyDown={(event) => {
            if (event.key === 'ArrowDown' && filtered.length) {
              event.preventDefault();
              setActiveIndex((index) => (index + 1) % filtered.length);
            } else if (event.key === 'ArrowUp' && filtered.length) {
              event.preventDefault();
              setActiveIndex((index) => (index - 1 + filtered.length) % filtered.length);
            } else if (event.key === 'Home' && filtered.length) {
              event.preventDefault();
              setActiveIndex(0);
            } else if (event.key === 'End' && filtered.length) {
              event.preventDefault();
              setActiveIndex(filtered.length - 1);
            } else if (event.key === 'Enter') {
              event.preventDefault();
              select(activeIndex);
            }
          }}
          placeholder={placeholder}
          aria-label={placeholder}
          role="combobox"
          aria-autocomplete="list"
          aria-expanded={open}
          aria-haspopup="listbox"
          aria-controls="pool-command-results"
          aria-activedescendant={filtered[activeIndex] ? `pool-command-${filtered[activeIndex].key.replace(/[^a-zA-Z0-9_-]/g, '-')}` : undefined}
          autoComplete="off"
          spellCheck={false}
        />
        <kbd>Esc</kbd>
      </div>
      <div id="pool-command-results" className="pool-command-results" role="listbox" aria-label={title}>
        {filtered.length ? filtered.map((item, index) => (
          <button
            type="button"
            id={`pool-command-${item.key.replace(/[^a-zA-Z0-9_-]/g, '-')}`}
            key={item.key}
            className="pool-command-result"
            role="option"
            aria-selected={index === activeIndex}
            tabIndex={-1}
            onPointerMove={() => setActiveIndex(index)}
            onClick={() => select(index)}
          >
            <span>{item.label}</span>
            <small>{item.group}</small>
          </button>
        )) : <div className="pool-command-empty">{emptyText}</div>}
      </div>
    </Modal>
  );
}
