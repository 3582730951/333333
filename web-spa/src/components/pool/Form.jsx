import React, { createContext, useCallback, useContext, useEffect, useId, useMemo, useRef, useState } from 'react';
import * as PopoverPrimitive from '@radix-ui/react-popover';
import * as SwitchPrimitive from '@radix-ui/react-switch';

import { Button } from './Button.jsx';
import { requestBrowserAnimationFrame } from '../../lib/browserLifecycle.js';

const FormContext = createContext(null);

function cx(...parts) {
  return parts.filter(Boolean).join(' ');
}

function getInitialValues(initValues) {
  return initValues && typeof initValues === 'object' ? { ...initValues } : {};
}

function initialValuesKey(initValues) {
  try {
    return JSON.stringify(initValues && typeof initValues === 'object' ? initValues : {});
  } catch {
    return null;
  }
}

function validateField(value, rules = []) {
  for (const rule of rules) {
    if (rule?.required && (value === undefined || value === null || value === '')) {
      return rule.message || '必填';
    }
    if (value === undefined || value === null || value === '') continue;
    const text = String(value);
    if (rule?.type === 'email' && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(text)) {
      return rule.message || '邮箱格式无效';
    }
    if (Number.isFinite(rule?.min) && text.length < rule.min) {
      return rule.message || `至少输入 ${rule.min} 个字符`;
    }
    if (Number.isFinite(rule?.max) && text.length > rule.max) {
      return rule.message || `最多输入 ${rule.max} 个字符`;
    }
    if (rule?.pattern instanceof RegExp && !rule.pattern.test(text)) {
      return rule.message || '格式无效';
    }
  }
  return '';
}

function FieldShell({ field, label, help, rules, controlId, children }) {
  const form = useContext(FormContext);
  const error = form?.errors?.[field] || '';
  const errorId = controlId ? `${controlId}-error` : undefined;
  const helpId = controlId ? `${controlId}-help` : undefined;
  return (
    <div className={cx('pool-field', form?.labelPosition === 'left' ? 'pool-field--left' : '')} data-pool-field={field || undefined} data-error={error ? 'true' : undefined}>
      {label ? controlId
        ? <label className="pool-field__label" htmlFor={controlId}>{label}</label>
        : <span className="pool-field__label">{label}</span> : null}
      <span>
        {children}
        {error ? <div id={errorId} className="pool-field__error" role="alert">{error}</div> : help ? <div id={helpId} className="pool-field__help">{help}</div> : null}
      </span>
    </div>
  );
}

function useField(field, rules, externalValue, externalOnChange, initValue) {
  const form = useContext(FormContext);
  const value = externalValue !== undefined ? externalValue : form?.values?.[field] ?? '';
  useEffect(() => {
    if (!form || !field || initValue === undefined) return;
    if (form.values[field] !== undefined) return;
    form.setValues((current) => ({ ...current, [field]: initValue }));
  }, [field, form, initValue]);
  const setValue = useCallback((next) => {
    if (externalOnChange) externalOnChange(next);
    if (form && field) {
      form.setValues((current) => ({ ...current, [field]: next }));
      form.notifyValueChange?.(field, next);
      if (form.errors[field]) {
        const message = validateField(next, rules);
        form.setErrors((current) => ({ ...current, [field]: message }));
      }
    }
  }, [externalOnChange, field, form, rules]);
  return [value, setValue];
}

export function TextInput({
  field,
  label,
  help,
  rules,
  value,
  onChange,
  onEnterPress,
  prefix,
  showClear,
  allowReveal,
  onClear,
  mode,
  className,
  style,
  initValue,
  ...props
}) {
  const generatedId = useId();
  const controlId = props.id || (field ? `pool-field-${field}-${generatedId}` : generatedId);
  const fieldError = useContext(FormContext)?.errors?.[field] || '';
  const describedBy = [props['aria-describedby'], fieldError ? `${controlId}-error` : help ? `${controlId}-help` : ''].filter(Boolean).join(' ') || undefined;
  const [current, setCurrent] = useField(field, rules, value, onChange, initValue);
  const [revealed, setRevealed] = useState(false);
  const inputType = mode === 'password' && !revealed ? 'password' : 'text';
  const input = (
    <input
      className={cx(prefix || showClear ? 'pool-input' : 'pool-input', className)}
      id={controlId}
      type={inputType}
      value={current ?? ''}
      onChange={(event) => setCurrent(event.target.value)}
      onKeyDown={(event) => {
        if (event.key === 'Enter' && onEnterPress) onEnterPress(event);
      }}
      style={!prefix && !showClear ? style : undefined}
      {...props}
      name={props.name || field}
      aria-invalid={props['aria-invalid'] ?? (fieldError ? true : undefined)}
      aria-describedby={describedBy}
    />
  );
  const control = prefix || showClear || allowReveal ? (
    <span className="pool-input-wrap" style={style}>
      {prefix ? <span className="pool-button__icon">{prefix}</span> : null}
      {input}
      {showClear && current ? <Button theme="borderless" size="small" onClick={() => { setCurrent(''); onClear?.(); }} aria-label="清除">×</Button> : null}
      {allowReveal && mode === 'password' ? (
        <Button
          theme="borderless"
          size="small"
          onClick={() => setRevealed((value) => !value)}
          aria-label={revealed ? '隐藏内容' : '显示内容'}
        >
          {revealed ? '隐藏' : '显示'}
        </Button>
      ) : null}
    </span>
  ) : input;
  if (!field && !label) return control;
  return <FieldShell field={field} label={label} help={help} rules={rules} controlId={controlId}>{control}</FieldShell>;
}

export function NumberInput({ field, label, help, rules, value, onChange, min, max, className, initValue, ...props }) {
  const generatedId = useId();
  const controlId = props.id || (field ? `pool-field-${field}-${generatedId}` : generatedId);
  const fieldError = useContext(FormContext)?.errors?.[field] || '';
  const describedBy = [props['aria-describedby'], fieldError ? `${controlId}-error` : help ? `${controlId}-help` : ''].filter(Boolean).join(' ') || undefined;
  const [current, setCurrent] = useField(field, rules, value, onChange, initValue);
  return (
    <FieldShell field={field} label={label} help={help} rules={rules} controlId={controlId}>
      <input
        className={cx('pool-input', className)}
        id={controlId}
        type="number"
        value={current ?? ''}
        min={min}
        max={max}
        onChange={(event) => {
          const raw = event.target.value;
          setCurrent(raw === '' ? '' : Number(raw));
        }}
        {...props}
        name={props.name || field}
        aria-invalid={props['aria-invalid'] ?? (fieldError ? true : undefined)}
        aria-describedby={describedBy}
      />
    </FieldShell>
  );
}

export function Textarea({ field, label, help, rules, value, onChange, autosize, className, initValue, ...props }) {
  const generatedId = useId();
  const controlId = props.id || (field ? `pool-field-${field}-${generatedId}` : generatedId);
  const fieldError = useContext(FormContext)?.errors?.[field] || '';
  const describedBy = [props['aria-describedby'], fieldError ? `${controlId}-error` : help ? `${controlId}-help` : ''].filter(Boolean).join(' ') || undefined;
  const [current, setCurrent] = useField(field, rules, value, onChange, initValue);
  return (
    <FieldShell field={field} label={label} help={help} rules={rules} controlId={controlId}>
      <textarea
        className={cx('pool-textarea', className)}
        id={controlId}
        value={current ?? ''}
        onChange={(event) => setCurrent(event.target.value)}
        rows={autosize ? 4 : props.rows}
        {...props}
        name={props.name || field}
        aria-invalid={props['aria-invalid'] ?? (fieldError ? true : undefined)}
        aria-describedby={describedBy}
      />
    </FieldShell>
  );
}

function normalizeOptions(optionList = []) {
  return optionList.map((item) => {
    if (item && typeof item === 'object') return item;
    return { label: String(item), value: item };
  });
}

function SearchableSingleSelect({ controlId, options, current, setCurrent, placeholder, className, style, emptyContent, disabled, ariaLabel }) {
  const [query, setQuery] = useState('');
  const [open, setOpen] = useState(false);
  const [activeIndex, setActiveIndex] = useState(0);
  const searchRef = useRef(null);
  const optionRefs = useRef(new Map());
  const currentKey = current === undefined || current === null ? '' : String(current);
  const valueMap = new Map(options.map((option) => [String(option.value), option.value]));
  const labelMap = new Map(options.map((option) => [String(option.value), option.label]));
  const normalizedQuery = query.trim().toLocaleLowerCase();
  const filteredOptions = normalizedQuery
    ? options.filter((option) => `${String(option.label ?? '')} ${String(option.value ?? '')}`.toLocaleLowerCase().includes(normalizedQuery))
    : options;
  const enabledIndices = filteredOptions.reduce((indices, option, index) => {
    if (!option.disabled) indices.push(index);
    return indices;
  }, []);
  const enabledIndexKey = enabledIndices.join(',');
  const listboxId = `${controlId}-listbox`;
  const activeOptionId = filteredOptions[activeIndex] ? `${controlId}-option-${activeIndex}` : undefined;
  const hasDisplayedValue = currentKey !== '' || (!placeholder && labelMap.has(currentKey));

  useEffect(() => {
    setActiveIndex(enabledIndices.length ? enabledIndices[0] : 0);
  }, [filteredOptions.length, normalizedQuery, enabledIndexKey]);

  useEffect(() => {
    if (!open || !filteredOptions[activeIndex]) return;
    optionRefs.current.get(String(filteredOptions[activeIndex].value))?.scrollIntoView?.({ block: 'nearest' });
  }, [activeIndex, filteredOptions, open]);

  const resetActiveOption = (fromEnd = false) => {
    const selectedIndex = filteredOptions.findIndex((option) => !option.disabled && String(option.value) === currentKey);
    if (selectedIndex >= 0) {
      setActiveIndex(selectedIndex);
      return;
    }
    if (enabledIndices.length) setActiveIndex(fromEnd ? enabledIndices[enabledIndices.length - 1] : enabledIndices[0]);
  };

  const closeOptions = () => {
    setOpen(false);
    setQuery('');
    setActiveIndex(0);
  };

  const selectOption = (option) => {
    if (!option || option.disabled || disabled) return;
    const key = String(option.value);
    setCurrent(valueMap.has(key) ? valueMap.get(key) : option.value);
    closeOptions();
  };

  const handleOptionKeys = (event) => {
    if (event.key === 'Escape') {
      closeOptions();
      return;
    }
    if (!enabledIndices.length) return;
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      const delta = event.key === 'ArrowDown' ? 1 : -1;
      setActiveIndex((index) => {
        const position = enabledIndices.indexOf(index);
        const currentPosition = position < 0 ? (delta > 0 ? -1 : 0) : position;
        return enabledIndices[(currentPosition + delta + enabledIndices.length) % enabledIndices.length];
      });
      return;
    }
    if (event.key === 'Home' || event.key === 'End') {
      event.preventDefault();
      setActiveIndex(event.key === 'Home' ? enabledIndices[0] : enabledIndices[enabledIndices.length - 1]);
      return;
    }
    if (event.key === 'Enter') {
      event.preventDefault();
      selectOption(filteredOptions[activeIndex]);
    }
  };

  return (
    <PopoverPrimitive.Root open={open} onOpenChange={(nextOpen) => {
      if (disabled) return;
      if (nextOpen) {
        setOpen(true);
        resetActiveOption();
      } else closeOptions();
    }}>
      <PopoverPrimitive.Trigger asChild>
        <button
          type="button"
          id={controlId}
          className={cx('pool-multi-select pool-single-select', className)}
          style={style}
          role="combobox"
          aria-haspopup="listbox"
          aria-expanded={open}
          aria-controls={listboxId}
          aria-activedescendant={open ? activeOptionId : undefined}
          aria-disabled={disabled || undefined}
          aria-label={ariaLabel || placeholder || '选项'}
          disabled={disabled}
          onKeyDown={(event) => {
            if (disabled) return;
            if (event.key === 'Enter' || event.key === ' ' || event.key === 'ArrowDown' || event.key === 'ArrowUp') {
              event.preventDefault();
              resetActiveOption(event.key === 'ArrowUp');
              setOpen(true);
            }
          }}
        >
          <span className={cx('pool-single-select__value', hasDisplayedValue ? '' : 'pool-multi-select__placeholder')}>
            {hasDisplayedValue ? labelMap.get(currentKey) ?? currentKey : placeholder || '请选择'}
          </span>
          <span className="pool-multi-select__chevron" aria-hidden="true">⌄</span>
        </button>
      </PopoverPrimitive.Trigger>
      <PopoverPrimitive.Content
        className="pool-multi-select__content"
        align="start"
        sideOffset={4}
        onOpenAutoFocus={(event) => {
          event.preventDefault();
          requestBrowserAnimationFrame(() => searchRef.current?.focus());
        }}
      >
        <input
          ref={searchRef}
          className="pool-input pool-multi-select__search"
          type="search"
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          onKeyDown={handleOptionKeys}
          role="combobox"
          aria-label={placeholder || `${ariaLabel || '选项'}搜索`}
          aria-autocomplete="list"
          aria-expanded={open}
          aria-controls={listboxId}
          aria-activedescendant={activeOptionId}
          autoComplete="off"
          spellCheck={false}
          placeholder={placeholder || '搜索...'}
        />
        <div
          id={listboxId}
          className="pool-multi-select__options"
          role="listbox"
        >
          {filteredOptions.length ? filteredOptions.map((option, index) => {
            const key = String(option.value);
            const selected = key === currentKey;
            return (
              <button
                type="button"
                key={option.key || key}
                id={`${controlId}-option-${index}`}
                ref={(node) => {
                  if (node) optionRefs.current.set(key, node);
                  else optionRefs.current.delete(key);
                }}
                className={cx('pool-multi-select__option', selected ? 'is-selected' : '', index === activeIndex ? 'is-active' : '')}
                role="option"
                aria-selected={selected}
                tabIndex={-1}
                disabled={option.disabled}
                onPointerMove={() => { if (!option.disabled) setActiveIndex(index); }}
                onClick={() => selectOption(option)}
              >
                <span className="pool-single-select__option-label">{option.label}</span>
                <span className="pool-multi-select__check" aria-hidden="true">{selected ? '✓' : ''}</span>
              </button>
            );
          }) : <div className="pool-multi-select__empty" role="status">{emptyContent || '暂无选项'}</div>}
        </div>
      </PopoverPrimitive.Content>
    </PopoverPrimitive.Root>
  );
}

function MultiSelectControl({ controlId, options, current, setCurrent, placeholder, className, style, filter, emptyContent, maxTagCount, disabled, ariaLabel }) {
  const [query, setQuery] = useState('');
  const [open, setOpen] = useState(false);
  const [activeIndex, setActiveIndex] = useState(0);
  const searchRef = useRef(null);
  const listRef = useRef(null);
  const selected = Array.isArray(current) ? current : current === undefined || current === null || current === '' ? [] : [current];
  const selectedKeys = new Set(selected.map((item) => String(item)));
  const valueMap = new Map(options.map((option) => [String(option.value), option.value]));
  const labelMap = new Map(options.map((option) => [String(option.value), option.label]));
  const visibleLimit = Number.isFinite(maxTagCount) && maxTagCount >= 0 ? maxTagCount : selected.length;
  const visibleSelected = selected.slice(0, visibleLimit);
  const hiddenCount = selected.length - visibleSelected.length;
  const normalizedQuery = query.trim().toLowerCase();
  const filteredOptions = normalizedQuery
    ? options.filter((option) => String(option.label ?? option.value).toLowerCase().includes(normalizedQuery))
    : options;
  const enabledIndices = filteredOptions.reduce((indices, option, index) => {
    if (!option.disabled) indices.push(index);
    return indices;
  }, []);
  const enabledIndexKey = enabledIndices.join(',');

  useEffect(() => {
    setActiveIndex((index) => {
      if (!enabledIndices.length) return 0;
      return enabledIndices.includes(index) ? index : enabledIndices[0];
    });
  }, [filteredOptions.length, normalizedQuery, enabledIndexKey]);

  const toggleValue = (raw) => {
    if (disabled) return;
    const key = String(raw);
    if (options.find((option) => String(option.value) === key)?.disabled) return;
    if (selectedKeys.has(key)) {
      setCurrent(selected.filter((item) => String(item) !== key));
      return;
    }
    setCurrent([...selected, valueMap.has(key) ? valueMap.get(key) : raw]);
  };

  const handleOptionKeys = (event) => {
    if (!enabledIndices.length) return;
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      const delta = event.key === 'ArrowDown' ? 1 : -1;
      setActiveIndex((index) => {
        const position = enabledIndices.indexOf(index);
        const current = position < 0 ? (delta > 0 ? -1 : 0) : position;
        return enabledIndices[(current + delta + enabledIndices.length) % enabledIndices.length];
      });
      return;
    }
    if (event.key === 'Home' || event.key === 'End') {
      event.preventDefault();
      setActiveIndex(event.key === 'Home' ? enabledIndices[0] : enabledIndices[enabledIndices.length - 1]);
      return;
    }
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      toggleValue(filteredOptions[activeIndex]?.value);
    }
  };

  const listboxId = `${controlId}-listbox`;
  const activeOptionId = filteredOptions[activeIndex] ? `${controlId}-option-${activeIndex}` : undefined;

  return (
    <PopoverPrimitive.Root open={open} onOpenChange={(nextOpen) => {
      if (disabled) return;
      setOpen(nextOpen);
      if (!nextOpen) {
        setQuery('');
        setActiveIndex(0);
      }
    }}>
      <PopoverPrimitive.Trigger asChild>
        <div
          id={controlId}
          className={cx('pool-multi-select', className)}
          style={style}
          role="combobox"
          aria-haspopup="listbox"
          aria-expanded={open}
          aria-controls={listboxId}
          aria-activedescendant={open ? activeOptionId : undefined}
          aria-disabled={disabled || undefined}
          aria-label={ariaLabel}
          tabIndex={disabled ? -1 : 0}
          onKeyDown={(event) => {
            if (disabled) return;
            if (event.key === 'Enter' || event.key === ' ' || event.key === 'ArrowDown' || event.key === 'ArrowUp') {
              event.preventDefault();
              if (event.key === 'ArrowUp' && enabledIndices.length) setActiveIndex(enabledIndices[enabledIndices.length - 1]);
              else if (enabledIndices.length) setActiveIndex(enabledIndices[0]);
              setOpen(true);
            }
          }}
        >
          <span className="pool-multi-select__values">
            {visibleSelected.length ? visibleSelected.map((item) => {
              const key = String(item);
              return (
                <span className="pool-multi-select__tag" key={key}>
                  <span>{labelMap.get(key) ?? key}</span>
                  <button
                    type="button"
                    className="pool-multi-select__remove"
                    aria-label={`移除 ${labelMap.get(key) ?? key}`}
                    onPointerDown={(event) => event.stopPropagation()}
                    onClick={(event) => { event.stopPropagation(); toggleValue(item); }}
                  >×</button>
                </span>
              );
            }) : <span className="pool-multi-select__placeholder">{placeholder || '请选择'}</span>}
            {hiddenCount > 0 ? <span className="pool-multi-select__tag">+{hiddenCount}</span> : null}
          </span>
          <span className="pool-multi-select__chevron" aria-hidden="true">⌄</span>
        </div>
      </PopoverPrimitive.Trigger>
      <PopoverPrimitive.Content
        className="pool-multi-select__content"
        align="start"
        sideOffset={4}
        onOpenAutoFocus={(event) => {
          event.preventDefault();
          requestBrowserAnimationFrame(() => (filter ? searchRef.current : listRef.current)?.focus());
        }}
      >
        {filter ? (
          <input
            ref={searchRef}
            className="pool-input pool-multi-select__search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            onKeyDown={handleOptionKeys}
            role="combobox"
            aria-label={`${ariaLabel || '选项'}搜索`}
            aria-expanded={open}
            aria-controls={listboxId}
            aria-activedescendant={activeOptionId}
            placeholder="搜索..."
          />
        ) : null}
        <div
          ref={listRef}
          id={listboxId}
          className="pool-multi-select__options"
          role="listbox"
          aria-multiselectable="true"
          aria-activedescendant={activeOptionId}
          tabIndex={filter ? -1 : 0}
          onKeyDown={handleOptionKeys}
        >
          {filteredOptions.length ? filteredOptions.map((option, index) => {
            const key = String(option.value);
            const checked = selectedKeys.has(key);
            return (
              <button
                type="button"
                key={option.key || key}
                id={`${controlId}-option-${index}`}
                className={cx('pool-multi-select__option', checked ? 'is-selected' : '', index === activeIndex ? 'is-active' : '')}
                role="option"
                aria-selected={checked}
                tabIndex={-1}
                disabled={option.disabled}
                onPointerMove={() => { if (!option.disabled) setActiveIndex(index); }}
                onClick={() => toggleValue(option.value)}
              >
                <span>{option.label}</span>
                <span className="pool-multi-select__check" aria-hidden="true">{checked ? '✓' : ''}</span>
              </button>
            );
          }) : <div className="pool-multi-select__empty">{emptyContent || '暂无选项'}</div>}
        </div>
      </PopoverPrimitive.Content>
    </PopoverPrimitive.Root>
  );
}

export function SelectInput({ field, label, help, rules, value, onChange, optionList = [], placeholder, className, style, children, initValue, filter, emptyContent, multiple = false, maxTagCount, ...props }) {
  const generatedId = useId();
  const controlId = props.id || (field ? `pool-field-${field}-${generatedId}` : generatedId);
  const options = normalizeOptions(optionList);
  const [current, setCurrent] = useField(field, rules, value, onChange, initValue);
  if (multiple) {
    const control = (
      <MultiSelectControl
        controlId={controlId}
        options={options}
        current={current}
        setCurrent={setCurrent}
        placeholder={placeholder}
        className={className}
        style={style}
        filter={filter}
        emptyContent={emptyContent}
        maxTagCount={maxTagCount}
        disabled={props.disabled}
        ariaLabel={props['aria-label']}
      />
    );
    if (!field && !label) return control;
    return <FieldShell field={field} label={label} help={help} rules={rules} controlId={controlId}>{control}</FieldShell>;
  }
  if (filter) {
    const control = (
      <SearchableSingleSelect
        controlId={controlId}
        options={options}
        current={current}
        setCurrent={setCurrent}
        placeholder={placeholder}
        className={className}
        style={style}
        emptyContent={emptyContent}
        disabled={props.disabled}
        ariaLabel={props['aria-label'] || label}
      />
    );
    if (!field && !label) return control;
    return <FieldShell field={field} label={label} help={help} rules={rules} controlId={controlId}>{control}</FieldShell>;
  }
  const valueMap = new Map(options.map((option) => [String(option.value), option.value]));
  const select = (
    <select
      className={cx('pool-select', className)}
      id={controlId}
      value={current === undefined || current === null ? '' : String(current)}
      onChange={(event) => {
        const raw = event.target.value;
        setCurrent(valueMap.has(raw) ? valueMap.get(raw) : raw);
      }}
      style={style}
      {...props}
    >
      {placeholder ? <option value="">{placeholder}</option> : null}
      {!options.length && emptyContent ? <option value="" disabled>{emptyContent}</option> : null}
      {children}
      {options.map((option) => (
        <option key={String(option.value)} value={String(option.value)} disabled={option.disabled}>
          {option.label}
        </option>
      ))}
    </select>
  );
  if (!field && !label) return select;
  return <FieldShell field={field} label={label} help={help} rules={rules} controlId={controlId}>{select}</FieldShell>;
}

export function Toggle({ field, label, help, value, checked, onChange, disabled, loading = false, className, initValue, ...props }) {
  const generatedId = useId();
  const controlId = props.id || (field ? `pool-field-${field}-${generatedId}` : generatedId);
  const form = useContext(FormContext);
  useEffect(() => {
    if (!form || !field || initValue === undefined) return;
    if (form.values[field] !== undefined) return;
    form.setValues((values) => ({ ...values, [field]: initValue }));
  }, [field, form, initValue]);
  const current = checked !== undefined ? checked : value !== undefined ? value : !!form?.values?.[field];
  const setCurrent = (next) => {
    onChange?.(next);
    if (form && field) {
      form.setValues((values) => ({ ...values, [field]: next }));
      form.notifyValueChange?.(field, next);
    }
  };
  const control = (
    <SwitchPrimitive.Root
      className={cx('pool-switch', className)}
      id={controlId}
      checked={!!current}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      onCheckedChange={setCurrent}
      {...props}
    >
      <SwitchPrimitive.Thumb className="pool-switch__thumb" />
    </SwitchPrimitive.Root>
  );
  if (!field && !label) return control;
  return <FieldShell field={field} label={label} help={help} controlId={controlId}>{control}</FieldShell>;
}

export function Form({
  children,
  initValues,
  onSubmit,
  onValueChange,
  getFormApi,
  labelPosition = 'top',
  labelWidth,
  className,
  style,
  ...props
}) {
  const [values, setValues] = useState(() => getInitialValues(initValues));
  const [errors, setErrors] = useState({});
  const fieldsRef = useRef(new Map());
  const formRef = useRef(null);
  const initKey = initialValuesKey(initValues);

  useEffect(() => {
    setValues(getInitialValues(initValues));
    setErrors({});
  }, [initKey]);

  const api = useMemo(() => ({
    getValues: () => ({ ...values }),
    getValue: (field) => values[field],
    setValue: (field, value) => {
      setValues((current) => ({ ...current, [field]: value }));
      setErrors((current) => {
        if (!current[field]) return current;
        const next = { ...current };
        delete next[field];
        return next;
      });
    },
    setValues: (next) => {
      setValues((current) => ({ ...current, ...(typeof next === 'function' ? next(current) : next) }));
    },
    reset: () => {
      setValues(getInitialValues(initValues));
      setErrors({});
    },
    submitForm: () => {
      const nextErrors = {};
      for (const [field, rules] of fieldsRef.current.entries()) {
        const message = validateField(values[field], rules);
        if (message) nextErrors[field] = message;
      }
      setErrors(nextErrors);
      const firstError = Object.keys(nextErrors)[0];
      if (!firstError) {
        onSubmit?.({ ...values });
        return;
      }
      requestBrowserAnimationFrame(() => {
        const field = Array.from(formRef.current?.querySelectorAll('[data-pool-field]') || [])
          .find((node) => node.dataset.poolField === firstError);
        field?.querySelector('input, textarea, button, [tabindex]:not([tabindex="-1"])')?.focus();
      });
    },
    validate: async () => {
      const nextErrors = {};
      for (const [field, rules] of fieldsRef.current.entries()) {
        const message = validateField(values[field], rules);
        if (message) nextErrors[field] = message;
      }
      setErrors(nextErrors);
      if (Object.keys(nextErrors).length > 0) {
        const error = new Error('validation failed');
        error.errorFields = Object.entries(nextErrors).map(([field, message]) => ({ field, message }));
        throw error;
      }
      return { ...values };
    },
  }), [initValues, onSubmit, values]);

  useEffect(() => {
    getFormApi?.(api);
  }, [api, getFormApi]);

  const context = useMemo(() => ({
    values,
    setValues,
    notifyValueChange: (field, value) => onValueChange?.(
      { ...values, [field]: value },
      { [field]: value },
    ),
    errors,
    setErrors,
    labelPosition,
    register: (field, rules) => {
      if (field) fieldsRef.current.set(field, rules || []);
    },
  }), [errors, labelPosition, onValueChange, values]);

  const registerFields = (node) => {
    React.Children.forEach(node, (child) => {
      if (!React.isValidElement(child)) return;
      if (child.props?.field) fieldsRef.current.set(child.props.field, child.props.rules || []);
      if (child.props?.children) registerFields(child.props.children);
    });
  };
  registerFields(children);

  return (
    <FormContext.Provider value={context}>
      <form
        ref={formRef}
        className={cx('pool-form', className)}
        style={{ '--pool-form-label-width': typeof labelWidth === 'number' ? `${labelWidth}px` : labelWidth, ...(style || {}) }}
        onSubmit={(event) => {
          event.preventDefault();
          api.submitForm();
        }}
        {...props}
      >
        {children}
      </form>
    </FormContext.Provider>
  );
}

function FormSlot({ label, help, children }) {
  return <FieldShell label={label} help={help}>{children}</FieldShell>;
}

Form.Input = TextInput;
Form.InputNumber = NumberInput;
Form.TextArea = Textarea;
Form.Select = SelectInput;
Form.Switch = Toggle;
Form.Slot = FormSlot;

export const Input = TextInput;
export const InputNumber = NumberInput;
export const Select = SelectInput;
export const Switch = Toggle;
