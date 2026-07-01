import React from 'react';
import ConfigForm from '../components/ConfigForm.jsx';

export default function Moderation() {
  return <ConfigForm title="内容合规" subtitle="敏感词与历史合规配置" url="/admin/moderation" />;
}
