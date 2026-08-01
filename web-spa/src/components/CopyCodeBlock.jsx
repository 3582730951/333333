import React, { useRef, useState } from 'react';
import { Button, Toast } from './pool/index.jsx';
import { IconCopy } from './pool/icons.jsx';
import { selectTextForManualCopy, writeClipboard } from '../lib/browserClipboard.js';

export default function CopyCodeBlock({ code, label = '复制命令', className = '' }) {
  const inputRef = useRef(null);
  const [copying, setCopying] = useState(false);
  const value = String(code ?? '');

  const copy = async () => {
    setCopying(true);
    try {
      if (await writeClipboard(value)) {
        Toast.success('已复制到剪贴板');
        return;
      }
      if (selectTextForManualCopy(inputRef.current)) {
        Toast.warning('已选中命令，请按 Ctrl/Cmd+C');
      } else {
        Toast.error('复制失败，请手动选择命令');
      }
    } finally {
      setCopying(false);
    }
  };

  return (
    <div className={`pool-copy-code ${className}`.trim()}>
      <textarea ref={inputRef} readOnly spellCheck={false} value={value} aria-label={label} />
      <Button size="small" icon={<IconCopy />} loading={copying} onClick={copy}>{label}</Button>
    </div>
  );
}
