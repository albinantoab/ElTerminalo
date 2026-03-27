import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebglAddon } from '@xterm/addon-webgl';
import '@xterm/xterm/css/xterm.css';
import '../types/wails.d.ts';
import { utf8ToBase64 } from '../utils';
import { CMD } from '../constants';

export interface XtermTheme {
  background: string;
  foreground: string;
  cursor: string;
  selectionBackground: string;
  black: string;
  red: string;
  green: string;
  yellow: string;
  blue: string;
  magenta: string;
  cyan: string;
  white: string;
  brightBlack: string;
  brightRed: string;
  brightGreen: string;
  brightYellow: string;
  brightBlue: string;
  brightMagenta: string;
  brightCyan: string;
  brightWhite: string;
}

export interface TerminalContextActions {
  splitVertical: () => void;
  splitHorizontal: () => void;
  closePane: () => void;
}

export class TerminalPane {
  public sessionId: string = '';
  public terminal: Terminal;
  private fitAddon: FitAddon;
  private container: HTMLElement;
  private resizeObserver: ResizeObserver;
  private eventCleanup: (() => void) | null = null;
  private exitEventCleanup: (() => void) | null = null;
  private resizeTimer: ReturnType<typeof setTimeout> | null = null;
  private lastCols: number = 0;
  private lastRows: number = 0;
  private ctxActions: TerminalContextActions | null = null;

  constructor(container: HTMLElement, theme: XtermTheme) {
    this.container = container;

    this.terminal = new Terminal({
      cursorBlink: true,
      cursorStyle: 'block',
      fontFamily: "'MonaspiceNe NFM', 'SF Mono', 'Menlo', monospace",
      fontSize: 12,
      lineHeight: 1.2,
      theme: theme,
      allowProposedApi: true,
      rightClickSelectsWord: true,
    });

    this.fitAddon = new FitAddon();
    this.terminal.loadAddon(this.fitAddon);

    this.terminal.open(container);

    // Block Ctrl+L from reaching the shell — only Cmd+L should clear
    // Send CSI u sequence for Shift+Enter so apps like Claude CLI can
    // distinguish it from plain Enter (matches iTerm / Kitty behaviour)
    this.terminal.attachCustomKeyEventHandler((e: KeyboardEvent) => {
      if (e.type === 'keydown' && e.ctrlKey && !e.metaKey && e.key.toLowerCase() === 'l') {
        return false;
      }
      if (e.type === 'keydown' && e.shiftKey && e.key === 'Enter') {
        e.preventDefault();
        window.go.main.App.WriteToSession(this.sessionId, utf8ToBase64('\x1b[13;2u'));
        return false;
      }
      return true;
    });

    // Custom context menu — xterm renders on canvas so Wails'
    // default context menu can't see terminal selections.
    this.initContextMenu(container);

    // Try WebGL renderer for GPU acceleration
    try {
      const webglAddon = new WebglAddon();
      this.terminal.loadAddon(webglAddon);
    } catch (e) {
      console.warn('WebGL addon failed, using canvas renderer');
    }

    this.fitAddon.fit();

    // Watch for container resize — fit immediately, debounce PTY notify
    this.resizeObserver = new ResizeObserver(() => {
      this.fit();
    });
    this.resizeObserver.observe(container);
  }

  async connect(cwd: string = ''): Promise<void> {
    const cols = this.terminal.cols;
    const rows = this.terminal.rows;

    // Create PTY session via Go backend
    this.sessionId = await window.go.main.App.CreateSession(cols, rows, cwd);

    // Subscribe to PTY output
    const eventName = 'pty:output:' + this.sessionId;
    this.eventCleanup = window.runtime.EventsOn(eventName, (data: string) => {
      // Decode base64 and write to terminal
      const bytes = Uint8Array.from(atob(data), c => c.charCodeAt(0));
      this.terminal.write(bytes);
    });

    // Subscribe to PTY exit
    this.exitEventCleanup = window.runtime.EventsOn('pty:exit:' + this.sessionId, () => {
      this.terminal.write('\r\n[Process exited]\r\n');
    });

    // Forward keyboard input to PTY
    this.terminal.onData((data: string) => {
      window.go.main.App.WriteToSession(this.sessionId, utf8ToBase64(data));
    });

    // Forward resize events to PTY — debounced to avoid shell prompt spam
    this.terminal.onResize(({ cols, rows }: { cols: number; rows: number }) => {
      if (cols !== this.lastCols || rows !== this.lastRows) {
        this.lastCols = cols;
        this.lastRows = rows;
        this.debouncedPtyResize(cols, rows);
      }
    });

    this.lastCols = this.terminal.cols;
    this.lastRows = this.terminal.rows;
  }

  fit(): void {
    try {
      this.fitAddon.fit();
    } catch (e) {
      // ignore fit errors during rapid resizing
    }
  }

  private debouncedPtyResize(cols: number, rows: number): void {
    if (this.resizeTimer) {
      clearTimeout(this.resizeTimer);
    }
    this.resizeTimer = setTimeout(async () => {
      window.go.main.App.ResizeSession(this.sessionId, cols, rows);
      // Clear stale prompt artifacts after resize — only when the shell is idle.
      // If a process is running (yarn dev, etc.), SIGWINCH from the PTY resize
      // is enough. Sending \x0c to a running process prints ^L.
      if (this.terminal.buffer.active.type === 'normal') {
        try {
          const statuses = await window.go.main.App.GetAllSessionStatuses();
          const status = statuses[this.sessionId];
          if (status?.isIdle) {
            window.go.main.App.WriteToSession(this.sessionId, utf8ToBase64('\x0c'));
          }
        } catch { /* skip clear on error */ }
      }
      this.resizeTimer = null;
    }, 150);
  }

  async getCWD(): Promise<string> {
    if (!this.sessionId) return '';
    return await window.go.main.App.GetSessionCWD(this.sessionId);
  }

  private initContextMenu(container: HTMLElement): void {
    let menu: HTMLElement | null = null;

    const dismiss = () => {
      if (menu) { menu.remove(); menu = null; }
    };

    container.addEventListener('contextmenu', (e) => {
      e.preventDefault();
      e.stopPropagation();
      dismiss();

      const hasSelection = this.terminal.hasSelection();

      menu = document.createElement('div');
      menu.className = 'term-ctx-menu';
      menu.style.left = `${e.clientX}px`;
      menu.style.top = `${e.clientY}px`;

      type MenuItem = { type: 'action'; label: string; shortcut?: string; action: () => void; enabled: boolean } | { type: 'separator' };

      const items: MenuItem[] = [
        {
          type: 'action', label: CMD.COPY.name, shortcut: CMD.COPY.shortcut,
          enabled: hasSelection,
          action: () => {
            const text = this.terminal.getSelection();
            navigator.clipboard.writeText(text);
            this.terminal.clearSelection();
          },
        },
        {
          type: 'action', label: CMD.PASTE.name, shortcut: CMD.PASTE.shortcut,
          enabled: true,
          action: async () => {
            const text = await navigator.clipboard.readText();
            window.go.main.App.WriteToSession(this.sessionId, utf8ToBase64(text));
          },
        },
        { type: 'separator' },
        {
          type: 'action', label: CMD.SELECT_ALL.name, shortcut: CMD.SELECT_ALL.shortcut,
          enabled: true,
          action: () => this.terminal.selectAll(),
        },
        {
          type: 'action', label: 'Clear',
          enabled: true,
          action: () => this.terminal.clear(),
        },
        { type: 'separator' },
        {
          type: 'action', label: CMD.SPLIT_VERTICAL.name, shortcut: CMD.SPLIT_VERTICAL.shortcut,
          enabled: !!this.ctxActions,
          action: () => this.ctxActions?.splitVertical(),
        },
        {
          type: 'action', label: CMD.SPLIT_HORIZONTAL.name, shortcut: CMD.SPLIT_HORIZONTAL.shortcut,
          enabled: !!this.ctxActions,
          action: () => this.ctxActions?.splitHorizontal(),
        },
        {
          type: 'action', label: CMD.CLOSE_PANE.name, shortcut: CMD.CLOSE_PANE.shortcut,
          enabled: !!this.ctxActions,
          action: () => this.ctxActions?.closePane(),
        },
      ];

      for (const item of items) {
        if (item.type === 'separator') {
          const sep = document.createElement('div');
          sep.className = 'term-ctx-separator';
          menu.appendChild(sep);
          continue;
        }
        const el = document.createElement('div');
        el.className = 'term-ctx-item' + (item.enabled ? '' : ' disabled');
        const labelSpan = document.createElement('span');
        labelSpan.textContent = item.label;
        el.appendChild(labelSpan);
        if (item.shortcut) {
          const shortcutSpan = document.createElement('span');
          shortcutSpan.className = 'term-ctx-shortcut';
          shortcutSpan.textContent = item.shortcut;
          el.appendChild(shortcutSpan);
        }
        if (item.enabled) {
          el.addEventListener('mousedown', (ev) => {
            ev.stopPropagation();
            item.action();
            dismiss();
          });
        }
        menu.appendChild(el);
      }

      document.body.appendChild(menu);

      // Keep menu within viewport
      requestAnimationFrame(() => {
        if (!menu) return;
        const r = menu.getBoundingClientRect();
        if (r.right > window.innerWidth) menu.style.left = `${window.innerWidth - r.width - 4}px`;
        if (r.bottom > window.innerHeight) menu.style.top = `${window.innerHeight - r.height - 4}px`;
      });
    }, true);

    // Dismiss on any click or keydown
    document.addEventListener('mousedown', dismiss);
    document.addEventListener('keydown', dismiss);
  }

  setContextActions(actions: TerminalContextActions): void {
    this.ctxActions = actions;
  }

  /** Return the text on the current cursor line(s), stripped of the shell prompt.
   *  Handles soft-wrapped lines and shell continuation lines (ending with \). */
  getCurrentInput(): string {
    const buf = this.terminal.buffer.active;
    const cursorAbsY = buf.cursorY + buf.baseY;

    // Strip right-side prompt decorations (box-drawing chars, arrows, etc.)
    // that shells like starship/p10k render after the command text
    const stripRight = (s: string) =>
      s.replace(/[\s─│╮╯╰╭┌┐└┘├┤┬┴┼═║╔╗╚╝╠╣╦╩╬➤❯→←↑↓›»▸▶❱⮞⟩]+$/, '');

    // Walk backwards to find the start of the input:
    // through soft-wrapped lines (isWrapped) and continuation lines (prev ends with \)
    let startY = cursorAbsY;
    while (startY > 0) {
      const line = buf.getLine(startY);
      if (!line) break;
      if (line.isWrapped) {
        startY--;
        continue;
      }
      const above = buf.getLine(startY - 1);
      if (above && stripRight(above.translateToString(true)).endsWith('\\')) {
        startY--;
        continue;
      }
      break;
    }

    // Strip the shell prompt from the first non-wrapped line
    const firstLine = buf.getLine(startY);
    if (!firstLine) return '';
    const firstText = stripRight(firstLine.translateToString(true));
    // Strip common shell prompts — covers $, %, #, > and unicode arrows
    // used by modern themes (starship, p10k, oh-my-zsh, etc.)
    const m = firstText.match(/^.*?[\$%#>➤❯→›»▸▶❱⮞⟩]\s?(.*)$/);
    const firstInput = (m ? m[1] : firstText).trimEnd();

    // Collect lines from startY to cursorAbsY, merging soft-wrapped lines
    const lines: string[] = [];
    let current = firstInput;
    for (let y = startY + 1; y <= cursorAbsY; y++) {
      const line = buf.getLine(y);
      if (!line) continue;
      const text = stripRight(line.translateToString(true));
      if (line.isWrapped) {
        current += text;
      } else {
        lines.push(current);
        current = text;
      }
    }
    lines.push(current);

    // Walk forward past cursor to collect remaining continuation lines
    if (lines[lines.length - 1].endsWith('\\')) {
      let y = cursorAbsY + 1;
      while (y < buf.length) {
        const line = buf.getLine(y);
        if (!line) break;
        const text = stripRight(line.translateToString(true));
        if (!text) break;
        if (line.isWrapped) {
          lines[lines.length - 1] += text;
        } else {
          lines.push(text);
        }
        if (!lines[lines.length - 1].endsWith('\\')) break;
        y++;
      }
    }

    return lines.join('\n').trim();
  }

  focus(): void {
    this.terminal.focus();
  }

  setTheme(theme: XtermTheme): void {
    this.terminal.options.theme = theme;
  }

  dispose(): void {
    if (this.sessionId) {
      window.go.main.App.CloseSession(this.sessionId);
    }
    this.resizeObserver.disconnect();
    if (this.eventCleanup) {
      this.eventCleanup();
    }
    if (this.exitEventCleanup) {
      this.exitEventCleanup();
    }
    this.terminal.dispose();
  }
}
