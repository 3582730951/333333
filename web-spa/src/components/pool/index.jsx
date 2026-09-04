import React from 'react';
import { getLocalItem, setLocalItem } from '../../lib/browserStorage.js';
import { Tooltip as PoolTooltip } from './Feedback.jsx';

export { Button, IconButton } from './Button.jsx';
export { Card, DataCard, MetricCard } from './Card.jsx';
export { ActionMenu } from './ActionMenu.jsx';
export { DataTable, Table } from './DataTable.jsx';
export { Modal, Drawer, ConfirmDialog } from './Dialog.jsx';
export { EmptyState, LoadingState, ErrorState } from './EmptyState.jsx';
export {
  Avatar,
  Banner,
  Divider,
  LocaleProvider,
  Space,
  Spin,
  Tag,
  Toast,
  ToastViewport,
  Tooltip,
  Typography,
} from './Feedback.jsx';
export {
  Form,
  Input,
  InputNumber,
  Select,
  SelectInput,
  Switch,
  Textarea,
  TextInput,
  Toggle,
} from './Form.jsx';
export { Progress, ProgressBar, StatusDot, StatusPill } from './Progress.jsx';
export { Tabs, TabPane } from './Tabs.jsx';

function cx(...parts) {
  return parts.filter(Boolean).join(' ');
}

function LayoutRoot({ children, className, ...props }) {
  return <div className={cx('pool-layout', className)} {...props}>{children}</div>;
}

function Header({ children, className, ...props }) {
  return <header className={cx('pool-header', className)} {...props}>{children}</header>;
}

function Sider({ children, className, ...props }) {
  return <aside className={cx('pool-sider', className)} {...props}>{children}</aside>;
}

function Content({ children, className, ...props }) {
  return <main className={cx('pool-content', className)} {...props}>{children}</main>;
}

export const Layout = Object.assign(LayoutRoot, { Header, Sider, Content });

function groupStorageKey(storageScope, itemKey) {
  return 'pool-nav-group:' + storageScope + ':' + itemKey;
}

function groupEntries(items) {
  return items.flatMap((item) => (item.items?.length ? [item, ...groupEntries(item.items)] : []));
}

// Storage goes through lib/browserStorage.js: it owns the SSR guard, the
// privacy-mode try/catch and an in-memory fallback, and check:runtime requires
// every storage touch to route through it. An unset group reads as '' and must
// default to expanded, so only an explicit 'false' collapses a group.
function readGroupStates(items, storageScope) {
  const states = {};
  for (const item of groupEntries(items)) {
    const stored = getLocalItem(groupStorageKey(storageScope, item.itemKey));
    states[item.itemKey] = stored === '' ? true : stored === 'true';
  }
  return states;
}

function persistGroupState(storageScope, itemKey, expanded) {
  setLocalItem(groupStorageKey(storageScope, itemKey), String(expanded));
}

function NavItem({
  item, selected, collapsed, onClick, onIntent, getGroupExpanded, onGroupToggle,
}) {
  const groupLabelId = React.useId();
  const groupChildrenId = React.useId();
  if (item.items?.length) {
    const expanded = getGroupExpanded?.(item.itemKey) ?? true;
    const visible = !collapsed && expanded;
    const label = <>{item.icon}<span className="pool-nav-text">{item.text}</span></>;
    const control = (
      <button
        id={groupLabelId}
        type="button"
        className="pool-nav-group-label"
        aria-label={collapsed ? item.text : undefined}
        aria-controls={groupChildrenId}
        aria-expanded={visible}
        data-state={visible ? 'open' : 'closed'}
        onClick={() => {
          onGroupToggle?.(item.itemKey, collapsed ? true : !expanded);
          onClick?.({ itemKey: item.itemKey, group: true });
        }}
      >
        {label}
      </button>
    );
    return (
      <section className="pool-nav-section" aria-labelledby={groupLabelId} data-nav-group={item.itemKey}>
        {collapsed ? <PoolTooltip content={item.text}>{control}</PoolTooltip> : control}
        <div id={groupChildrenId} className="pool-nav-children" role="group" aria-labelledby={groupLabelId} data-state={visible ? 'open' : 'closed'} hidden={!visible}>
          {item.items.map((child) => (
            <NavItem
              key={child.itemKey}
              item={child}
              selected={selected}
              collapsed={collapsed}
              onClick={onClick}
              onIntent={onIntent}
              getGroupExpanded={getGroupExpanded}
              onGroupToggle={onGroupToggle}
            />
          ))}
        </div>
      </section>
    );
  }
  const current = selected.includes(item.itemKey);
  const button = (
    <button
      type="button"
      className="pool-nav-item"
      aria-current={current ? 'page' : undefined}
      aria-label={collapsed ? item.text : undefined}
      onClick={() => onClick?.({ itemKey: item.itemKey })}
      onPointerEnter={() => onIntent?.({ itemKey: item.itemKey })}
      onFocus={() => onIntent?.({ itemKey: item.itemKey })}
      onTouchStart={() => onIntent?.({ itemKey: item.itemKey })}
    >
      {item.icon}
      <span className="pool-nav-text">{item.text}</span>
    </button>
  );
  return collapsed ? <PoolTooltip content={item.text}>{button}</PoolTooltip> : button;
}

export function Nav({
  items = [], selectedKeys = [], isCollapsed, onClick, onIntent, className, style, storageScope = 'default',
}) {
  const [groupStates, setGroupStates] = React.useState(() => readGroupStates(items, storageScope));
  const getGroupExpanded = (itemKey) => groupStates[itemKey] !== false;
  const onGroupToggle = (itemKey, expanded) => {
    setGroupStates((states) => ({ ...states, [itemKey]: expanded }));
    persistGroupState(storageScope, itemKey, expanded);
  };
  return (
    <nav className={cx('pool-nav', className)} style={style}>
      {items.map((item) => (
        <NavItem
          key={item.itemKey}
          item={item}
          selected={selectedKeys}
          collapsed={isCollapsed}
          onClick={onClick}
          onIntent={onIntent}
          getGroupExpanded={getGroupExpanded}
          onGroupToggle={onGroupToggle}
        />
      ))}
    </nav>
  );
}
