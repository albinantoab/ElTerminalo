export function escHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

export function generateId(prefix: string): string {
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
}

export function waitForLayout(): Promise<void> {
  return new Promise<void>(r => requestAnimationFrame(() => requestAnimationFrame(() => r())));
}
