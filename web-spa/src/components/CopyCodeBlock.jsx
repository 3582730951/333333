import React, { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { Button, Toast } from './pool/index.jsx';
import { IconCopy } from './pool/icons.jsx';
import { selectTextForManualCopy, writeClipboard } from '../lib/browserClipboard.js';

// A fixed height in CSS has to guess how many lines the command will be, and every command
// longer than the guess gets clipped mid-glyph. The box measures its own content instead.
// The cap stops a pathological blob from pushing the copy button off-screen; past it the
// textarea scrolls, which is honest about there being more to see.
const MIN_HEIGHT = 64;
const MAX_HEIGHT = 340;

export default function CopyCodeBlock({ code, label = '复制命令', className = '' }) {
  const inputRef = useRef(null);
  const [copying, setCopying] = useState(false);
  const value = String(code ?? '');

  const fitToContent = useCallback(() => {
    const node = inputRef.current;
    if (!node) return;
    // scrollHeight only reports the content height once the box is smaller than it, so
    // the height has to be released before it can be read.
    node.style.height = 'auto';
    const fitted = Math.min(MAX_HEIGHT, Math.max(MIN_HEIGHT, node.scrollHeight));
    node.style.height = `${fitted}px`;
  }, []);

  useLayoutEffect(fitToContent, [fitToContent, value]);

  // The command wraps, so how tall it is depends on how wide the pane happens to be.
  // Only width is acted on: fitToContent changes the height, which would otherwise
  // re-trigger the observer that just ran it.
  useEffect(() => {
    const node = inputRef.current;
    if (!node || typeof ResizeObserver === 'undefined') return undefined;
    let lastWidth = node.clientWidth;
    const observer = new ResizeObserver(() => {
      const width = node.clientWidth;
      if (width === lastWidth) return;
      lastWidth = width;
      fitToContent();
    });
    observer.observe(node);
    return () => observer.disconnect();
  }, [fitToContent]);

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
