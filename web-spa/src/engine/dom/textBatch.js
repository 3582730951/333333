/**
 * Preallocates a small batch of actual DOM text nodes. These labels are not
 * rasterized into WebGL: assistive technology, selection, browser translation,
 * and normal font fallback all remain available to the DOM.
 */
export function createDomTextBatch(root, { capacity = 8, className = 'aurora-dom-text' } = {}) {
  if (!root || typeof document === 'undefined') throw new Error('a DOM root is required');
  const size = Math.max(1, Math.floor(Number(capacity) || 8));
  const nodes = new Array(size);
  for (let index = 0; index < size; index += 1) {
    const node = document.createElement('span');
    node.className = className;
    node.setAttribute('data-aurora-dom-text', 'true');
    node.style.position = 'absolute';
    node.style.display = 'none';
    node.style.userSelect = 'text';
    node.style.pointerEvents = 'auto';
    root.append(node);
    nodes[index] = node;
  }

  function set(index, { text, x = 0, y = 0, ariaLabel, visible = true } = {}) {
    const node = nodes[index];
    if (!node) return false;
    node.textContent = text == null ? '' : String(text);
    node.style.transform = `translate(${Number(x) || 0}px, ${Number(y) || 0}px)`;
    node.style.display = visible ? 'inline' : 'none';
    if (ariaLabel == null || ariaLabel === '') node.removeAttribute('aria-label');
    else node.setAttribute('aria-label', String(ariaLabel));
    return true;
  }

  function hide(index) {
    const node = nodes[index];
    if (!node) return false;
    node.style.display = 'none';
    return true;
  }

  function dispose() {
    for (let index = 0; index < nodes.length; index += 1) nodes[index].remove();
  }

  return { set, hide, dispose, get capacity() { return size; } };
}
