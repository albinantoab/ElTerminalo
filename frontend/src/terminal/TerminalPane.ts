import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebglAddon } from '@xterm/addon-webgl';
import '@xterm/xterm/css/xterm.css';
import '../types/wails.d.ts';

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

export class TerminalPane {
  public sessionId: string = '';
  public terminal: Terminal;
  private fitAddon: FitAddon;
  private container: HTMLElement;
  private resizeObserver: ResizeObserver;
  private eventCleanup: (() => void) | null = null;
  private resizeTimer: ReturnType<typeof setTimeout> | null = null;
  private lastCols: number = 0;
  private lastRows: number = 0;

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
    });

    this.fitAddon = new FitAddon();
    this.terminal.loadAddon(this.fitAddon);

    this.terminal.open(container);

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
    window.runtime.EventsOn('pty:exit:' + this.sessionId, () => {
      this.terminal.write('\r\n[Process exited]\r\n');
    });

    // Forward keyboard input to PTY
    this.terminal.onData((data: string) => {
      const encoded = btoa(data);
      window.go.main.App.WriteToSession(this.sessionId, encoded);
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
    this.resizeTimer = setTimeout(() => {
      window.go.main.App.ResizeSession(this.sessionId, cols, rows);
      // Send Ctrl+L (clear/redraw) to clean up stale prompt lines
      const ctrlL = btoa('\x0c');
      window.go.main.App.WriteToSession(this.sessionId, ctrlL);
      this.resizeTimer = null;
    }, 100);
  }

  async getCWD(): Promise<string> {
    if (!this.sessionId) return '';
    return await window.go.main.App.GetSessionCWD(this.sessionId);
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
    this.terminal.dispose();
  }
}
