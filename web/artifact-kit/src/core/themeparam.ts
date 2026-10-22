// Pure parser for the ?theme=light|dark parameter the wiki puts on the frame URL.
export function parseThemeParam(search: string): 'light' | 'dark' | null {
  const v = new URLSearchParams(search).get('theme');
  return v === 'light' || v === 'dark' ? v : null;
}
