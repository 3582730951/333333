import React from 'react';

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

function NavItem({ item, selected, collapsed, onClick }) {
  const groupLabelId = React.useId();
  if (item.items?.length) {
    const label = <>{item.icon}<span className="pool-nav-text">{item.text}</span></>;
    return (
      <section className="pool-nav-section" aria-labelledby={groupLabelId}>
        {collapsed ? (
          <button id={groupLabelId} type="button" className="pool-nav-group-label" title={item.text} onClick={() => onClick?.({ itemKey: item.itemKey, group: true })}>
            {label}
          </button>
        ) : <h2 id={groupLabelId} className="pool-nav-group-label pool-nav-group-label--static">{label}</h2>}
        <div className="pool-nav-children" role="group" aria-labelledby={groupLabelId}>
          {item.items.map((child) => (
            <NavItem key={child.itemKey} item={child} selected={selected} collapsed={collapsed} onClick={onClick} />
          ))}
        </div>
      </section>
    );
  }
  const current = selected.includes(item.itemKey);
  return (
    <button
      type="button"
      className="pool-nav-item"
      aria-current={current ? 'page' : undefined}
      title={collapsed ? item.text : undefined}
      onClick={() => onClick?.({ itemKey: item.itemKey })}
    >
      {item.icon}
      <span className="pool-nav-text">{item.text}</span>
    </button>
  );
}

export function Nav({ items = [], selectedKeys = [], isCollapsed, onClick, className, style }) {
  return (
    <nav className={cx('pool-nav', className)} style={style}>
      {items.map((item) => <NavItem key={item.itemKey} item={item} selected={selectedKeys} collapsed={isCollapsed} onClick={onClick} />)}
    </nav>
  );
}
