import { Terminal, IDisposable } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebglAddon } from '@xterm/addon-webgl';
import { SearchAddon } from '@xterm/addon-search';
import { WebLinksAddon } from '@xterm/addon-web-links';
import { Unicode11Addon } from '@xterm/addon-unicode11';
import '@xterm/xterm/css/xterm.css';
import '../types/wails.d.ts';
import type { PtyExitPayload } from '../types/wails.d.ts';
import { utf8ToBase64, base64ToBytes } from '../utils';
import { CMD, ACK_FLUSH_BYTES, ACK_FLUSH_MS, SELECTION_COPY_DEBOUNCE_MS, TITLE_MAX_LENGTH } from '../constants';
import { logError, logWarn } from '../log';
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

/** The slice of the user's settings a pane actually runs on. The host builds
 *  it — the session's font zoom is folded into `fontSize` there, and
 *  `cursorBlink` is already ANDed with "is this the focused pane". */
export interface PaneOptions {
  fontFamily: string;
  fontSize: number;
  lineHeight: number;
  cursorStyle: 'block' | 'underline' | 'bar';
  cursorBlink: boolean;
  scrollback: number;
  /** xterm's `macOptionIsMeta`, from the `optionIsMeta` setting. */
  macOptionIsMeta: boolean;
  copyOnSelect: boolean;
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
  /** Scrollback search, driven by the find bar. Public because the bar is the
   *  host's — one bar at a time, for whichever pane is active. */
  public readonly search: SearchAddon;
  private fitAddon: FitAddon;
  private webLinksAddon: WebLinksAddon;
  private unicodeAddon: Unicode11Addon;
  private container: HTMLElement;
  private resizeObserver: ResizeObserver;
  private eventCleanup: (() => void) | null = null;
  private exitEventCleanup: (() => void) | null = null;
  private resizeTimer: ReturnType<typeof setTimeout> | null = null;
  private lastCols: number = 0;
  private lastRows: number = 0;
  // The GPU renderer, when this pane is the one holding a context — see
  // enableWebgl(). Null means xterm's own canvas renderer is drawing.
  private webglAddon: WebglAddon | null = null;
  // Set once a WebGL2 context has been refused, so the failure is reported once
  // per pane instead of on every tab switch. Enabling is still retried: the
  // usual reason for a refusal is a momentarily full context budget, which the
  // next switch has probably freed.
  private webglRefusalReported = false;
  // Bytes written to xterm but not yet acknowledged to the backend, and the
  // timer that flushes them when the flood stops short of the byte threshold.
  private pendingAckBytes = 0;
  private ackTimer: ReturnType<typeof setTimeout> | null = null;
  private ctxActions: TerminalContextActions | null = null;
  // What this pane is meant to be running with. Kept so the font options can be
  // re-applied on the way back on screen — see applyOptions().
  private options: PaneOptions;
  private fontOptionsPending = false;
  private selectionCopyTimer: ReturnType<typeof setTimeout> | null = null;
  // The button of the last mousedown on this pane, with a macOS Ctrl+click
  // counted as the right one, and when that right-click was. Together they are
  // what keeps copy-on-select out of a context-menu gesture — see
  // inRightClickGesture().
  private lastMouseButton = 0;
  private lastRightClickAt = 0;
  public shellIntegration: ShellIntegration;
  public smartRender: SmartRenderManager;
  // Last directory the shell reported (OSC 7), or the one this pane was opened
  // with. Never cleared once set: a momentary failure to read the cwd must not
  // be allowed to erase the pane's folder from the saved layout.
  private lastKnownCwd: string = '';
  private cwdReportedByShell = false;
  // The last title the shell set with OSC 0/2, or '' if it never has. Second in
  // line behind a hand-renamed tab when the tab bar picks a name.
  private oscTitle = '';
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
  /** Called when the shell rang the bell (BEL, or OSC 777). The host owns what
   *  a bell *means* — the sound, the flash, the tab marker — because only it
   *  knows whether this pane is the one being looked at. */
  public onBell: (() => void) | null = null;
  /** Called when anything that feeds this pane's displayed name changes: the
   *  OSC 0/2 title, or the OSC 7 working directory. Fired on every report, so
   *  the host debounces rather than the pane guessing what it is for. */
  public onTitleChange: (() => void) | null = null;

  constructor(container: HTMLElement, theme: XtermTheme, options: PaneOptions) {
    this.container = container;
    this.options = options;

    this.terminal = new Terminal({
      cursorBlink: options.cursorBlink,
      cursorStyle: options.cursorStyle,
      fontFamily: options.fontFamily,
      fontSize: options.fontSize,
      lineHeight: options.lineHeight,
      scrollback: options.scrollback,
      macOptionIsMeta: options.macOptionIsMeta,
      theme: theme,
      allowProposedApi: true,
      rightClickSelectsWord: true,
    });

    this.fitAddon = new FitAddon();
    this.terminal.loadAddon(this.fitAddon);

    // Unicode 11 widths, before anything is written: the addon only *registers*
    // the provider (its activate() does nothing else), so the version has to be
    // selected here. Without it xterm measures emoji and CJK with the Unicode 6
    // table and every line containing one drifts a column out of step with what
    // the shell thinks it drew.
    this.unicodeAddon = new Unicode11Addon();
    this.terminal.loadAddon(this.unicodeAddon);
    this.terminal.unicode.activeVersion = '11';

    // URLs become clickable, but only with Cmd held. A terminal's mouse belongs
    // to the program running in it — selecting text, vim, tmux, `less` — so a
    // plain click must never navigate anywhere; the addon's default handler
    // does exactly that, which is why it is replaced rather than configured.
    this.webLinksAddon = new WebLinksAddon((event: MouseEvent, uri: string) => {
      if (!event.metaKey) return;
      // The only way out of the webview: window.open() inside WKWebView with no
      // UI delegate goes nowhere, and navigating this frame would replace the app.
      try {
        window.runtime.BrowserOpenURL(uri);
      } catch (e) {
        logWarn(`Failed to open ${uri} in the browser`, e);
      }
    });
    this.terminal.loadAddon(this.webLinksAddon);

    this.search = new SearchAddon();
    this.terminal.loadAddon(this.search);

    this.terminal.open(container);

    // OSC 0 / OSC 2: the window title the shell (or the program it is running)
    // wants. Second in line for the tab's name, behind a manual rename.
    this.terminal.onTitleChange((title: string) => {
      // Whatever is on the far end of the pty chose this string — a remote
      // shell as readily as a local one — and it ends up in a DOM attribute
      // and in the native window title. Control bytes go, and it is cut to a
      // length a tab bar and a Window menu can actually hold.
      const next = sanitizeForTerminal(title).trim().slice(0, TITLE_MAX_LENGTH);
      if (next === this.oscTitle) return;
      this.oscTitle = next;
      this.onTitleChange?.();
    });

    this.terminal.onBell(() => this.onBell?.());

    // Which pointer gesture is in progress, watched from the pane's own element
    // so it is known before xterm has decided what the click means. Capture
    // phase for the same reason: this must see the press whatever the layers
    // below do with it.
    container.addEventListener('mousedown', (e: MouseEvent) => {
      // macOS turns Ctrl+left-click into a context-menu click, and everything
      // below treats it as the right button — the webview, xterm's right-click
      // word select, and this pane's own menu.
      const contextClick = e.button === 2 || (e.button === 0 && e.ctrlKey);
      this.lastMouseButton = contextClick ? 2 : e.button;
      if (contextClick) this.lastRightClickAt = performance.now();
    }, true);

    // Copy-on-select. The event fires on every mouse move that extends a drag,
    // so only the trailing edge is worth a clipboard write; an empty selection
    // is the *clearing* of one and must not wipe what was copied before it.
    //
    // And only a left-button drag counts. `rightClickSelectsWord` means a
    // right-click *makes* a selection — the word under the pointer — and fires
    // this event with it, so without the gesture test the click that opens the
    // context menu would silently overwrite the clipboard, and the menu's own
    // Paste would then paste that word back into the shell.
    this.terminal.onSelectionChange(() => {
      if (!this.options.copyOnSelect) return;
      if (this.inRightClickGesture()) return;
      if (this.selectionCopyTimer) clearTimeout(this.selectionCopyTimer);
      this.selectionCopyTimer = setTimeout(() => {
        this.selectionCopyTimer = null;
        if (this.disposed || !this.options.copyOnSelect) return;
        // Re-checked: a right-click landing inside the debounce window is the
        // user reaching for the menu, whatever the selection was before it.
        if (this.inRightClickGesture()) return;
        const text = this.terminal.getSelection();
        if (!text) return;
        navigator.clipboard.writeText(text).catch(() => {
          // Denied or unfocused — the explicit Cmd+C path still works.
        });
      }, SELECTION_COPY_DEBOUNCE_MS);
    });

    // Register OSC 133 handler for shell integration (command blocks)
    this.shellIntegration = new ShellIntegration(this.terminal);

    // OSC 7: the shell tells us its working directory on every prompt and on
    // every cd. This is authoritative and free — no process spawn, nothing that
    // can be denied or time out — so it is preferred over asking the backend.
    this.cwdOscHandler = this.terminal.parser.registerOscHandler(7, (data: string) => {
      const cwd = parseOsc7Path(data);
      if (cwd) {
        const changed = cwd !== this.lastKnownCwd;
        this.lastKnownCwd = cwd;
        this.cwdReportedByShell = true;
        // Last in line behind the OSC title, but it is what most shells give
        // us, so a `cd` renames the tab.
        if (changed) this.onTitleChange?.();
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

    // No WebGL here on purpose: a context is only worth holding while the pane
    // is on screen, and the host decides that — see enableWebgl().

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
      const bytes = base64ToBytes(data);
      // The callback is xterm's own "I have finished with this chunk" signal,
      // and the only honest moment to acknowledge it: acking on arrival would
      // tell the backend to keep reading while the bytes are still queued in
      // the renderer, which is the stall this flow control exists to prevent.
      this.terminal.write(bytes, () => {
        // The write queue drains asynchronously, so by now the pane may have
        // been disposed or restarted onto a new session. Those bytes were
        // never this session's to account for.
        if (this.disposed || this.sessionId !== sid) return;
        this.queueAck(sid, bytes.length);
      });
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

    // Now that both listeners exist, claim the session. Two things come out of
    // this one call. The backend flushes whatever the shell wrote between
    // CreateSession and here — a fast banner, or a shell that printed an error
    // and quit — which no listener could have caught, and Wails does not replay
    // events. And it reports whether the session is still there: the backend
    // drops it from its map *before* emitting the exit, so the answer is
    // decisive — `true` means the event is still to come, `false` means it has
    // already fired, either handled above or missed entirely.
    let stillThere = true;
    try {
      stillThere = await window.go.main.App.AttachSession(sid);
    } catch (e) {
      // No answer is not an answer: assume the shell is alive rather than
      // declaring a working pane dead on a transient binding failure.
      logWarn(`AttachSession failed for session ${sid}; assuming the shell is alive`, e);
    }
    if (!stillThere && !this.disposed && this.sessionId === sid) {
      // The real status went with the event that was lost, so this is the
      // backend's "unknown" pair — not a clean exit, so the pane stays and
      // says so. If the real event arrives after all, unsubscribeSession() has
      // cleared sessionId by then and its `this.sessionId !== sid` guard drops it.
      this.handleSessionExit({ exitCode: -1, signal: '' });
    }
  }

  /** True while the pointer gesture on this pane is a context-menu one: the last
   *  button pressed was the right one (or a macOS Ctrl+click), or one was
   *  pressed within the copy debounce window. A selection made by such a click
   *  is xterm selecting the word under the pointer for the menu, not the user
   *  choosing something to copy. */
  private inRightClickGesture(): boolean {
    if (this.lastMouseButton !== 0) return true;
    return performance.now() - this.lastRightClickAt < SELECTION_COPY_DEBOUNCE_MS;
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

  /** Type text into this pane's shell on the app's own behalf — a dropped file
   *  path, say. A no-op on a dead pane, exactly like a keystroke. */
  sendText(text: string): void {
    this.sendInput(text);
  }

  /** Paste text into this pane on the app's own behalf — text or a link dragged
   *  in from another application. Goes through xterm's paste path, exactly like
   *  Cmd+V and the context menu: writing it as raw input would skip bracketed
   *  paste, and a multi-line drop would then run line by line the moment it
   *  landed instead of sitting on the command line to be looked at. */
  pasteText(text: string): void {
    if (this.disposed || !this.sessionId) return;
    this.terminal.paste(text);
  }

  /** Count `n` more written bytes towards this session's acknowledgement, and
   *  send it once it is worth a binding call. A flood arrives as thousands of
   *  small chunks, so acking each one would put more traffic on the bridge than
   *  the output itself; the timer is what guarantees the last few hundred bytes
   *  of a quiet session are still acknowledged. */
  private queueAck(sid: string, n: number): void {
    this.pendingAckBytes += n;
    if (this.pendingAckBytes >= ACK_FLUSH_BYTES) {
      this.flushAck(sid);
      return;
    }
    if (!this.ackTimer) {
      this.ackTimer = setTimeout(() => {
        this.ackTimer = null;
        this.flushAck(sid);
      }, ACK_FLUSH_MS);
    }
  }

  private flushAck(sid: string): void {
    if (this.ackTimer) {
      clearTimeout(this.ackTimer);
      this.ackTimer = null;
    }
    const n = this.pendingAckBytes;
    this.pendingAckBytes = 0;
    // A session this pane has retired is gone from the backend's books along
    // with its byte count; the binding would ignore the id anyway.
    if (n <= 0 || this.sessionId !== sid) return;
    window.go.main.App.AckOutput(sid, n).catch(() => {
      // The shell can exit between the write and the ack. Nothing left to
      // unblock, and the exit handler is what tells the user.
    });
  }

  /** Drop the outstanding acknowledgement. Called when the session it belonged
   *  to is retired: the backend has forgotten the session and its counter, so
   *  those bytes are no longer owed to anyone. */
  private cancelAcks(): void {
    if (this.ackTimer) {
      clearTimeout(this.ackTimer);
      this.ackTimer = null;
    }
    this.pendingAckBytes = 0;
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
    // Whatever was still owed belonged to the session just retired.
    this.cancelAcks();
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
    // Same reason as the command blocks: the search addon's highlights are
    // decorations hung off markers in the buffer that is about to go. The
    // addon itself survives reset() — reset() rebuilds the buffer and the
    // input handler, and touches neither the addon manager nor the link
    // providers nor the registered Unicode versions — but its cached term
    // would go on describing a screen that no longer exists.
    this.search.clearDecorations();
    this.terminal.reset();
    try {
      await this.connect(this.lastKnownCwd);
    } catch (e) {
      logError(`Failed to restart shell in ${this.lastKnownCwd || 'the default directory'}`, e);
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
    this.resizeTimer = setTimeout(() => {
      this.resizeTimer = null;
      // A pane whose shell has exited has nothing left to resize.
      if (!this.sessionId) return;
      // Resizing the pty is the whole job: the kernel raises SIGWINCH and the
      // foreground program — zsh, vim, less — redraws itself. A terminal does
      // not type on the user's behalf, and the \x0c this used to append landed
      // in whatever was running whenever the idle probe was wrong or failed.
      //
      // Fire-and-forget: the binding returns a promise, so a shell that died
      // between the check above and here must be caught, not thrown.
      window.go.main.App.ResizeSession(this.sessionId, cols, rows).catch(() => {});
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

  /** The title the shell last set with OSC 0/2, or '' if it never has. */
  get title(): string {
    return this.oscTitle;
  }

  /** The last directory this pane is known to have been in — the shell's own
   *  OSC 7 report where there is one, otherwise the directory it was opened
   *  with. Never '' once anything has been established. */
  get cwd(): string {
    return this.lastKnownCwd;
  }

  /** Apply a new set of options to a live pane: a settings reload, or a font
   *  zoom.
   *
   *  Everything except the font is set straight away. The font is held back
   *  while the pane is off screen — not because xterm 5.5 needs it to be, but
   *  because the one case where it would matter cannot be told apart from here.
   *  The default cell measurement is TextMetricsMeasureStrategy: an
   *  OffscreenCanvas `measureText`, which depends on no layout at all and is
   *  perfectly correct in a detached container. Only the DomMeasureStrategy
   *  fallback — a hidden span the browser has to lay out — reads zero there, and
   *  a zero measurement is *discarded*, leaving the previous cell size in place
   *  with nothing to re-measure on the way back; the next fit() would then size
   *  the pty from the old font's metrics. So the font waits for flushOptions(),
   *  which the host calls once the pane is back in the document. On the common
   *  path that costs one deferred assignment and changes nothing. */
  applyOptions(options: PaneOptions): void {
    if (this.disposed) return;
    this.options = options;
    this.terminal.options.cursorStyle = options.cursorStyle;
    this.terminal.options.cursorBlink = options.cursorBlink;
    this.terminal.options.scrollback = options.scrollback;
    this.terminal.options.macOptionIsMeta = options.macOptionIsMeta;
    if (this.container.isConnected) {
      this.applyFontOptions();
    } else {
      this.fontOptionsPending = true;
    }
  }

  /** Push font options that were set while this pane was off screen. Cheap and
   *  idempotent: xterm fires nothing when an option is assigned its current
   *  value, so calling this on every tab switch costs a few comparisons. */
  flushOptions(): void {
    if (this.disposed || !this.fontOptionsPending || !this.container.isConnected) return;
    this.applyFontOptions();
  }

  private applyFontOptions(): void {
    this.fontOptionsPending = false;
    this.terminal.options.fontFamily = this.options.fontFamily;
    this.terminal.options.fontSize = this.options.fontSize;
    this.terminal.options.lineHeight = this.options.lineHeight;
  }

  setTheme(theme: XtermTheme): void {
    // Renderer-agnostic: xterm pushes the new colours into whichever renderer
    // is attached, so this works the same with or without WebGL.
    this.terminal.options.theme = theme;
  }

  /** Hand this pane a GPU renderer. Idempotent, and only worth calling while
   *  the pane is actually on screen: a WebGL context is a scarce, browser-wide
   *  resource — past the cap the oldest ones are dropped, and the pane that
   *  owned one goes blank. So contexts follow visibility, and the host gives
   *  them back on the way out (see disableWebgl).
   *
   *  A context lost anyway — the tab count crept up, the GPU reset, the machine
   *  woke from sleep — is caught rather than left to blank the pane: xterm's
   *  addon waits ~3s for the browser to restore it (`WebglRenderer`: "Wait a
   *  few seconds to see if the 'webglcontextrestored' event is fired. If not,
   *  dispatch the onContextLoss notification to observers") and only then tells
   *  us, at which point disposing the addon puts xterm's own canvas renderer
   *  back — a slower pane, but a visible one. */
  enableWebgl(): void {
    if (this.disposed || this.webglAddon) return;
    try {
      const addon = new WebglAddon();
      addon.onContextLoss(() => {
        logWarn(`WebGL context lost for pane (session ${this.sessionId || 'none'}); falling back to the canvas renderer`);
        this.disableWebgl();
      });
      this.terminal.loadAddon(addon);
      this.webglAddon = addon;
    } catch (e) {
      // No context to be had — this webview has no WebGL2, or its budget is
      // full right now. Canvas draws the pane perfectly well either way.
      this.webglAddon = null;
      if (!this.webglRefusalReported) {
        this.webglRefusalReported = true;
        logWarn('WebGL renderer unavailable for this pane; using xterm\'s canvas renderer', e);
      }
    }
  }

  /** Give the GPU context back. Disposing the addon is what restores xterm's
   *  canvas renderer, so the pane keeps drawing. Idempotent, and safe to call
   *  from inside the addon's own onContextLoss. */
  disableWebgl(): void {
    const addon = this.webglAddon;
    if (!addon) return;
    // Cleared first so a re-entrant call — the context loss handler firing
    // while dispose() is unwinding — finds nothing left to do.
    this.webglAddon = null;
    try {
      addon.dispose();
    } catch (e) {
      logWarn('Failed to dispose the WebGL renderer', e);
    }
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
    if (this.selectionCopyTimer) {
      clearTimeout(this.selectionCopyTimer);
      this.selectionCopyTimer = null;
    }
    if (this.resizeObserver) this.resizeObserver.disconnect();
    // Before terminal.dispose(): the addon's teardown reaches back into the
    // terminal's render service to reinstate the canvas renderer.
    this.disableWebgl();
    // The addon manager would dispose these itself, but it does it after
    // terminal.dispose() has already torn down what they hold — the search
    // addon's decorations, the link provider's registration. Disposing here is
    // safe either way: every one of them is idempotent, and xterm wraps a
    // loaded addon's dispose() so the manager's later pass is a no-op.
    this.search.dispose();
    this.webLinksAddon.dispose();
    this.unicodeAddon.dispose();
    // A pane whose shell already exited has no session left to close, and the
    // backend ignores ids it doesn't know — either way this must not throw.
    if (sid) {
      window.go.main.App.CloseSession(sid).catch(() => { /* already gone */ });
    }
    this.terminal.dispose();
  }
}
