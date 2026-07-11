import React from 'react';
import ConfigForm from '../components/ConfigForm';

export default function Moderation() {
  return <ConfigForm title="内容合规" subtitle="敏感词与历史合规配置" kind="moderation" />;
}
