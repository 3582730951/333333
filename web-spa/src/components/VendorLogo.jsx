import React from 'react';
import { IconGlobe } from '@douyinfe/semi-icons';
import openaiLogo from '../assets/vendors/openai-blossom.svg';
import anthropicLogo from '../assets/vendors/anthropic.svg';
import paypalLogo from '../assets/vendors/paypal.svg';

const vendorMap = {
  'openai': { label: 'OpenAI', asset: openaiLogo, tone: 'neutral' },
  'chatgpt': { label: 'OpenAI', asset: openaiLogo, tone: 'neutral' },
  'codex': { label: 'OpenAI', asset: openaiLogo, tone: 'neutral' },
  'claude': { label: 'Claude', asset: anthropicLogo, tone: 'claude' },
  'anthropic': { label: 'Anthropic', asset: anthropicLogo, tone: 'claude' },
  'paypal': { label: 'PayPal', asset: paypalLogo, tone: 'paypal' },
};

function normalizeVendor(vendor) {
  return String(vendor || 'custom').trim().toLowerCase();
}

export function vendorLogoInfo(vendor) {
  return vendorMap[normalizeVendor(vendor)] || null;
}

export default function VendorLogo({
  vendor = 'custom',
  label,
  size = 32,
  showLabel = false,
  className = '',
}) {
  const info = vendorLogoInfo(vendor);
  const resolvedLabel = label || info?.label || 'Custom provider';
  const style = { '--vendor-logo-size': `${size}px` };
  const classes = [
    'pool-vendor-logo',
    info ? `pool-vendor-logo--${info.tone}` : 'pool-vendor-logo--custom',
    showLabel ? 'pool-vendor-logo--with-label' : '',
    className,
  ].filter(Boolean).join(' ');

  return (
    <span className={classes} style={style} title={resolvedLabel} aria-label={resolvedLabel}>
      <span className="pool-vendor-logo__mark">
        {info ? (
          <img src={info.asset} alt="" aria-hidden="true" />
        ) : (
          <IconGlobe aria-hidden="true" />
        )}
      </span>
      {showLabel ? <span className="pool-vendor-logo__label">{resolvedLabel}</span> : null}
    </span>
  );
}
