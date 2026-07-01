let cachedDocument = null;
let cachedCurrentSignature = '';

function readAssetSignature(doc) {
  const scripts = Array.from(doc.querySelectorAll('script[type="module"][src]')).map((node) => node.getAttribute('src'));
  const styles = Array.from(doc.querySelectorAll('link[rel="stylesheet"][href]')).map((node) => node.getAttribute('href'));
  return [...scripts, ...styles].filter(Boolean).sort().join('|');
}

export function currentAssetSignature(doc = document) {
  if (typeof document !== 'undefined' && doc === document) {
    if (cachedDocument !== document || !cachedCurrentSignature) {
      cachedDocument = document;
      cachedCurrentSignature = readAssetSignature(doc);
    }
    return cachedCurrentSignature;
  }
  return readAssetSignature(doc);
}

export function invalidateCurrentAssetSignature() {
  cachedDocument = null;
  cachedCurrentSignature = '';
}

export function assetSignatureFromHTML(html) {
  const doc = new DOMParser().parseFromString(html, 'text/html');
  return readAssetSignature(doc);
}
