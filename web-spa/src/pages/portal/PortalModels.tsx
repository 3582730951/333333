import React from 'react';
import ModelNameList from '../../components/ModelNameList';

export default function PortalModels() {
  return <ModelNameList endpoint="/user/models" title="可用模型" subtitle="你的启用推理 Key 所属分组可访问的模型名称" />;
}
