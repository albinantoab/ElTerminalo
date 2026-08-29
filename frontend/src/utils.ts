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

// `Uint8Array.fromBase64` is a WebKit-shipped proposal that TypeScript's ES2020
// lib doesn't know about yet. Reached through the constructor so `this` is the
// intrinsic it expects.
type Base64Decoder = { fromBase64?(b64: string): Uint8Array };
const U8 = Uint8Array as Uint8ArrayConstructor & Base64Decoder;

/** Decode base64 into raw bytes.
 *
 *  This is the hot path: every chunk the shell prints comes through here, so it
 *  writes into a preallocated array rather than paying `Uint8Array.from`'s
 *  closure call per byte. The native decoder is used where the webview has it. */
export function base64ToBytes(b64: string): Uint8Array {
  if (U8.fromBase64) return U8.fromBase64(b64);
  const binary = atob(b64);
  const len = binary.length;
  const bytes = new Uint8Array(len);
  for (let i = 0; i < len; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}
