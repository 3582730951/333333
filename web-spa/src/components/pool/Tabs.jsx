import React from 'react';
import * as TabsPrimitive from '@radix-ui/react-tabs';

function cx(...parts) {
  return parts.filter(Boolean).join(' ');
}

export function TabPane() {
  return null;
}

export function Tabs({ children, activeKey, defaultActiveKey, onChange, tabPosition, className, style }) {
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
          <TabsPrimitive.Content key={pane.props.itemKey} value={pane.props.itemKey}>
            {pane.props.children}
          </TabsPrimitive.Content>
        ))}
      </div>
    </TabsPrimitive.Root>
  );
}
