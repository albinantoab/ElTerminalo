export function escHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

export function generateId(prefix: string): string {
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
}

export function waitForLayout(): Promise<void> {
  return new Promise<void>(r => requestAnimationFrame(() => requestAnimationFrame(() => r())));
}

/** Base64-encode a UTF-8 string. Unlike btoa(), handles non-ASCII characters. */
export function utf8ToBase64(str: string): string {
  const bytes = new TextEncoder().encode(str);
  let binary = '';
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]);
  return btoa(binary);
}

/** Strip ANSI escape codes (CSI, OSC, SGR) from a string to get plain text. */
export function stripAnsi(text: string): string {
  return text
    .replace(/\x1b\[[0-9;]*[a-zA-Z]/g, '')   // CSI sequences (colors, cursor, etc.)
    .replace(/\x1b\][^\x07]*\x07/g, '')       // OSC sequences (terminated by BEL)
    .replace(/\x1b\][^\x1b]*\x1b\\/g, '')     // OSC sequences (terminated by ST)
    .replace(/\x1b\([A-B]/g, '')              // Character set designators
    .replace(/\x1b[=>]/g, '');                // Keypad modes
}

/** Base64-encode raw bytes. */
export function bytesToBase64(bytes: Uint8Array): string {
  let binary = '';
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]);
  return btoa(binary);
}
