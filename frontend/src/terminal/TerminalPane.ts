import { Terminal, IDisposable } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebglAddon } from '@xterm/addon-webgl';
import '@xterm/xterm/css/xterm.css';
import '../types/wails.d.ts';
import type { PtyExitPayload } from '../types/wails.d.ts';
import { utf8ToBase64 } from '../utils';
import { CMD } from '../constants';
import { ShellIntegration } from './ShellIntegration';
import { SmartRenderManager } from './smartrender/SmartRenderManager';

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

// Hostname of this machine, used to reject OSC 7 reports that originate from
// a remote shell (an SSH session whose far end also has shell integration).
// Set once at startup; until then only host-less reports are accepted.
let localHostname = '';

export function setLocalHostname(name: string): void {
  localHostname = name.trim().toLowerCase();
}

function isLocalHost(host: string): boolean {
  const h = host.toLowerCase();
  if (h === '' || h === 'localhost' || h === '127.0.0.1') return true;
  if (!localHostname) return false;
  if (h === localHostname) return true;
  // "albins-mac-mini" vs "albins-mac-mini.local": compare the first label.
  return h.split('.')[0] === localHostname.split('.')[0];
}

// Parses an OSC 7 payload ("file://<host>/<path>") into an absolute path, or
// returns '' when the report is for a different machine. The payload arrives
// already decoded from UTF-8, so only percent-escapes need undoing; the
// emitter escapes '%' and nothing else.
function parseOsc7Path(data: string): string {
  if (!data.startsWith('file://')) return '';
  const rest = data.slice('file://'.length);
  const slash = rest.indexOf('/');
  if (slash < 0) return '';
  if (!isLocalHost(rest.slice(0, slash))) return '';
  const raw = rest.slice(slash);
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

// Backend error strings and signal names are written straight into the
// terminal, where a stray control byte would be read as an escape sequence.
function sanitizeForTerminal(s: string): string {
  return s.replace(/[\x00-\x1f\x7f]/g, ' ');
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
  public shellIntegration: ShellIntegration;
  public smartRender: SmartRenderManager;
  // Last directory the shell reported (OSC 7), or the one this pane was opened
  // with. Never cleared once set: a momentary failure to read the cwd must not
  // be allowed to erase the pane's folder from the saved layout.
  private lastKnownCwd: string = '';
  private cwdReportedByShell = false;
  private cwdOscHandler: IDisposable | null = null;
  private disposed = false;
  private connecting = false;
  // Set while the pane has no shell and is showing a "Press Enter" hint, so a
  // stray Enter before the first connect can't spawn a second shell.
  private awaitingRestart = false;
  // Called when this pane's shell exits. The host owns the decision of whether
  // the pane survives; with no host listening the pane keeps itself and offers
  // a restart.
  public onExit: ((exit: PtyExitPayload) => void) | null = null;

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

    // Register OSC 133 handler for shell integration (command blocks)
    this.shellIntegration = new ShellIntegration(this.terminal);

    // OSC 7: the shell tells us its working directory on every prompt and on
    // every cd. This is authoritative and free — no process spawn, nothing that
    // can be denied or time out — so it is preferred over asking the backend.
    this.cwdOscHandler = this.terminal.parser.registerOscHandler(7, (data: string) => {
      const cwd = parseOsc7Path(data);
      if (cwd) {
        this.lastKnownCwd = cwd;
        this.cwdReportedByShell = true;
      }
      return true;
    });
    this.smartRender = new SmartRenderManager(this.terminal, container, this.shellIntegration);

    // Send CSI u sequence for Shift+Enter so apps like Claude CLI can
    // distinguish it from plain Enter (matches iTerm / Kitty behaviour).
    // Ctrl+L is deliberately left alone: vim, less, tmux and Claude Code all
    // bind it, and Cmd+L is this app's own clear.
    this.terminal.attachCustomKeyEventHandler((e: KeyboardEvent) => {
      if (e.type === 'keydown' && e.shiftKey && e.key === 'Enter') {
        e.preventDefault();
        this.sendInput('\x1b[13;2u');
        return false;
      }
      // CMD+Backspace: delete entire line (send Ctrl+U)
      if (e.type === 'keydown' && e.metaKey && e.key === 'Backspace') {
        e.preventDefault();
        this.sendInput('\x15');
        return false;
      }
      return true;
    });

    // Input plumbing is wired once, for the life of the pane: connect() runs
    // again whenever a dead shell is restarted, and re-subscribing there would
    // duplicate every keystroke and every resize.
    this.terminal.onData((data: string) => {
      if (!this.sessionId) {
        // Dead pane: Enter revives it, everything else is dropped so keystrokes
        // can't pile up as rejected WriteToSession promises.
        if (this.awaitingRestart && (data === '\r' || data === '\n')) {
          this.awaitingRestart = false;
          // Out of the key handler first: restart() resets the terminal, which
          // must not happen while xterm is still dispatching this data event.
          queueMicrotask(() => { void this.restart(); });
        }
        return;
      }
      this.sendInput(data);
    });

    // Forward resize events to PTY — debounced to avoid shell prompt spam
    this.terminal.onResize(({ cols, rows }: { cols: number; rows: number }) => {
      if (cols !== this.lastCols || rows !== this.lastRows) {
        this.lastCols = cols;
        this.lastRows = rows;
        this.debouncedPtyResize(cols, rows);
      }
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
    if (this.disposed) return;
    const cols = this.terminal.cols;
    const rows = this.terminal.rows;

    // Seed the cache with the directory this pane is being opened in, so the
    // layout can still be saved correctly before the first prompt has rendered
    // — and so a pane whose shell never starts still remembers where it lived.
    if (cwd) this.lastKnownCwd = cwd;

    // Create PTY session via Go backend
    this.connecting = true;
    let sid: string;
    try {
      sid = await window.go.main.App.CreateSession(cols, rows, cwd);
    } finally {
      this.connecting = false;
    }

    // The pane can be closed while CreateSession is in flight; the session it
    // just handed us would otherwise be orphaned.
    if (this.disposed) {
      // A binding rejects, it doesn't throw — a synchronous catch would miss it.
      window.go.main.App.CloseSession(sid).catch(() => { /* nothing to undo */ });
      return;
    }

    this.sessionId = sid;
    this.awaitingRestart = false;

    // Subscribe to PTY output
    this.eventCleanup = window.runtime.EventsOn('pty:output:' + sid, (data: string) => {
      // Same guard as the exit listener, and for the same reason now that
      // unsubscribing is deferred by a microtask: a retired session's bytes
      // don't belong in a restarted shell's screen, and a disposed terminal
      // would throw on the write.
      if (this.disposed || this.sessionId !== sid) return;
      // Decode base64 and write to terminal
      const bytes = Uint8Array.from(atob(data), c => c.charCodeAt(0));
      this.terminal.write(bytes);
    });

    // Subscribe to PTY exit. The payload separates a clean `exit` from a crash
    // or a signal, which is what decides whether the pane is worth keeping.
    //
    // EventsOnce, not EventsOn: a session exits once, and Wails' dispatch loop
    // iterates a *snapshot* of the listener list which it writes back over the
    // live one when the loop ends — so an unsubscribe decided from inside this
    // very callback (which is what handleSessionExit does) is silently undone,
    // leaving the listener, its closure and this whole pane alive forever. A
    // one-shot listener is removed by that loop itself, from the snapshot, so
    // the removal is what gets written back.
    this.exitEventCleanup = window.runtime.EventsOnce('pty:exit:' + sid, (payload?: PtyExitPayload) => {
      // A session this pane has already retired — closed by the user, or
      // replaced by a restart — must not be able to reach back in.
      if (this.disposed || this.sessionId !== sid) return;
      this.handleSessionExit({
        exitCode: typeof payload?.exitCode === 'number' ? payload.exitCode : 0,
        signal: typeof payload?.signal === 'string' ? payload.signal : '',
      });
    });

    this.lastCols = this.terminal.cols;
    this.lastRows = this.terminal.rows;

    // A shell that dies immediately can emit its exit before the subscription
    // above existed, and Wails does not replay events — the pane would then sit
    // there quietly dead, with nothing on screen and Enter doing nothing. The
    // backend drops the session from its map *before* emitting the exit, so
    // asking now is decisive: `true` means the event is still to come, `false`
    // means it already fired — either already handled above, or missed.
    let stillThere = true;
    try {
      stillThere = await window.go.main.App.SessionExists(sid);
    } catch {
      // No answer is not an answer: assume the shell is alive rather than
      // declaring a working pane dead on a transient binding failure.
    }
    if (!stillThere && !this.disposed && this.sessionId === sid) {
      // The real status went with the event that was lost, so this is the
      // backend's "unknown" pair — not a clean exit, so the pane stays and
      // says so. If the real event arrives after all, unsubscribeSession() has
      // cleared sessionId by then and its `this.sessionId !== sid` guard drops it.
      this.handleSessionExit({ exitCode: -1, signal: '' });
    }
  }

  /** Send input to this pane's shell. A no-op without a live session, so a dead
   *  pane can't turn every keystroke into a rejected WriteToSession promise. */
  private sendInput(data: string): void {
    if (!this.sessionId) return;
    window.go.main.App.WriteToSession(this.sessionId, utf8ToBase64(data)).catch(() => {
      // The shell can die between the keystroke and the write; the exit handler
      // is what tells the user, so there is nothing to report here.
    });
  }

  /** Detach from the current session's events and forget its id — from here on
   *  the pane is dead: getCWD() falls back to the cache, input is ignored.
   *
   *  Safe to call from inside an event callback: the pane is retired
   *  synchronously, but the unsubscribe calls themselves are deferred. Wails
   *  dispatches an event over a snapshot of that event's listener list and
   *  writes the snapshot back afterwards, so an unsubscribe run mid-dispatch is
   *  reverted; a microtask runs only once the whole synchronous dispatch —
   *  write-back included — has finished. Outside a dispatch this is just one
   *  tick later, and nothing observes the difference: `sessionId` is already
   *  gone, so both handlers ignore anything that lands in between. */
  private unsubscribeSession(): void {
    const offOutput = this.eventCleanup;
    const offExit = this.exitEventCleanup;
    this.eventCleanup = null;
    this.exitEventCleanup = null;
    this.sessionId = '';
    if (!offOutput && !offExit) return;
    queueMicrotask(() => {
      if (offOutput) offOutput();
      if (offExit) offExit();
    });
  }

  private handleSessionExit(exit: PtyExitPayload): void {
    this.unsubscribeSession();
    if (this.onExit) {
      this.onExit(exit);
    } else {
      this.showExitNotice(exit);
    }
  }

  /** Tell the user why the pane went quiet and arm Enter to bring it back. */
  showExitNotice(exit: PtyExitPayload): void {
    if (this.disposed) return;
    const clean = exit.exitCode === 0 && exit.signal === '';
    // -1 with no signal is the backend saying its shutdown ladder gave up on a
    // wedged process: the session is over, but how it ended is unknowable.
    // Reported as-is, and — not being a clean exit — the pane stays.
    const unknown = exit.exitCode === -1 && exit.signal === '';
    const reason = exit.signal
      ? `[Process terminated by ${sanitizeForTerminal(exit.signal)}]`
      : clean ? '[Process exited]'
      : unknown ? '[Process ended; exit status unknown]'
      : `[Process exited with code ${exit.exitCode}]`;
    const hint = clean ? 'Press Enter to start a new shell' : 'Press Enter to restart';
    this.terminal.write(`\r\n\x1b[${clean ? '2' : '31'}m${reason}\x1b[0m\r\n\x1b[2m${hint}\x1b[0m\r\n`);
    this.awaitingRestart = true;
  }

  /** Write one dim line into the pane on the app's own behalf — something the
   *  shell can't say for itself. The text may carry a backend-supplied path or
   *  message, so it is sanitized like every other foreign string. */
  writeNotice(text: string): void {
    if (this.disposed) return;
    this.terminal.write(`\r\n\x1b[2m${sanitizeForTerminal(text)}\x1b[0m\r\n`);
  }

  /** A shell that never started at all — same dead pane, different message. */
  showStartFailure(err: unknown): void {
    if (this.disposed) return;
    const msg = sanitizeForTerminal(err instanceof Error ? err.message : String(err));
    this.terminal.write(`\r\n\x1b[31m[Failed to start shell: ${msg}]\x1b[0m\r\n\x1b[2mPress Enter to retry\x1b[0m\r\n`);
    this.awaitingRestart = true;
  }

  /** Start a fresh shell in this pane, in its last-known directory. Reached
   *  from Enter on a dead pane, so it never rejects — a failure just re-arms
   *  the retry hint. */
  async restart(): Promise<void> {
    if (this.disposed || this.connecting || this.sessionId) return;
    this.awaitingRestart = false;
    this.unsubscribeSession();
    // The dead shell may have left the alternate buffer, mouse tracking or
    // bracketed paste switched on; only a full reset guarantees a usable slate.
    // Drop the tracked command blocks first so their markers — and the badges
    // hanging off them — go with the buffer they point into.
    this.shellIntegration.reset();
    this.terminal.reset();
    try {
      await this.connect(this.lastKnownCwd);
    } catch (e) {
      console.error('Failed to restart shell:', e);
      this.showStartFailure(e);
    }
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
      this.resizeTimer = null;
      // A pane whose shell has exited has nothing left to resize.
      if (!this.sessionId) return;
      // Fire-and-forget: the binding returns a promise, so a shell that died
      // between the check above and here must be caught, not thrown.
      window.go.main.App.ResizeSession(this.sessionId, cols, rows).catch(() => {});
      // Clear stale prompt artifacts after resize — only when the shell is idle.
      // If a process is running (yarn dev, etc.), SIGWINCH from the PTY resize
      // is enough. Sending \x0c to a running process prints ^L.
      if (this.terminal.buffer.active.type === 'normal') {
        try {
          const statuses = await window.go.main.App.GetAllSessionStatuses();
          const status = statuses[this.sessionId];
          if (status?.isIdle) {
            window.go.main.App.WriteToSession(this.sessionId, utf8ToBase64('\x0c')).catch(() => {});
          }
        } catch { /* skip clear on error */ }
      }
    }, 150);
  }

  async getCWD(): Promise<string> {
    // The shell's own OSC 7 report is authoritative and costs nothing. Only a
    // shell without integration (fish, a custom rc that resets hooks) needs the
    // backend probe, which forks lsof and can fail transiently. Either way,
    // never return a worse answer than the last directory we actually knew.
    if (this.cwdReportedByShell || !this.sessionId) return this.lastKnownCwd;
    try {
      const cwd = await window.go.main.App.GetSessionCWD(this.sessionId);
      if (cwd) {
        this.lastKnownCwd = cwd;
        return cwd;
      }
    } catch {
      /* fall through to the cached value */
    }
    return this.lastKnownCwd;
  }

  private initContextMenu(container: HTMLElement): void {
    let menu: HTMLElement | null = null;

    const dismiss = () => {
      if (menu) { menu.remove(); menu = null; }
      document.removeEventListener('mousedown', dismiss);
      document.removeEventListener('keydown', dismiss);
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
            // Go through xterm's paste path, exactly like Cmd+V: writing the
            // raw text to the PTY skips bracketed paste, so a multi-line
            // clipboard would execute every line the moment it arrives.
            this.terminal.paste(text);
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
        {
          type: 'action', label: CMD.COPY_LAST_OUTPUT.name,
          enabled: this.shellIntegration.getLastCommandOutput() !== null,
          action: () => {
            const output = this.shellIntegration.getLastCommandOutput();
            if (output) navigator.clipboard.writeText(output);
          },
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

      document.addEventListener('mousedown', dismiss);
      document.addEventListener('keydown', dismiss);
    }, true);
  }

  setContextActions(actions: TerminalContextActions): void {
    this.ctxActions = actions;
  }

  clear(): void {
    this.terminal.clear();
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
    if (this.disposed) return;
    this.disposed = true;
    this.awaitingRestart = false;
    // Unsubscribe before closing, so the exit event this close provokes can no
    // longer reach a pane that is already on its way out.
    const sid = this.sessionId;
    this.unsubscribeSession();
    this.smartRender.dispose();
    this.shellIntegration.dispose();
    if (this.cwdOscHandler) {
      this.cwdOscHandler.dispose();
      this.cwdOscHandler = null;
    }
    if (this.resizeTimer) {
      clearTimeout(this.resizeTimer);
      this.resizeTimer = null;
    }
    if (this.resizeObserver) this.resizeObserver.disconnect();
    // A pane whose shell already exited has no session left to close, and the
    // backend ignores ids it doesn't know — either way this must not throw.
    if (sid) {
      window.go.main.App.CloseSession(sid).catch(() => { /* already gone */ });
    }
    this.terminal.dispose();
  }
}
