import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import * as SwitchPrimitive from '@radix-ui/react-switch';

import { Button } from './Button.jsx';

const FormContext = createContext(null);

function cx(...parts) {
  return parts.filter(Boolean).join(' ');
}

function getInitialValues(initValues) {
  return initValues && typeof initValues === 'object' ? { ...initValues } : {};
}

function validateField(value, rules = []) {
  for (const rule of rules) {
    if (rule?.required && (value === undefined || value === null || value === '')) {
      return rule.message || '必填';
    }
  }
  return '';
}

function FieldShell({ field, label, help, rules, children }) {
  const form = useContext(FormContext);
  const error = form?.errors?.[field] || '';
  return (
    <label className={cx('pool-field', form?.labelPosition === 'left' ? 'pool-field--left' : '')}>
      {label ? <span className="pool-field__label">{label}</span> : null}
      <span>
        {children}
        {error ? <div className="pool-field__error">{error}</div> : help ? <div className="pool-field__help">{help}</div> : null}
      </span>
    </label>
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
  const [current, setCurrent] = useField(field, rules, value, onChange, initValue);
  const [revealed, setRevealed] = useState(false);
  const inputType = mode === 'password' && !revealed ? 'password' : 'text';
  const input = (
    <input
      className={cx(prefix || showClear ? 'pool-input' : 'pool-input', className)}
      type={inputType}
      value={current ?? ''}
      onChange={(event) => setCurrent(event.target.value)}
      onKeyDown={(event) => {
        if (event.key === 'Enter' && onEnterPress) onEnterPress(event);
      }}
      style={!prefix && !showClear ? style : undefined}
      {...props}
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
  return <FieldShell field={field} label={label} help={help} rules={rules}>{control}</FieldShell>;
}

export function NumberInput({ field, label, help, rules, value, onChange, min, max, className, initValue, ...props }) {
  const [current, setCurrent] = useField(field, rules, value, onChange, initValue);
  return (
    <FieldShell field={field} label={label} help={help} rules={rules}>
      <input
        className={cx('pool-input', className)}
        type="number"
        value={current ?? ''}
        min={min}
        max={max}
        onChange={(event) => {
          const raw = event.target.value;
          setCurrent(raw === '' ? '' : Number(raw));
        }}
        {...props}
      />
    </FieldShell>
  );
}

export function Textarea({ field, label, help, rules, value, onChange, autosize, className, initValue, ...props }) {
  const [current, setCurrent] = useField(field, rules, value, onChange, initValue);
  return (
    <FieldShell field={field} label={label} help={help} rules={rules}>
      <textarea
        className={cx('pool-textarea', className)}
        value={current ?? ''}
        onChange={(event) => setCurrent(event.target.value)}
        rows={autosize ? 4 : props.rows}
        {...props}
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

export function SelectInput({ field, label, help, rules, value, onChange, optionList = [], placeholder, className, style, children, initValue, ...props }) {
  const options = normalizeOptions(optionList);
  const [current, setCurrent] = useField(field, rules, value, onChange, initValue);
  const valueMap = new Map(options.map((option) => [String(option.value), option.value]));
  const select = (
    <select
      className={cx('pool-select', className)}
      value={current === undefined || current === null ? '' : String(current)}
      onChange={(event) => {
        const raw = event.target.value;
        setCurrent(valueMap.has(raw) ? valueMap.get(raw) : raw);
      }}
      style={style}
      {...props}
    >
      {placeholder ? <option value="">{placeholder}</option> : null}
      {children}
      {options.map((option) => (
        <option key={String(option.value)} value={String(option.value)} disabled={option.disabled}>
          {option.label}
        </option>
      ))}
    </select>
  );
  if (!field && !label) return select;
  return <FieldShell field={field} label={label} help={help} rules={rules}>{select}</FieldShell>;
}

export function Toggle({ field, label, help, value, checked, onChange, disabled, className, initValue, ...props }) {
  const form = useContext(FormContext);
  useEffect(() => {
    if (!form || !field || initValue === undefined) return;
    if (form.values[field] !== undefined) return;
    form.setValues((values) => ({ ...values, [field]: initValue }));
  }, [field, form, initValue]);
  const current = checked !== undefined ? checked : value !== undefined ? value : !!form?.values?.[field];
  const setCurrent = (next) => {
    onChange?.(next);
    if (form && field) form.setValues((values) => ({ ...values, [field]: next }));
  };
  const control = (
    <SwitchPrimitive.Root
      className={cx('pool-switch', className)}
      checked={!!current}
      disabled={disabled}
      onCheckedChange={setCurrent}
      {...props}
    >
      <SwitchPrimitive.Thumb className="pool-switch__thumb" />
    </SwitchPrimitive.Root>
  );
  if (!field && !label) return control;
  return <FieldShell field={field} label={label} help={help}>{control}</FieldShell>;
}

export function Form({
  children,
  initValues,
  onSubmit,
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

  useEffect(() => {
    setValues(getInitialValues(initValues));
    setErrors({});
  }, [initValues]);

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
      if (Object.keys(nextErrors).length === 0) onSubmit?.({ ...values });
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
    errors,
    setErrors,
    labelPosition,
    register: (field, rules) => {
      if (field) fieldsRef.current.set(field, rules || []);
    },
  }), [errors, labelPosition, values]);

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
