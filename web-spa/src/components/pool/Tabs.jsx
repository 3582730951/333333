import React from 'react';
import * as TabsPrimitive from '@radix-ui/react-tabs';

function cx(...parts) {
  return parts.filter(Boolean).join(' ');
}

/**
 * @param {{ itemKey: string, tab: React.ReactNode, children?: React.ReactNode }} _props
 */
export function TabPane(_props) {
  return null;
}

/**
 * @param {{
 *   children: React.ReactNode,
 *   activeKey?: string,
 *   defaultActiveKey?: string,
 *   onChange?: (value: string) => void,
 *   tabPosition?: 'left' | 'top',
 *   className?: string,
 *   style?: React.CSSProperties,
 *   keepMounted?: boolean,
 * }} props
 */
export function Tabs({ children, activeKey, defaultActiveKey, onChange, tabPosition, className, style, keepMounted = false }) {
  const panes = React.Children.toArray(children).filter(Boolean);
  const first = panes[0]?.props?.itemKey;
  const value = activeKey ?? defaultActiveKey ?? first;
  return (
    <TabsPrimitive.Root
      value={value}
      defaultValue={defaultActiveKey ?? first}
      onValueChange={onChange}
      className={cx('pool-tabs', tabPosition === 'left' ? 'pool-tabs--left' : '', className)}
      style={style}
    >
      <TabsPrimitive.List className="pool-tabs-list">
        {panes.map((pane) => (
          <TabsPrimitive.Trigger key={pane.props.itemKey} value={pane.props.itemKey} className="pool-tab-trigger">
            {pane.props.tab}
          </TabsPrimitive.Trigger>
        ))}
      </TabsPrimitive.List>
      <div className="pool-tabs-panels">
        {panes.map((pane) => (
          <TabsPrimitive.Content key={pane.props.itemKey} value={pane.props.itemKey} forceMount={keepMounted || undefined}>
            {pane.props.children}
          </TabsPrimitive.Content>
        ))}
      </div>
    </TabsPrimitive.Root>
  );
}
