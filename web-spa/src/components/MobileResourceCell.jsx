import React from 'react';
import { Typography } from '@douyinfe/semi-ui';

function hasRenderableValue(value) {
  return value !== null && value !== undefined && value !== '';
}

export default function MobileResourceCell({
  avatar,
  title,
  subtitle,
  badges,
  details = [],
  actions,
}) {
  const visibleDetails = details.filter((item) => hasRenderableValue(item?.value));
  return (
    <div className="pool-mobile-row">
      {avatar ? <div className="pool-mobile-row__avatar">{avatar}</div> : null}
      <div className="pool-mobile-row__main">
        <div className="pool-mobile-row__head">
          <div className="pool-mobile-row__title">{title || '-'}</div>
          {badges ? <div className="pool-mobile-row__badges">{badges}</div> : null}
        </div>
        {subtitle ? (
          <Typography.Text type="tertiary" size="small" className="pool-mobile-row__subtitle">
            {subtitle}
          </Typography.Text>
        ) : null}
        {visibleDetails.length > 0 ? (
          <div className="pool-mobile-row__details">
            {visibleDetails.map((item) => (
              <div className="pool-mobile-row__detail" key={item.label}>
                <span className="pool-mobile-row__label">{item.label}</span>
                <div className="pool-mobile-row__value">{item.value}</div>
              </div>
            ))}
          </div>
        ) : null}
      </div>
      {actions ? <div className="pool-mobile-row__actions">{actions}</div> : null}
    </div>
  );
}
