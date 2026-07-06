import React from 'react';
import { Button } from './pool/index.jsx';
import { IconInbox, IconPlus, IconRefresh, IconFile, IconKey, IconSetting } from './pool/icons.jsx';

// Consistent empty state with icon + hint + optional CTA, used as Table `empty`.
export default function EmptyState({
  title = '暂无数据',
  desc,
  action,
  type = 'default',  // 'default' | 'accounts' | 'keys' | 'settings' | 'custom'
  icon: customIcon
}) {
  // Use appropriate icon based on type, or custom icon
  const getIcon = () => {
    if (customIcon) return customIcon;
    switch (type) {
      case 'accounts': return <IconPlus style={{ fontSize: 32 }} />;
      case 'keys': return <IconKey style={{ fontSize: 32 }} />;
      case 'settings': return <IconSetting style={{ fontSize: 32 }} />;
      case 'refresh': return <IconRefresh style={{ fontSize: 32 }} />;
      default: return <IconInbox style={{ fontSize: 32 }} />;
    }
  };

  // Default descriptions for common types
  const defaultDescs = {
    accounts: '导入 auth.json 或开启自动注册来填充账号池',
    keys: '创建 API Key 以便外部应用访问',
    groups: '创建分组来组织和管理账号',
    egress: '配置出口节点以启用多出口策略',
    usage: '等待使用数据生成',
    quota: '配置配额策略以限制资源使用',
    users: '添加用户以授权其访问系统',
    settings: '当前分类下暂无配置项',
  };

  const displayDesc = desc || defaultDescs[type] || null;

  return (
    <div className="pool-empty pool-empty--panel">
      <div className="pool-empty-icon">
        {getIcon()}
      </div>
      <div className="pool-empty-title">
        {title}
      </div>
      {displayDesc ? (
        <div className="pool-empty-desc">
          {displayDesc}
        </div>
      ) : null}
      {action ? (
        <div style={{ marginTop: 8 }}>{action}</div>
      ) : (
        type === 'accounts' && !action && (
          <div style={{ marginTop: 8 }}>
            <Button theme="solid" icon={<IconPlus />}>导入 auth.json</Button>
          </div>
        )
      )}
    </div>
  );
}
