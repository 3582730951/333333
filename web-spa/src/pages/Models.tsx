import React from 'react';
import ModelNameList from '../components/ModelNameList';

export default function Models() {
  return <ModelNameList endpoint="/admin/models" title="模型列表" subtitle="当前账号池能力快照中的模型名称" />;
}
