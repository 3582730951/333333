export function getCookie(name) {
  const cookieName = String(name || '');
  if (!cookieName || typeof document === 'undefined') return '';

  let raw = '';
  try {
    raw = document.cookie || '';
  } catch {
    return '';
  }

  const prefix = `${cookieName}=`;
  const match = raw
    .split(';')
    .map((part) => part.trimStart())
    .find((part) => part.startsWith(prefix));
  if (!match) return '';

  const value = match.slice(prefix.length);
  try {
    return decodeURIComponent(value);
  } catch {
    return value;
  }
}
