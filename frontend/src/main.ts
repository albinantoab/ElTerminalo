import { TerminalPane, setLocalHostname } from './terminal/TerminalPane';
import type { PaneOptions } from './terminal/TerminalPane';
import { AppTheme, themeFromDTO, applyThemeToCSS } from './theme/themes';
import { PaneInfo, SplitNode, Tab, SavedSplitNode, SavedState, SavedTab, CustomCommand, PaletteCommand } from './types';
import { CommandPalette } from './palette/CommandPalette';
import { CommandWizard } from './wizard/CommandWizard';
import { ThemeWizard } from './wizard/ThemeWizard';
import { StateManager } from './state/StateManager';
import { StatusModal } from './status/StatusModal';
import { AskAI } from './ai/AskAI';
import { HistoryModal } from './history/HistoryModal';
import { FindBar } from './find/FindBar';
import { escHtml, generateId, waitForLayout, utf8ToBase64 } from './utils';
import {
  MAX_TABS, DOUBLE_CLICK_DELAY_MS, MIN_SPLIT_RATIO, MAX_SPLIT_RATIO,
  DEFAULT_SPLIT_RATIO, SPATIAL_NAV_THRESHOLD, STATE_SAVE_INTERVAL_MS,
  MAX_DROP_PATHS, MAX_CLOSED_TABS, MIN_FONT_SIZE, MAX_FONT_SIZE,
  SETTINGS_FONT_SIZE_MIN, SETTINGS_FONT_SIZE_MAX, MENU_ACTION_DEDUP_MS,
  TITLE_REFRESH_DEBOUNCE_MS, BELL_FLASH_MS, BELL_COALESCE_MS, TAB_DRAG_MIME,
  MODAL_PASSTHROUGH_META_KEYS, DEFAULT_SETTINGS, CMD,
} from './constants';
import { logError, logInfo } from './log';
import './types/wails.d.ts';
import type {
  PtyExitPayload, FilesDroppedPayload, Settings, MenuActionPayload, ThemesChangedPayload,
} from './types/wails.d.ts';

/** Cancel the webview's own drag and drop, from the moment the page exists.
 *
 *  WKWebView's default action for a dropped file is to *load* it in the main
 *  frame: the app is replaced by a directory listing, and a terminal has no
 *  back button. Nothing else stops that. The app runs with
 *  `DisableWebViewDrop: false` — deliberately, see main.go — so the webview
 *  keeps its default handling; Wails v2.11 installs no
 *  `decidePolicyForNavigationAction` of its own; and the handlers that would
 *  intercept the drop — with them the whole `--wails-drop-target` CSS opt-in —
 *  are installed only by the *JavaScript* runtime.OnFileDrop, which this app
 *  does not call. It subscribes on the Go side instead, and that emitter tests
 *  no styles and asks no permission. The page's own `preventDefault()` is the
 *  only guard there is.
 *
 *  Which is why this runs at module load rather than from `init()`. `init()`
 *  is called on DOMContentLoaded and then awaits the AI model download — some
 *  hundreds of megabytes on a fresh install — long before it reaches anything
 *  that installs handlers. A folder dropped on the splash while that ran took
 *  the window with it: the app navigated to `file:///…` and was gone. The two
 *  listeners below need no app to run. They cancel, and do nothing else; what
 *  is *done* with a drop is the ElTerminalo instance's business, and it adds
 *  its own listener for that (see `installWebviewDropHandlers`).
 *
 *  Both are capture-phase, so nothing between the document and the drop target
 *  can stop the event from reaching the one line that matters. */
function installDropNavigationGuard(): void {
  // Cancelling dragover is also what makes the page a drop target at all.
  // Uncancelled, the webview refuses whatever it has no default action of its
  // own for: a dragged text selection gets a "no" cursor and never produces a
  // drop event. (A dragged file it accepts unasked — and then navigates to,
  // which is what the drop handler below exists to stop.)
  document.addEventListener('dragover', (e: DragEvent) => {
    // A tab being dragged along the tab bar is the app's own gesture, not
    // something arriving from outside it; the tab bar's own handlers decide
    // where it may land, and claiming `copy` here would show the wrong cursor
    // over every part of the window that is not a valid drop target.
    if (isTabDrag(e)) return;
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy';
  }, true);

  document.addEventListener('drop', (e: DragEvent) => {
    if (isTabDrag(e)) return;
    e.preventDefault();
  }, true);
}

/** True when the drag in progress is one of this app's own tabs. */
function isTabDrag(e: DragEvent): boolean {
  return !!e.dataTransfer && Array.from(e.dataTransfer.types).includes(TAB_DRAG_MIME);
}

// Before DOMContentLoaded, before init(), before the splash's first frame.
installDropNavigationGuard();

const ICON_CPU =
  '<svg class="status-stat-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">'
  + '<rect x="5" y="5" width="14" height="14" rx="1.5"/>'
  + '<rect x="9" y="9" width="6" height="6"/>'
  + '<path d="M9 2v3M15 2v3M9 19v3M15 19v3M2 9h3M2 15h3M19 9h3M19 15h3"/>'
  + '</svg>';

const ICON_MEM =
  '<svg class="status-stat-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">'
  + '<rect x="2" y="7" width="20" height="10" rx="1"/>'
  + '<path d="M6 11v2M10 11v2M14 11v2M18 11v2"/>'
  + '</svg>';

function severityClass(pct: number): string {
  if (pct >= 90) return 'is-critical';
  if (pct >= 70) return 'is-warning';
  return '';
}

function clamp(n: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, n));
}

/** The last segment of a path, for naming a tab after the folder it is in.
 *  `/` keeps its own name; a trailing slash is not a directory called "". */
function basename(path: string): string {
  const trimmed = path.replace(/\/+$/, '');
  if (!trimmed) return path ? '/' : '';
  return trimmed.slice(trimmed.lastIndexOf('/') + 1) || '/';
}

const CURSOR_STYLES: readonly Settings['cursorStyle'][] = ['block', 'underline', 'bar'];
const BELL_MODES: readonly Settings['bell'][] = ['none', 'sound', 'visual', 'both'];
const QUIT_MODES: readonly Settings['confirmQuit'][] = ['running', 'always', 'never'];

/** Make whatever came back from GetSettings safe to hand to xterm.
 *
 *  The Go side validates config.json and fills its own defaults, so in the
 *  normal case this changes nothing. It exists for the cases where that is not
 *  what arrives: an older backend without the field, a binding that resolved
 *  with something unexpected, a value that is valid on disk and still fatal
 *  here. xterm does not ignore an option it dislikes — it *throws* on an
 *  unknown `cursorStyle`, on `lineHeight` below 1, on negative `scrollback` —
 *  and a throw inside the pane constructor takes the window with it. */
function normalizeSettings(raw: Partial<Settings> | null | undefined): Settings {
  const d = DEFAULT_SETTINGS;
  const s = raw ?? {};
  const str = (v: unknown, fallback: string): string =>
    typeof v === 'string' && v.trim() !== '' ? v : fallback;
  const bool = (v: unknown, fallback: boolean): boolean =>
    typeof v === 'boolean' ? v : fallback;
  const int = (v: unknown, fallback: number, lo: number, hi: number): number =>
    typeof v === 'number' && Number.isFinite(v) ? clamp(Math.round(v), lo, hi) : fallback;
  return {
    fontFamily: str(s.fontFamily, d.fontFamily),
    fontSize: int(s.fontSize, d.fontSize, SETTINGS_FONT_SIZE_MIN, SETTINGS_FONT_SIZE_MAX),
    // xterm throws below 1, and a line height above 4 is a blank screen with
    // occasional text in it.
    lineHeight: typeof s.lineHeight === 'number' && Number.isFinite(s.lineHeight)
      ? clamp(s.lineHeight, 1, 4) : d.lineHeight,
    cursorStyle: CURSOR_STYLES.includes(s.cursorStyle as Settings['cursorStyle'])
      ? s.cursorStyle as Settings['cursorStyle'] : d.cursorStyle,
    cursorBlink: bool(s.cursorBlink, d.cursorBlink),
    scrollback: int(s.scrollback, d.scrollback, 0, 1_000_000),
    shell: typeof s.shell === 'string' ? s.shell : d.shell,
    optionIsMeta: bool(s.optionIsMeta, d.optionIsMeta),
    bell: BELL_MODES.includes(s.bell as Settings['bell']) ? s.bell as Settings['bell'] : d.bell,
    copyOnSelect: bool(s.copyOnSelect, d.copyOnSelect),
    confirmQuit: QUIT_MODES.includes(s.confirmQuit as Settings['confirmQuit'])
      ? s.confirmQuit as Settings['confirmQuit'] : d.confirmQuit,
  };
}

class ElTerminalo {
  private tabs: Tab[] = [];
  private activeTabIndex = 0;
  private themes: AppTheme[] = [];
  private currentTheme!: AppTheme;
  private customCommands: CustomCommand[] = [];
  private renamingTabIndex = -1;
  // Everything from config.json, already normalized. Read once before the
  // first pane exists, and again on "Reload Settings".
  private settings: Settings = { ...DEFAULT_SETTINGS };
  // Session-only font zoom, as an offset from the configured size — so a
  // settings reload that changes the size keeps whatever zoom is in effect.
  private zoomDelta = 0;
  // The one find bar there is, or null. It belongs to the pane it was opened
  // on; switching pane or tab takes it down.
  private findBar: FindBar | null = null;
  // Closed tabs, most recent last, for Cmd+Shift+T. Snapshots, not the tabs
  // themselves: the panes are disposed the moment the tab goes.
  private closedTabs: SavedTab[] = [];
  // The last action that actually ran, and where it came from. A keyboard
  // shortcut and its menu item name the same action, and this is what makes a
  // double delivery impossible — see dispatchAction().
  private lastAction: { name: string; at: number; source: 'key' | 'menu' } | null = null;
  // The tab bar is rebuilt from scratch, and a shell redrawing its prompt
  // reports a title and a directory in the same breath.
  private titleRefreshTimer: ReturnType<typeof setTimeout> | null = null;
  // The last title handed to the OS, so an unchanged one isn't sent again.
  private lastWindowTitle = '';
  // Per-pane visual-bell timers, keyed by the pane's element so a pane that
  // goes away takes its entry with it.
  private bellTimers = new WeakMap<HTMLElement, ReturnType<typeof setTimeout>>();
  // When each pane was last allowed to ring, keyed the same way. This is what
  // makes a flood of BELs one bell — see handleBell().
  private bellLastAt = new WeakMap<HTMLElement, number>();
  // The tab being dragged along the tab bar, or -1.
  private draggingTabIndex = -1;

  private container!: HTMLElement;
  private tabBar!: HTMLElement;
  private statusbar!: HTMLElement;

  private palette!: CommandPalette;
  private wizard!: CommandWizard;
  private themeWizard!: ThemeWizard;
  private stateManager!: StateManager;
  private statusModal!: StatusModal;
  private askAI!: AskAI;
  private historyModal!: HistoryModal;
  private aiGenerating = false;
  private aiPrompts = new Set<string>();
  private modelUpdateAvailable = false;
  // Set for the whole of restoreState(). Every save path consults it, because
  // a save taken while the layout is half rebuilt would overwrite the real one.
  private restoring = false;
  // Where a structurally broken state file was moved to, so the fallback tab
  // can tell the user their layout wasn't lost, only set aside. '' when the
  // restore went fine or there was nothing to quarantine.
  private quarantinedStatePath = '';

  private stateSaveInterval: ReturnType<typeof setInterval> | null = null;
  private updateCheckInterval: ReturnType<typeof setInterval> | null = null;
  private statsInterval: ReturnType<typeof setInterval> | null = null;
  private systemStats: { cpuPercent: number; memoryUsedMB: number; memoryTotalMB: number; memoryPercent: number } | null = null;

  private onVisibilityChange = () => {
    if (!document.hidden) {
      this.focusActivePane();
      // After macOS sleep/lock the timer never fires, but visibilitychange
      // does once the user comes back. Backend throttles to once per 24h, so
      // calling on every visibility-restore is cheap.
      this.checkForUpdate();
    }
  };
  private onWindowFocus = () => {
    document.body.classList.remove('app-blurred');
    this.focusActivePane();
    this.checkForUpdate();
  };
  private onWindowBlur = () => {
    document.body.classList.add('app-blurred');
  };
  private onBeforeUnload = () => {
    this.destroy();
  };

  // Current tab helpers
  private get tab(): Tab { return this.tabs[this.activeTabIndex]; }
  private get panes(): PaneInfo[] { return this.tab?.panes || []; }
  private get activeIndex(): number { return this.tab?.activeIndex || 0; }
  private set activeIndex(v: number) { if (this.tab) this.tab.activeIndex = v; }
  private get layoutRoot(): SplitNode | null { return this.tab?.layoutRoot || null; }
  private set layoutRoot(v: SplitNode | null) { if (this.tab) this.tab.layoutRoot = v; }

  async init(): Promise<void> {
    // Drag and drop first — ahead of every await below, and ahead of the very
    // modules these handlers consult. A drop that lands while init() is still
    // working (the model download alone can hold it for minutes on a fresh
    // install, and restoreState() reconnects every saved pane after that) must
    // still be cancelled, and the guard installed at module load is only half
    // of that: it stops the navigation, this decides what to do instead.
    // Both halves are written to do nothing, gracefully, until the rest of the
    // app exists — see isModalOpen() and handleFilesDropped().
    this.installWebviewDropHandlers();

    // Native file drops. macOS hands the webview the real filesystem paths, so
    // the Go side resolves them and reports where the drop landed; nothing is
    // read, copied or staged here — the paths go straight to the shell, which
    // is what the user dragged them onto a terminal to do. Subscribed this
    // early for the same reason: the event fires whether or not there are
    // panes yet, and it is better answered than missed.
    window.runtime.EventsOn('files:dropped', (payload?: FilesDroppedPayload) => {
      this.handleFilesDropped(payload);
    });

    this.container = document.getElementById('pane-container')!;
    this.tabBar = document.getElementById('tab-bar')!;
    this.statusbar = document.getElementById('statusbar')!;
    const paletteOverlay = document.getElementById('palette-overlay')!;
    const wizardOverlay = document.getElementById('wizard-overlay')!;

    // Create composed modules with callbacks
    this.palette = new CommandPalette(paletteOverlay, {
      getBuiltInCommands: () => this.getBuiltInCommands(),
      getCustomCommands: () => this.customCommands,
      getActiveSessionId: () => this.panes[this.activeIndex]?.pane?.sessionId || '',
      getActivePaneCWD: () => this.getActivePaneCWD(),
      focusActivePane: () => this.focusActivePane(),
      refreshCustomCommands: () => this.refreshCustomCommands(),
      onEditCommand: (cmd) => this.editCustomCommand(cmd),
      onEditTheme: (cmd) => this.editTheme(cmd),
      onDeleteTheme: (cmd) => this.deleteTheme(cmd),
    });

    this.wizard = new CommandWizard(wizardOverlay, {
      getActivePaneCWD: () => this.getActivePaneCWD(),
      focusActivePane: () => this.focusActivePane(),
      refreshCustomCommands: () => this.refreshCustomCommands(),
      getCustomCommands: () => this.customCommands,
    });

    const themeWizardOverlay = document.getElementById('theme-wizard-overlay')!;
    this.themeWizard = new ThemeWizard(themeWizardOverlay, {
      onSave: async () => {
        // Re-reads the list and re-applies the current theme in case the edit
        // was to that one.
        await this.refreshThemes();
      },
      focusActivePane: () => {
        if (this.panes[this.activeIndex]) this.panes[this.activeIndex].pane.focus();
      },
    });

    const statusOverlay = document.getElementById('status-overlay')!;
    this.statusModal = new StatusModal(statusOverlay, {
      getTabs: () => this.tabs,
      getActiveTabIndex: () => this.activeTabIndex,
      focusActivePane: () => this.focusActivePane(),
      switchToPane: (tabIndex, paneIndex) => {
        this.switchToTab(tabIndex);
        requestAnimationFrame(() => this.setActive(paneIndex));
      },
    });

    const aiOverlay = document.getElementById('ai-overlay')!;
    this.askAI = new AskAI(aiOverlay, {
      getActiveSessionId: () => this.panes[this.activeIndex]?.pane?.sessionId || '',
      getActivePaneCWD: () => this.getActivePaneCWD(),
      focusActivePane: () => this.focusActivePane(),
      setAILoading: (loading) => this.setAILoading(loading),
    });

    const historyOverlay = document.getElementById('history-overlay')!;
    this.historyModal = new HistoryModal(historyOverlay, {
      getActiveSessionId: () => this.panes[this.activeIndex]?.pane?.sessionId || '',
      getActivePaneCWD: () => this.getActivePaneCWD(),
      focusActivePane: () => this.focusActivePane(),
    });

    this.stateManager = new StateManager({
      getTabs: () => this.tabs,
      getActiveTabIndex: () => this.activeTabIndex,
      getCurrentThemeName: () => this.currentTheme.name,
      isRestoring: () => this.restoring,
    });

    // Needed before any pane connects: OSC 7 reports are filtered by host.
    try { setLocalHostname(await window.go.main.App.GetHostname()); } catch { /* host-less reports still work */ }

    // Before the first pane, because every pane is built from these: font,
    // cursor, scrollback and the Option key are constructor arguments, not
    // things to apply afterwards and watch reflow.
    this.settings = await this.fetchSettings();

    const themeDTOs = await window.go.main.App.GetThemes();
    this.themes = themeDTOs.map(themeFromDTO);
    this.currentTheme = this.themes[0];

    await this.refreshCustomCommands();

    // Download AI model during splash if needed
    await this.ensureModelReady();

    const restored = await this.restoreState();
    if (!restored) {
      applyThemeToCSS(this.currentTheme);
      await this.createTab('Terminalo 1');
      // Only now is there a terminal to say it in.
      this.reportQuarantinedState();
    }

    this.switchToTab(this.activeTabIndex);
    window.addEventListener('keydown', (e: KeyboardEvent) => this.handleKeydown(e), true);

    // Re-focus active pane when app regains focus (after lock/unlock, screen switch, etc.)
    // Without this, shortcuts stop working until the user clicks the terminal.
    // Also hide the active pane border when the app loses focus.
    document.addEventListener('visibilitychange', this.onVisibilityChange);
    window.addEventListener('focus', this.onWindowFocus);
    window.addEventListener('blur', this.onWindowBlur);
    window.addEventListener('beforeunload', this.onBeforeUnload);

    this.renderStatusBar();
    this.stateSaveInterval = setInterval(() => this.stateManager.save(), STATE_SAVE_INTERVAL_MS);

    // Dismiss splash screen
    this.dismissSplash();

    // AI model loads lazily on first Cmd+K use, unloads after idle.

    // Update checks are event-driven, not interval-driven, because setInterval
    // is unreliable across system sleep/lock. The Go backend persists the last
    // check time and throttles to once per 24h. We trigger a check on:
    //   - launch (here)
    //   - window focus / visibility restore (handled in onWindowFocus etc.)
    //   - a 30-min backstop pulse for unattended long-running sessions
    // The backstop is cheap because the backend short-circuits if not due.
    this.checkForUpdate();
    this.checkModelUpdate();
    this.updateCheckInterval = setInterval(() => { this.checkForUpdate(); this.checkModelUpdate(); }, 30 * 60 * 1000);

    // Poll process CPU/memory for the status bar. First call primes the
    // sampler (returns 0% CPU), so kick off immediately and then on a tick.
    this.pollSystemStats();
    this.statsInterval = setInterval(() => this.pollSystemStats(), 3000);

    // Quitting is a handshake: Go puts up the native confirmation dialog itself
    // and, once the user agrees, asks us to persist the layout. It quits on
    // ConfirmQuit or after its own 2s fallback, so this must stay quick and must
    // never bail out — a failed save is not a reason to refuse to quit.
    window.runtime.EventsOn('app:save-and-quit', async () => {
      try {
        await this.stateManager.save();
      } catch (e) {
        logError('Failed to save the window layout before quit', e);
      }
      // Also a promise: nothing left to do if it rejects, but an unhandled
      // rejection is not how this handler should end.
      window.go.main.App.ConfirmQuit().catch(() => {});
    });

    // The native menu bar. Every item routes to the same function its keyboard
    // shortcut does; dispatchAction() is what keeps one gesture from counting
    // twice if the webview ever delivers a Cmd-key to both.
    window.runtime.EventsOn('menu:action', (payload?: MenuActionPayload) => {
      const action = typeof payload?.action === 'string' ? payload.action : '';
      if (action) this.dispatchAction(action, 'menu');
    });

    // The theme files on disk changed — an import, or an edit made outside the
    // app — and `name` is the one to end up on.
    window.runtime.EventsOn('themes:changed', (payload?: ThemesChangedPayload) => {
      const name = typeof payload?.name === 'string' ? payload.name : '';
      this.refreshThemes(name).catch((e) => logError('Failed to reload the theme list', e));
    });
  }

  /** Read config.json through the backend, normalized and never rejecting: a
   *  window that will not start because a settings file could not be read is
   *  worse than one running on the defaults. */
  private async fetchSettings(): Promise<Settings> {
    try {
      return normalizeSettings(await window.go.main.App.GetSettings());
    } catch (e) {
      logError('Failed to read settings; using the defaults', e);
      return { ...DEFAULT_SETTINGS };
    }
  }

  /** What the app does with an HTML5 drop, once the module-level guard has
   *  cancelled it. WKWebView delivers `dragover`/`drop` to the page as well as
   *  posting the native drop that becomes `files:dropped`, so the same gesture
   *  arrives twice, and the two halves split by kind:
   *
   *  - Files are ignored here. They arrive a second time through the native
   *    path, which is the only one that knows their real filesystem paths — the
   *    File objects in this event have names and bytes and no path at all.
   *  - Text is handled here, because the native path never sees it: a selection
   *    dragged out of another app, or a link dragged from Safari, is pasted
   *    into the pane under the pointer.
   *
   *  The cost of cancelling everything is that text can no longer be dropped
   *  into the app's own inputs (the palette's, a wizard's). Nobody drags text
   *  into a terminal's dialog boxes, and the alternative is an app that can be
   *  navigated away from by a misplaced file.
   *
   *  Installed as init()'s first statement, ahead of the modules and panes it
   *  consults, so every question it asks about them has to be safe to ask
   *  before they exist: during startup the splash is what the drop landed on
   *  and there is no pane behind it yet, so the drop falls off the end of this
   *  handler having been cancelled and nothing else. */
  private installWebviewDropHandlers(): void {
    document.addEventListener('drop', (e: DragEvent) => {
      // Already cancelled in the capture phase, by the guard installed at
      // module load. Repeated because this handler must never be the reason a
      // dropped file replaces the app with a directory listing — not even if
      // the two ever come to be installed in a different order.
      e.preventDefault();
      const dt = e.dataTransfer;
      if (!dt) return;
      // Files belong to the native path, which reports real paths. Both tests
      // are needed: `files` is empty for a dropped directory in some webviews,
      // and `types` is what the drag advertised.
      if (dt.files.length > 0 || dt.types.includes('Files')) return;
      // A dragged link offers its URL as both; plain text is the one thing
      // every source provides, and for a link it is the URL — which is what
      // gets inserted, exactly as iTerm2 does it.
      const text = (dt.getData('text/plain') || dt.getData('text/uri-list')).trim();
      if (!text) return;
      // Same gate as a file drop: the pane under the pointer, nothing if there
      // isn't one, and nothing at all while a dialog owns the window.
      if (this.isModalOpen()) return;
      const target = this.paneAtPoint(e.clientX, e.clientY);
      if (!target?.pane.sessionId) return;
      target.pane.pasteText(text);
    });
  }

  /** True while something is layered over the panes: the palette, a modal, a
   *  wizard, an update/confirmation dialog, or the splash. A drop that lands on
   *  the window now was aimed at what is on top, and the panes are not it.
   *
   *  Every `?.` below is load-bearing, in spite of the `!` on the fields it
   *  reads. The drop handlers are installed as init()'s first act, ahead of the
   *  modules themselves — so a drop that arrives during startup, or after an
   *  init() that threw part way through building them, reaches this with some
   *  of them still undefined. Undefined is "not open": nothing is layered over
   *  panes that do not exist yet. The alternative is a TypeError thrown inside
   *  a drop listener, where nobody would ever see it. */
  private isModalOpen(): boolean {
    return this.palette?.isOpen()
      || this.statusModal?.isOpen()
      || this.askAI?.isOpen()
      || this.historyModal?.isOpen()
      || this.wizard?.isOpen()
      || this.themeWizard?.isOpen()
      // Covers the update prompt, the model download and the confirm dialogs:
      // they are built on demand and all carry this class.
      || document.querySelector('.update-overlay') !== null
      // And the splash, which is the same thing one layer up: an opaque cover
      // over the whole window, still there for its minimum display time after
      // init() has already built the panes behind it. It has to be named
      // explicitly because paneAtPoint() now falls back to plain geometry,
      // which sees through anything. The class arrives when it starts fading —
      // and with it `pointer-events: none`, the app underneath being what the
      // user is now aiming at.
      || document.querySelector('#splash:not(.splash-exit)') !== null;
  }

  /** Insert the dropped paths, shell-escaped, into the pane they landed on. */
  private handleFilesDropped(payload?: FilesDroppedPayload): void {
    let paths = Array.isArray(payload?.paths)
      ? payload.paths.filter(p => typeof p === 'string' && p !== '')
      : [];
    if (paths.length === 0) return;

    // A dialog covers the panes but not the whole window, so a drop that misses
    // it can still hit-test onto a pane behind it — and the splash covers all
    // of it. The user was dropping onto what they could see; nothing goes into
    // a shell they can't.
    if (this.isModalOpen()) {
      logInfo('File drop ignored: a dialog or the splash is over the panes');
      return;
    }

    // This event is subscribed before init() has built anything, so that a drop
    // during the splash is answered rather than left to the webview. Answered,
    // at that point, means saying so and stopping: there is no pane to insert
    // into, and nothing is held back to replay once there is — the user dropped
    // it on a loading screen.
    if (this.panes.length === 0) {
      logInfo('File drop ignored: the app is still starting up');
      return;
    }

    // Whichever pane is under the cursor, not whichever is active: dropping on
    // a background pane is a deliberate act, and it doesn't move the focus.
    //
    // No pane under the point — the tab bar, the status bar, the window's own
    // chrome, the gap between two panes — means there is no pane this was
    // meant for. Falling
    // back to the active one would type the paths into whatever is running
    // there, out of sight of where the user was actually aiming, and into vim
    // or ssh as readily as into a prompt.
    const target = this.paneAtPoint(payload?.x, payload?.y);
    if (!target) {
      logInfo('File drop ignored: it did not land on a pane');
      return;
    }
    // A pane whose shell has exited has nowhere to put them.
    if (!target.pane.sessionId) return;

    // The whole drop goes to the pty in one write, and that write blocks until
    // the foreground program reads it. Hundreds of paths is also past what any
    // command line wants, so the rest are left out — and said out loud, before
    // the paths themselves, rather than silently dropped.
    const total = paths.length;
    if (total > MAX_DROP_PATHS) {
      paths = paths.slice(0, MAX_DROP_PATHS);
      logInfo(`File drop capped at ${MAX_DROP_PATHS} paths; ${total - MAX_DROP_PATHS} skipped`);
      target.pane.writeNotice(
        `[Dropped ${total} items; only the first ${MAX_DROP_PATHS} were inserted]`,
      );
    }

    const escaped = paths.map(p => this.shellEscape(p)).join(' ');
    // Trailing space so the next path — or the rest of the command — doesn't
    // run into this one.
    target.pane.sendText(escaped + ' ');
  }

  /** The pane under a window coordinate, or null when the point isn't on one.
   *  Only the visible tab's panes are in the DOM, which is exactly the set a
   *  drop can land on.
   *
   *  Two passes, because the topmost element at a point is not always the pane
   *  the user was aiming at. Smart-render badges and the panel they open are
   *  children of <body>, `position: fixed` at `z-index: 500` — they have to be,
   *  to sit over a pane whose own box would clip them — so a drop aimed at the
   *  pane behind one hit-tests onto the badge, whose `.closest('.pane-leaf')`
   *  is null, and used to be swallowed. The geometric pass is for exactly that:
   *  the leaf whose rectangle contains the point, whatever is painted on top.
   *
   *  There is deliberately no third pass onto the active pane. A point on the
   *  tab bar, the status bar, the gap between two panes, or off every leaf
   *  belongs to no pane, and typing the paths into whichever shell happens to
   *  be focused — out of sight of where the user aimed, into vim or ssh as
   *  readily as into a prompt — is worse than doing nothing. Anything layered
   *  *over* the panes is out of scope here too, and stays that way: both
   *  callers run the isModalOpen() gate before asking. */
  private paneAtPoint(x?: number, y?: number): PaneInfo | null {
    if (typeof x !== 'number' || typeof y !== 'number') return null;
    const leaf = document.elementFromPoint(x, y)?.closest('.pane-leaf');
    if (leaf) {
      const hit = this.panes.find(p => p.element === leaf);
      if (hit) return hit;
    }
    for (const p of this.panes) {
      const r = p.element.getBoundingClientRect();
      if (x >= r.left && x < r.right && y >= r.top && y < r.bottom) return p;
    }
    return null;
  }

  private destroy(): void {
    if (this.stateSaveInterval) { clearInterval(this.stateSaveInterval); this.stateSaveInterval = null; }
    if (this.updateCheckInterval) { clearInterval(this.updateCheckInterval); this.updateCheckInterval = null; }
    if (this.statsInterval) { clearInterval(this.statsInterval); this.statsInterval = null; }
    if (this.titleRefreshTimer) { clearTimeout(this.titleRefreshTimer); this.titleRefreshTimer = null; }
    document.removeEventListener('visibilitychange', this.onVisibilityChange);
    window.removeEventListener('focus', this.onWindowFocus);
    window.removeEventListener('blur', this.onWindowBlur);
    window.removeEventListener('beforeunload', this.onBeforeUnload);
    this.stateManager.save();
  }

  private shellEscape(s: string): string {
    if (!/[^a-zA-Z0-9@%+=:,./-]/.test(s)) return s;
    return "'" + s.replace(/'/g, "'\\''") + "'";
  }


  private updateInfo: { available: boolean; latestVersion: string; url: string } | null = null;

  private async checkForUpdate(): Promise<void> {
    try {
      const info = await window.go.main.App.CheckForUpdate();
      const wasAvailable = !!this.updateInfo?.available;
      if (info.available) {
        this.updateInfo = { available: true, latestVersion: info.latestVersion, url: info.url };
        this.renderStatusBar();
      } else if (wasAvailable) {
        this.updateInfo = null;
        this.renderStatusBar();
      }
    } catch (_) {
      // Silently ignore — update check is best-effort
    }
  }

  private async pollSystemStats(): Promise<void> {
    try {
      const s = await window.go.main.App.GetSystemStats();
      this.systemStats = {
        cpuPercent: s.cpuPercent,
        memoryUsedMB: s.memoryUsedMB,
        memoryTotalMB: s.memoryTotalMB,
        memoryPercent: s.memoryPercent,
      };
      this.renderStatusBar();
    } catch (_) {
      // Best-effort — leave previous reading in place
    }
  }

  private async ensureModelReady(): Promise<void> {
    try {
      const downloaded = await window.go.main.App.IsModelDownloaded();
      if (downloaded) return;

      const status = document.getElementById('splash-status');
      const bar = document.getElementById('splash-bar');
      if (status) status.textContent = 'Downloading AI model... (Esc to skip)';
      if (bar) {
        bar.classList.add('downloading');
        bar.style.width = '0%';
      }

      // Allow Escape to cancel the download
      let skipped = false;
      const onKey = (e: KeyboardEvent) => {
        if (e.key === 'Escape') {
          skipped = true;
          window.go.main.App.SkipDownload();
          if (status) status.textContent = 'Skipping download...';
        }
      };
      document.addEventListener('keydown', onKey, true);

      // Listen for download progress events
      const unsub = window.runtime.EventsOn('model:download-progress', (data: { downloaded: number; total: number }) => {
        if (data.total > 0 && !skipped) {
          const pct = Math.round((data.downloaded / data.total) * 100);
          const mbDown = (data.downloaded / 1024 / 1024).toFixed(1);
          const mbTotal = (data.total / 1024 / 1024).toFixed(1);
          if (status) status.textContent = `Downloading AI model... ${mbDown} / ${mbTotal} MB (${pct}%) — Esc to skip`;
          if (bar) bar.style.width = `${pct}%`;
        }
      });

      try {
        await window.go.main.App.DownloadModel();
        if (!skipped && status) status.textContent = 'Model ready';
        if (!skipped && bar) bar.style.width = '100%';
      } catch {
        if (skipped) {
          if (status) status.textContent = 'Download skipped — use Cmd+K to retry later';
        } else {
          if (status) status.textContent = 'Download failed — use Cmd+K to retry later';
        }
      } finally {
        unsub();
        document.removeEventListener('keydown', onKey, true);
      }

    } catch (e) {
      logError('AI model download check failed during startup', e);
    }
  }

  private dismissSplash(): void {
    const splash = document.getElementById('splash');
    if (!splash) return;
    const minDisplayMs = 2000;
    const remaining = Math.max(0, minDisplayMs - performance.now());
    setTimeout(() => {
      splash.classList.add('splash-exit');
      setTimeout(() => splash.remove(), 600);
    }, remaining);
  }

  // --- Tab Management ---

  private async createTab(name?: string): Promise<void> {
    // Not a silent no-op: nine is the limit because Cmd+1..9 is how tabs are
    // reached, and a "+" that does nothing is a bug from where the user sits.
    if (this.tabs.length >= MAX_TABS) {
      this.notice(`[Maximum ${MAX_TABS} tabs]`);
      return;
    }
    let tabName = name;
    if (!tabName) {
      const used = new Set(this.tabs.map(t => t.name));
      let n = 1;
      while (used.has(`Terminalo ${n}`)) n++;
      tabName = `Terminalo ${n}`;
    }
    const tab: Tab = {
      id: generateId('tab'),
      name: tabName,
      renamed: false,
      attention: false,
      panes: [],
      activeIndex: 0,
      layoutRoot: null,
    };
    this.tabs.push(tab);
    this.activeTabIndex = this.tabs.length - 1;

    const pane = await this.createPaneForTab(tab);
    tab.layoutRoot = { type: 'leaf', paneInfo: pane };

    this.renderTabBar();
    this.renderLayout();
    await waitForLayout();
    pane.pane.fit();
    // This tab is now the visible one, so its pane takes a GPU context and the
    // tab that just went off screen gives its own back.
    this.syncWebgl();
    // A shell that won't start leaves a pane that says so and retries on Enter,
    // rather than rejecting out of here — this also runs during init().
    try {
      await pane.pane.connect();
    } catch (e) {
      logError(`Failed to start shell for new tab "${tabName}"`, e);
      pane.pane.showStartFailure(e);
    }
    this.setActive(0);
    this.stateManager.save();
  }

  private closeTab(index: number): void {
    if (this.tabs.length <= 1) return;

    const wasVisible = index === this.activeTabIndex;
    const tab = this.tabs[index];
    // Before anything is disposed: the snapshot is built from the panes.
    this.snapshotClosedTab(tab);
    if (this.findBar && tab.panes.some(p => p.pane === this.findBar!.pane)) this.closeFind();
    for (const p of tab.panes) {
      p.pane.dispose();
    }
    this.tabs.splice(index, 1);

    // Closing a tab to the left of the active one shifts it down. Without this
    // the window would land on a different tab than the user was working in —
    // reachable from the tab bar's × and now from a background pane exiting.
    if (index < this.activeTabIndex) this.activeTabIndex--;
    if (this.activeTabIndex >= this.tabs.length) {
      this.activeTabIndex = this.tabs.length - 1;
    }

    if (wasVisible) {
      // The tab on screen is the one that went: the window has to be redrawn
      // around whatever took its place.
      this.switchToTab(this.activeTabIndex);
    } else {
      // A background tab closing — the tab bar's × on another tab, or a shell
      // there exiting on its own — must leave the visible tab alone. A
      // renderLayout() would tear its DOM down and rebuild it, dropping the
      // user's selection, and setActive() would yank focus back out of
      // whatever they were doing.
      this.renderTabBar();
    }
    this.stateManager.save();
  }

  private switchToTab(index: number): void {
    if (index < 0 || index >= this.tabs.length) return;
    // The bar belongs to a pane that is about to leave the screen.
    this.closeFind();
    const outgoing = this.tab;
    this.activeTabIndex = index;
    // Whatever rang while this tab was in the background has now been seen.
    this.tab.attention = false;
    // Release the outgoing tab's GPU contexts *before* the incoming tab asks
    // for its own. Browsers cap how many WebGL contexts are alive at once and
    // silently drop the oldest to make room; overlapping the two tabs would
    // spend that budget blanking a pane the user is about to look at.
    if (outgoing && outgoing !== this.tab) {
      for (const p of outgoing.panes) p.pane.disableWebgl();
    }
    this.renderTabBar();
    this.renderLayout();
    requestAnimationFrame(() => {
      // Font options set while this tab was off screen were held back — xterm
      // cannot measure a cell box in a detached element. Now it can, and the
      // fit below has to run against the right one.
      for (const p of this.panes) p.pane.flushOptions();
      this.fitAll();
      // After the fit: the renderer sizes its canvas from the terminal's
      // dimensions, so a context taken before the panes have their final size
      // would only have to be resized again.
      this.syncWebgl();
      if (this.panes.length > 0) {
        this.setActive(this.activeIndex);
      }
    });
  }

  /** Only the tab on screen holds WebGL contexts. Called after every render
   *  that changes which panes are visible; both halves are idempotent, so it
   *  is cheap to call whenever in doubt. */
  private syncWebgl(): void {
    for (const tab of this.tabs) {
      const visible = tab === this.tab;
      for (const p of tab.panes) {
        if (visible) p.pane.enableWebgl();
        else p.pane.disableWebgl();
      }
    }
  }

  private async renameTab(index: number, newName: string): Promise<void> {
    const tab = this.tabs[index];
    const trimmed = newName.trim();
    if (!tab || !trimmed) return;
    // Confirming the name that was already on screen is not a rename. The
    // field is pre-filled with whatever the tab is currently called, so a
    // double-click and a click away would otherwise pin that name for good and
    // cut the tab off from the shell that was keeping it up to date.
    if (!tab.renamed && trimmed === this.tabDisplayName(tab, index)) {
      this.renderTabBar();
      return;
    }
    tab.name = trimmed;
    // From now on this tab is called what the user called it, whatever the
    // shell has to say about its own title or its directory.
    tab.renamed = true;
    this.renderTabBar();
    await this.stateManager.save();
  }

  /** What a tab is called on screen.
   *
   *  A name the user typed wins outright — they said what this tab is, and no
   *  amount of `cd` changes that. Otherwise the active pane's own OSC 0/2 title
   *  is the best answer, because a program that sets one (ssh, vim, a build) is
   *  saying what it is doing. Failing that, the folder it is sitting in, which
   *  is what most prompts would have shown anyway. And failing even that, the
   *  placeholder the tab was born with. */
  private tabDisplayName(tab: Tab, index: number): string {
    if (tab.renamed && tab.name.trim()) return tab.name;
    const active = tab.panes[tab.activeIndex] || tab.panes[0];
    const title = active?.pane.title.trim();
    if (title) return title;
    const cwd = basename(active?.pane.cwd || '');
    if (cwd) return cwd;
    return tab.name || `Terminalo ${index + 1}`;
  }

  /** Coalesce the tab-bar rebuilds a burst of title/cwd reports would cause: a
   *  prompt redraw sends both, and a `cd` in a loop sends one per iteration. */
  private scheduleTitleRefresh(): void {
    // Never while a tab is being renamed: rebuilding the bar would replace the
    // input the user is typing into, mid-word.
    if (this.renamingTabIndex >= 0 || this.titleRefreshTimer) return;
    this.titleRefreshTimer = setTimeout(() => {
      this.titleRefreshTimer = null;
      if (this.renamingTabIndex >= 0) return;
      this.renderTabBar();
    }, TITLE_REFRESH_DEBOUNCE_MS);
  }

  /** Keep the window's title in step with the visible tab. The native title is
   *  hidden behind the app's own titlebar, but it is what the Window menu and
   *  the app switcher read, so both get the same string. */
  private updateWindowTitle(): void {
    const tab = this.tab;
    const name = tab ? this.tabDisplayName(tab, this.activeTabIndex).trim() : '';
    const title = name ? `${name} — El Terminalo` : 'El Terminalo';
    const bar = document.getElementById('titlebar');
    if (bar) bar.textContent = title;
    if (title === this.lastWindowTitle) return;
    this.lastWindowTitle = title;
    window.go.main.App.SetWindowTitle(title).catch(() => {
      // Cosmetic, and the titlebar above already says it.
    });
  }

  private renderTabBar(): void {
    const tabs = this.tabs.map((t, i) => {
      const isActive = i === this.activeTabIndex;
      const label = this.tabDisplayName(t, i);
      if (this.renamingTabIndex === i) {
        return `<div class="tab-item active">
          <input class="tab-rename-input" type="text" value="${escHtml(label)}" data-index="${i}" />
        </div>`;
      }
      return `<div class="tab-item ${isActive ? 'active' : ''}" data-index="${i}" draggable="true">
        <span class="tab-shortcut">${i + 1}</span>
        <span class="tab-name">${escHtml(label)}</span>
        ${t.attention ? '<span class="tab-attention" title="Bell"></span>' : ''}
        ${this.tabs.length > 1 ? `<span class="tab-close" data-close="${i}">×</span>` : ''}
      </div>`;
    }).join('');

    this.tabBar.innerHTML = `
      <div class="tab-list">${tabs}</div>
      <div class="tab-new" title="${CMD.NEW_TAB.name} (${CMD.NEW_TAB.shortcut})">+</div>
    `;

    // Wire tab clicks — delay single click to detect double click
    this.tabBar.querySelectorAll('.tab-item[data-index]').forEach(el => {
      const idx = parseInt(el.getAttribute('data-index') || '0');
      let clickTimer: ReturnType<typeof setTimeout> | null = null;

      el.addEventListener('click', (e) => {
        if ((e.target as HTMLElement).classList.contains('tab-close')) return;
        if (clickTimer) { clearTimeout(clickTimer); clickTimer = null; return; }
        clickTimer = setTimeout(() => {
          clickTimer = null;
          this.switchToTab(idx);
        }, DOUBLE_CLICK_DELAY_MS);
      });

      el.addEventListener('dblclick', (e) => {
        e.preventDefault();
        if (clickTimer) { clearTimeout(clickTimer); clickTimer = null; }
        this.renamingTabIndex = idx;
        this.renderTabBar();
        requestAnimationFrame(() => {
          const input = this.tabBar.querySelector('.tab-rename-input') as HTMLInputElement;
          if (input) {
            input.focus();
            input.select();
            input.addEventListener('blur', () => {
              this.renameTab(idx, input.value);
              this.renamingTabIndex = -1;
              this.renderTabBar();
            });
            input.addEventListener('keydown', (ev: KeyboardEvent) => {
              ev.stopPropagation();
              if (ev.key === 'Enter') { ev.preventDefault(); input.blur(); }
              if (ev.key === 'Escape') { ev.preventDefault(); this.renamingTabIndex = -1; this.renderTabBar(); }
            });
          }
        });
      });
    });

    // Wire close buttons
    this.tabBar.querySelectorAll('.tab-close').forEach(el => {
      el.addEventListener('click', (e) => {
        e.stopPropagation();
        const idx = parseInt(el.getAttribute('data-close') || '0');
        this.confirmCloseTab(idx);
      });
    });

    // Wire new tab button
    this.tabBar.querySelector('.tab-new')?.addEventListener('click', () => {
      this.createTab();
    });

    this.wireTabDragging();
    this.updateWindowTitle();
  }

  /** HTML5 drag to reorder tabs.
   *
   *  The drag carries a private MIME type, which is how the app tells its own
   *  gesture apart from a file or a selection dropped in from outside: the
   *  document-level guards installed at module load — the ones that stop a
   *  dropped folder from navigating the whole window away — check for it and
   *  stand aside. */
  private wireTabDragging(): void {
    const items = Array.from(this.tabBar.querySelectorAll('.tab-item[data-index]')) as HTMLElement[];
    for (const el of items) {
      const idx = parseInt(el.getAttribute('data-index') || '0', 10);

      el.addEventListener('dragstart', (ev) => {
        const e = ev as DragEvent;
        if (!e.dataTransfer) return;
        this.draggingTabIndex = idx;
        e.dataTransfer.effectAllowed = 'move';
        // The index travels in the drag itself as well as in the field above:
        // the field is what the hover feedback reads, the payload is what the
        // drop trusts.
        e.dataTransfer.setData(TAB_DRAG_MIME, String(idx));
        el.classList.add('tab-dragging');
      });

      el.addEventListener('dragend', () => {
        this.draggingTabIndex = -1;
        el.classList.remove('tab-dragging');
        this.clearTabDropMarkers();
      });

      el.addEventListener('dragover', (ev) => {
        const e = ev as DragEvent;
        if (!isTabDrag(e)) return;
        // Cancelling dragover is what makes this a drop target at all.
        e.preventDefault();
        e.stopPropagation();
        if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
        if (idx === this.draggingTabIndex) return;
        this.clearTabDropMarkers();
        el.classList.add(this.draggingTabIndex >= 0 && this.draggingTabIndex < idx
          ? 'tab-drop-after' : 'tab-drop-before');
      });

      el.addEventListener('dragleave', () => {
        el.classList.remove('tab-drop-before', 'tab-drop-after');
      });

      el.addEventListener('drop', (ev) => {
        const e = ev as DragEvent;
        if (!isTabDrag(e)) return;
        e.preventDefault();
        e.stopPropagation();
        const from = parseInt(e.dataTransfer?.getData(TAB_DRAG_MIME) || '', 10);
        this.draggingTabIndex = -1;
        this.clearTabDropMarkers();
        if (Number.isNaN(from)) return;
        this.moveTab(from, idx);
      });
    }
  }

  private clearTabDropMarkers(): void {
    this.tabBar.querySelectorAll('.tab-drop-before, .tab-drop-after').forEach(el => {
      el.classList.remove('tab-drop-before', 'tab-drop-after');
    });
  }

  /** Move a tab within the bar. The tab that was on screen stays on screen,
   *  wherever the move left it — including when it is the one being moved. */
  private moveTab(from: number, to: number): void {
    if (from === to) return;
    if (from < 0 || from >= this.tabs.length || to < 0 || to >= this.tabs.length) return;
    const visible = this.tabs[this.activeTabIndex];
    const [moved] = this.tabs.splice(from, 1);
    this.tabs.splice(to, 0, moved);
    this.activeTabIndex = Math.max(0, this.tabs.indexOf(visible));
    // Only the bar changes: the panes on screen belong to the same tab they
    // did a moment ago, so their DOM is left alone.
    this.renderTabBar();
    this.stateManager.save();
  }

  private cycleTab(delta: number): void {
    const n = this.tabs.length;
    if (n <= 1) return;
    this.switchToTab(((this.activeTabIndex + delta) % n + n) % n);
  }

  /** Remember a tab on its way out, so Cmd+Shift+T can bring it back.
   *
   *  Called from closeTab() *before* the panes are disposed, and finished before
   *  it returns: the snapshot is built from each pane's cached directory, with
   *  no binding call to wait for. It used to be pushed from a `.then()` on
   *  serializeLayout(), which probes the backend for a shell without
   *  integration — and a Cmd+W followed straight away by Cmd+Shift+T then popped
   *  a stack the snapshot had not reached yet, so the tab the user had just
   *  closed was simply not there. */
  private snapshotClosedTab(tab: Tab): void {
    if (!tab.layoutRoot) return;
    this.closedTabs.push({
      name: tab.name,
      renamed: tab.renamed,
      layout: this.stateManager.serializeLayoutSync(tab.layoutRoot),
    });
    // Oldest first out: the point of the stack is the last few minutes.
    while (this.closedTabs.length > MAX_CLOSED_TABS) this.closedTabs.shift();
  }

  /** Cmd+Shift+T: rebuild the tab that was closed last, at the end of the bar.
   *
   *  Runs under the same `restoring` guard a launch-time restore does: the
   *  layout is half-built for as long as its panes are connecting, and a save
   *  taken in the middle of that would write a window that never existed. One
   *  save happens at the end, when it is whole. */
  private async reopenClosedTab(): Promise<void> {
    // Not while another rebuild is in flight — the launch-time restore, or a
    // second Cmd+Shift+T on top of this one. Both would be writing to the same
    // guard and to activeTabIndex at the same time.
    if (this.restoring) return;
    if (this.tabs.length >= MAX_TABS) {
      this.notice(`[Maximum ${MAX_TABS} tabs]`);
      return;
    }
    // Peeked, not popped: a rebuild that fails must leave the entry where it
    // was, so the next Cmd+Shift+T tries the same tab again rather than skipping
    // past it to an older one — or, on an empty stack, to nothing at all.
    const saved = this.closedTabs[this.closedTabs.length - 1];
    if (!saved) return;

    const tab: Tab = {
      id: generateId('tab'),
      name: saved.name,
      renamed: !!saved.renamed,
      attention: false,
      panes: [],
      activeIndex: 0,
      layoutRoot: null,
    };
    this.tabs.push(tab);
    this.activeTabIndex = this.tabs.length - 1;

    this.restoring = true;
    try {
      tab.layoutRoot = await this.restoreLayoutNode(saved.layout, tab);
    } catch (e) {
      logError(`Failed to reopen the closed tab "${saved.name}"`, e);
    } finally {
      this.restoring = false;
    }

    if (!tab.layoutRoot) {
      // Nothing came back — don't leave an empty tab behind, and leave the
      // snapshot on the stack: the tab was not reopened, so it is still closed.
      for (const p of tab.panes) p.pane.dispose();
      const idx = this.tabs.indexOf(tab);
      if (idx >= 0) this.tabs.splice(idx, 1);
      this.activeTabIndex = Math.min(this.activeTabIndex, this.tabs.length - 1);
      this.switchToTab(this.activeTabIndex);
      return;
    }

    // Rebuilt: only now does the entry leave the stack. By identity rather than
    // by popping — a background shell exiting during the rebuild closes its own
    // tab and pushes a newer snapshot on top of this one.
    const used = this.closedTabs.lastIndexOf(saved);
    if (used >= 0) this.closedTabs.splice(used, 1);

    this.renderTabBar();
    this.renderLayout();
    await waitForLayout();
    this.fitAll();
    this.syncWebgl();
    this.setActive(0);
    this.stateManager.save();
  }

  // --- State Persistence ---

  private async restoreState(): Promise<boolean> {
    const state = await this.stateManager.load();
    if (!state) return false;

    // Nothing may be saved until the window is whole again.
    this.restoring = true;
    try {
      if (!state.tabs && state.layout) {
        // v1 migration: single layout -> single tab
        applyThemeToCSS(this.currentTheme);
        const tab: Tab = { id: 'tab-migrated', name: 'Terminal', renamed: false, attention: false, panes: [], activeIndex: 0, layoutRoot: null };
        this.tabs.push(tab);
        this.activeTabIndex = 0;
        tab.layoutRoot = await this.restoreLayoutNode(state.layout, tab);
        this.renderTabBar();
        this.renderLayout();
        await waitForLayout();
        for (const p of tab.panes) p.pane.fit();
        this.syncWebgl();
        return true;
      }

      if (!state.tabs || state.tabs.length === 0) return false;

      const theme = this.themes.find(t => t.name === state.themeName);
      if (theme) this.currentTheme = theme;
      applyThemeToCSS(this.currentTheme);

      for (const savedTab of state.tabs) {
        const tab: Tab = {
          id: generateId('tab'),
          name: savedTab.name,
          // Absent in state written before this existed — and rightly false
          // there, since those tabs were never renamed by hand.
          renamed: !!savedTab.renamed,
          attention: false,
          panes: [],
          activeIndex: 0,
          layoutRoot: null,
        };
        this.tabs.push(tab);
        // Per-leaf failures are absorbed below, so one unreadable directory
        // can't take the rest of the layout down with it.
        tab.layoutRoot = await this.restoreLayoutNode(savedTab.layout, tab);
      }

      this.activeTabIndex = Math.min(state.activeTabIndex || 0, this.tabs.length - 1);
      this.renderTabBar();
      this.renderLayout();
      await waitForLayout();
      for (const p of this.tab.panes) p.pane.fit();
      // Every restored tab has its panes, but only the active one is on screen
      // and only its panes may hold a GPU context — a seventeen-pane layout
      // would otherwise ask for seventeen and lose most of them.
      this.syncWebgl();

      // Deliberately no save here: nothing changed from the user's point of
      // view, and the 30s autosave starts once init() is past this.
      return true;
    } catch (e) {
      logError('Failed to restore the saved window layout', e);
      // Leave nothing half-built behind — init() falls back to a fresh tab.
      for (const t of this.tabs) for (const p of t.panes) p.pane.dispose();
      this.tabs = [];
      this.activeTabIndex = 0;
      // The file on disk is structurally unusable. Move it aside *before*
      // returning: the fresh tab init() is about to build saves itself, and
      // that save must land on a new file rather than overwrite the layout the
      // user actually had. Best-effort — an unquarantined file is still better
      // than refusing to start.
      try {
        this.quarantinedStatePath = await window.go.main.App.QuarantineAppState();
      } catch (qe) {
        logError('Failed to quarantine the unreadable state file', qe);
      }
      return false;
    } finally {
      this.restoring = false;
    }
  }

  /** Say, in the fallback tab's terminal, that the old layout was set aside —
   *  a silent reset looks like the app forgot everything. */
  private reportQuarantinedState(): void {
    const path = this.quarantinedStatePath;
    this.quarantinedStatePath = '';
    if (!path) return;
    const pane = this.panes[this.activeIndex] || this.panes[0];
    pane?.pane.writeNotice(`[Previous layout could not be restored; it was saved as ${path}]`);
  }

  private async restoreLayoutNode(saved: SavedSplitNode | undefined, tab: Tab): Promise<SplitNode> {
    if (saved?.type === 'split' && saved.children) {
      const [first, second] = await Promise.all([
        this.restoreLayoutNode(saved.children[0], tab),
        this.restoreLayoutNode(saved.children[1], tab),
      ]);
      return { type: 'split', direction: saved.direction, ratio: saved.ratio ?? DEFAULT_SPLIT_RATIO, children: [first, second] };
    }
    // A leaf — and anything malformed, including a node that isn't there at all
    // — is one pane, so one bad entry costs a split, not the whole restore. A
    // shell that refuses to start still gets its pane: it keeps the saved
    // directory (so the next save persists the folder, not ""), says why, and
    // Enter retries.
    const pane = await this.createPaneForTab(tab);
    try {
      await pane.pane.connect(saved?.cwd || '');
    } catch (e) {
      logError(`Failed to start shell for restored pane in ${saved?.cwd || 'the default directory'}`, e);
      pane.pane.showStartFailure(e);
    }
    return { type: 'leaf', paneInfo: pane };
  }

  // --- Pane Management ---

  private async createPaneForTab(tab: Tab): Promise<PaneInfo> {
    const el = document.createElement('div');
    el.className = 'pane-leaf';
    el.style.flex = '1';

    // A new pane is never the active one yet — setActive() turns its cursor on
    // a moment later — so it is built with the blink off.
    const pane = new TerminalPane(el, this.currentTheme.xterm, this.paneOptions(false));
    const id = generateId('pane');
    const info: PaneInfo = { id, pane, element: el };

    pane.setContextActions({
      splitVertical: () => this.splitPane('vertical'),
      splitHorizontal: () => this.splitPane('horizontal'),
      closePane: () => this.confirmCloseActivePane(),
    });

    pane.onExit = (exit) => this.handlePaneExit(info, exit);

    pane.onBell = () => this.handleBell(info, tab);

    // OSC 0/2 titles and OSC 7 directories both feed the tab's name, and both
    // arrive on every prompt.
    pane.onTitleChange = () => {
      if (tab === this.tab && tab.panes[tab.activeIndex] === info) this.scheduleTitleRefresh();
    };

    pane.smartRender.onBadgesChanged = () => this.renderStatusBar();

    // Record commands in history database
    pane.shellIntegration.onCommandFinishedAdd(async (block, exitCode) => {
      if (!block.commandText || !pane.sessionId) return;
      const cmd = block.commandText.trim();
      if (this.aiPrompts.has(cmd)) {
        this.aiPrompts.delete(cmd);
        return;
      }
      try {
        const cwd = await pane.getCWD();
        await window.go.main.App.RecordCommand(cmd, cwd, exitCode, pane.sessionId);
      } catch { /* best-effort */ }
    });

    el.addEventListener('mousedown', (e) => {
      if (e.button === 2) return; // don't steal focus on right-click
      // The find bar lives inside the pane; clicking its field or its buttons
      // must not hand the keyboard straight back to the terminal.
      if ((e.target as HTMLElement | null)?.closest('.find-bar')) return;
      const idx = tab.panes.indexOf(info);
      if (idx >= 0 && tab === this.tab) this.setActive(idx);
    });

    tab.panes.push(info);
    return info;
  }

  private async createPane(): Promise<PaneInfo> {
    return this.createPaneForTab(this.tab);
  }

  private findParent(node: SplitNode, paneId: string): { parent: SplitNode; childIndex: 0 | 1 } | null {
    if (node.type !== 'split' || !node.children) return null;
    for (let i = 0; i < 2; i++) {
      const child = node.children[i as 0 | 1];
      if (child.type === 'leaf' && child.paneInfo?.id === paneId) {
        return { parent: node, childIndex: i as 0 | 1 };
      }
      const found = this.findParent(child, paneId);
      if (found) return found;
    }
    return null;
  }

  private async splitPane(direction: 'vertical' | 'horizontal'): Promise<void> {
    if (!this.layoutRoot) return;
    const activePane = this.panes[this.activeIndex];
    if (!activePane) return;

    const newPane = await this.createPane();
    const replaceInTree = (node: SplitNode): SplitNode => {
      if (node.type === 'leaf' && node.paneInfo?.id === activePane.id) {
        return { type: 'split', direction, ratio: DEFAULT_SPLIT_RATIO, children: [{ type: 'leaf', paneInfo: activePane }, { type: 'leaf', paneInfo: newPane }] };
      }
      if (node.type === 'split' && node.children) {
        return { ...node, children: [replaceInTree(node.children[0]), replaceInTree(node.children[1])] };
      }
      return node;
    };

    this.layoutRoot = replaceInTree(this.layoutRoot);
    this.renderLayout();
    await waitForLayout();
    this.fitAll();
    // The split added a visible pane, which needs its own context.
    this.syncWebgl();
    try {
      await newPane.pane.connect();
    } catch (e) {
      logError(`Failed to start shell for new pane ${newPane.id}`, e);
      newPane.pane.showStartFailure(e);
    }
    this.setActive(this.panes.indexOf(newPane));
    this.stateManager.save();
  }

  private showConfirmDialog(title: string, body: string, confirmLabel: string, onConfirm: () => void): void {
    if (document.querySelector('.confirm-overlay')) return;
    const overlay = document.createElement('div');
    overlay.className = 'update-overlay confirm-overlay';
    overlay.innerHTML = `<div class="update-dialog">
      <div class="update-dialog-title">${title}</div>
      <div class="update-dialog-body">${body}</div>
      <div class="update-dialog-actions">
        <button class="theme-btn theme-btn-cancel" id="confirm-cancel">Cancel</button>
        <button class="theme-btn theme-btn-save" id="confirm-ok" style="background:#f85149;border-color:#f85149;">${confirmLabel}</button>
      </div>
    </div>`;
    document.body.appendChild(overlay);
    const dismiss = () => { overlay.remove(); document.removeEventListener('keydown', onKey, true); };
    const confirm = () => { dismiss(); onConfirm(); };
    document.getElementById('confirm-cancel')?.addEventListener('click', dismiss);
    document.getElementById('confirm-ok')?.addEventListener('click', confirm);
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') dismiss();
      if (e.key === 'Enter') confirm();
      e.stopPropagation();
    };
    document.addEventListener('keydown', onKey, true);
  }

  private confirmCloseActivePane(): void {
    if (this.panes.length <= 1 || !this.layoutRoot) return;
    // The pane the user is being asked about, not "whichever is active when
    // they answer": a shell exiting anywhere while the dialog is up can close
    // panes and tabs on its own, moving the active one out from under it.
    const target = this.panes[this.activeIndex];
    if (!target) return;
    this.showConfirmDialog('Close Pane?', 'The active pane and its session will be closed.', 'Close', () => {
      const tab = this.tabs.find(t => t.panes.includes(target));
      // Gone already — its own shell exited while the dialog was open. And a
      // pane that has since become its tab's last one can only go by taking
      // the tab with it, which is more than this dialog asked for.
      if (!tab || tab.panes.length <= 1) return;
      this.closePane(target);
    });
  }

  private confirmCloseTab(index: number): void {
    if (this.tabs.length <= 1) return;
    // Same reason, one level up: a background pane's shell exiting closes its
    // tab, so by the time the user confirms, this index can name a different
    // tab — or none at all. Re-resolve the captured tab instead.
    const tab = this.tabs[index];
    if (!tab) return;
    const paneCount = tab.panes.length;
    this.showConfirmDialog('Close Tab?', `${paneCount} pane${paneCount !== 1 ? 's' : ''} in this tab will be closed.`, 'Close', () => {
      const idx = this.tabs.indexOf(tab);
      if (idx < 0) return;
      this.closeTab(idx);
    });
  }

  /** Close one pane, wherever it lives — the active tab or a background one.
   *  Returns false when the pane can't go: the app must always keep at least
   *  one pane, so the last pane of the last tab stays put. */
  private closePane(target: PaneInfo): boolean {
    const tabIndex = this.tabs.findIndex(t => t.panes.includes(target));
    if (tabIndex < 0) return false;
    const tab = this.tabs[tabIndex];

    // A pane that is its whole tab can only go by taking the tab with it.
    if (tab.panes.length <= 1) {
      if (this.tabs.length <= 1) return false;
      this.closeTab(tabIndex);
      return true;
    }

    if (!tab.layoutRoot) return false;
    const result = this.findParent(tab.layoutRoot, target.id);
    // Without a parent the tree and the pane list disagree; removing one side
    // only would leave the layout pointing at a disposed pane.
    if (!result) return false;

    const sibling = result.parent.children![result.childIndex === 0 ? 1 : 0];
    result.parent.type = sibling.type;
    result.parent.direction = sibling.direction;
    result.parent.ratio = sibling.ratio;
    result.parent.paneInfo = sibling.paneInfo;
    result.parent.children = sibling.children;

    const removedIdx = tab.panes.indexOf(target);
    tab.panes.splice(removedIdx, 1);
    if (this.findBar?.pane === target.pane) this.closeFind();
    target.pane.dispose();

    // Keep the focus where it was: everything after the hole shifts down one.
    if (removedIdx < tab.activeIndex) tab.activeIndex--;
    if (tab.activeIndex >= tab.panes.length) tab.activeIndex = tab.panes.length - 1;
    if (tab.activeIndex < 0) tab.activeIndex = 0;

    // Background tabs aren't in the DOM; switchToTab redraws them on return.
    if (tabIndex === this.activeTabIndex) {
      this.renderLayout();
      requestAnimationFrame(() => this.fitAll());
      this.setActive(tab.activeIndex);
    }
    this.stateManager.save();
    return true;
  }

  /** A pane's shell exited. A clean `exit` means the user is done with the pane,
   *  so it goes; anything else is a failure they should see, so the pane stays
   *  with the reason on screen and Enter to restart it. */
  private handlePaneExit(info: PaneInfo, exit: PtyExitPayload): void {
    const clean = exit.exitCode === 0 && exit.signal === '';
    // Mid-restore the layout isn't assembled yet, so nothing may be
    // restructured — the pane just says what happened and waits for Enter.
    if (clean && !this.restoring && this.closePane(info)) return;
    info.pane.showExitNotice(exit);
  }

  private renderLayout(): void {
    this.container.innerHTML = '';
    if (!this.layoutRoot) return;
    this.container.appendChild(this.renderNode(this.layoutRoot));
  }

  private renderNode(node: SplitNode): HTMLElement {
    if (node.type === 'leaf' && node.paneInfo) {
      node.paneInfo.element.style.flex = '1';
      return node.paneInfo.element;
    }
    if (node.type === 'split' && node.children) {
      const isVertical = node.direction === 'vertical';
      const ratio = node.ratio ?? DEFAULT_SPLIT_RATIO;
      const wrapper = document.createElement('div');
      wrapper.className = isVertical ? 'pane-split-vertical' : 'pane-split-horizontal';
      wrapper.style.flex = '1';
      const first = this.renderNode(node.children[0]);
      const second = this.renderNode(node.children[1]);
      first.style.flex = `${ratio}`;
      second.style.flex = `${1 - ratio}`;
      const divider = document.createElement('div');
      divider.className = isVertical ? 'pane-divider-v' : 'pane-divider-h';
      divider.addEventListener('mousedown', (e) => {
        e.preventDefault();
        const startPos = isVertical ? e.clientX : e.clientY;
        const wrapperRect = wrapper.getBoundingClientRect();
        const totalSize = isVertical ? wrapperRect.width : wrapperRect.height;
        const startRatio = ratio;
        const onMouseMove = (ev: MouseEvent) => {
          const delta = ((isVertical ? ev.clientX : ev.clientY) - startPos) / totalSize;
          const newRatio = Math.max(MIN_SPLIT_RATIO, Math.min(MAX_SPLIT_RATIO, startRatio + delta));
          node.ratio = newRatio;
          first.style.flex = `${newRatio}`;
          second.style.flex = `${1 - newRatio}`;
        };
        const onMouseUp = () => {
          document.removeEventListener('mousemove', onMouseMove);
          document.removeEventListener('mouseup', onMouseUp);
          document.body.style.cursor = '';
          document.body.style.userSelect = '';
          this.fitAll();
          this.stateManager.save();
        };
        document.body.style.cursor = isVertical ? 'col-resize' : 'row-resize';
        document.body.style.userSelect = 'none';
        document.addEventListener('mousemove', onMouseMove);
        document.addEventListener('mouseup', onMouseUp);
      });
      wrapper.appendChild(first);
      wrapper.appendChild(divider);
      wrapper.appendChild(second);
      return wrapper;
    }
    return document.createElement('div');
  }

  private fitAll(): void {
    for (const p of this.panes) p.pane.fit();
  }

  private setActive(index: number): void {
    if (index < 0 || index >= this.panes.length) return;
    // One find bar, on the pane the user is in. Moving out of that pane closes
    // it rather than leaving a search field hanging over a terminal nobody is
    // typing into.
    if (this.findBar && this.findBar.pane !== this.panes[index].pane) this.closeFind();
    for (const p of this.panes) {
      p.element.classList.remove('active');
      p.pane.terminal.options.cursorBlink = false;
    }
    this.activeIndex = index;
    this.panes[index].element.classList.add('active');
    this.panes[index].pane.terminal.options.cursorBlink = this.settings.cursorBlink;
    this.panes[index].pane.focus();
    this.renderStatusBar();
    // The tab is named after this pane, and it just changed which pane that is.
    this.scheduleTitleRefresh();
  }

  // --- Theme ---

  private setTheme(name: string): void {
    const theme = this.themes.find(t => t.name.toLowerCase() === name.toLowerCase());
    if (!theme) return;
    this.currentTheme = theme;
    applyThemeToCSS(theme);
    for (const tab of this.tabs) {
      for (const p of tab.panes) p.pane.setTheme(theme.xterm);
    }
    // The find bar reads its highlight colours from the CSS variables when it
    // opens, and the addon bakes them into decorations that are already drawn —
    // so a bar left open across a theme switch keeps the old theme's highlights
    // until it is told to build them again.
    this.findBar?.refreshTheme();
    this.renderStatusBar();
    this.stateManager.save();
  }

  /** Re-read the themes on disk, and land on `selectName` when one is asked
   *  for. Shared by the theme wizard, the `themes:changed` event and importing
   *  a color scheme — all three change the same list under the same window. */
  private async refreshThemes(selectName?: string): Promise<void> {
    const themeDTOs = await window.go.main.App.GetThemes();
    this.themes = themeDTOs.map(themeFromDTO);
    if (selectName && this.themes.some(t => t.name.toLowerCase() === selectName.toLowerCase())) {
      this.setTheme(selectName);
      return;
    }
    // No new theme to switch to — but the current one may itself have been
    // rewritten, so it is re-applied rather than left as the stale copy this
    // window is holding.
    const updated = this.themes.find(t => t.name === this.currentTheme?.name);
    if (!updated) return;
    this.currentTheme = updated;
    applyThemeToCSS(updated);
    for (const tab of this.tabs) {
      for (const p of tab.panes) p.pane.setTheme(updated.xterm);
    }
  }

  /** Ask the backend to import a color scheme file. A cancelled dialog returns
   *  "" and is not an error; a file that cannot be parsed rejects, and that is
   *  said in the pane rather than only in the log.
   *
   *  A successful import needs nothing more from here: writing the theme makes
   *  the Go side emit `themes:changed` with the new name, and the handler for
   *  that calls refreshThemes(name) — the same call this used to make itself,
   *  one GetThemes round trip and one whole-window re-theme earlier. */
  private async importColorScheme(): Promise<void> {
    try {
      await window.go.main.App.ImportColorScheme();
    } catch (e) {
      logError('Failed to import a color scheme', e);
      this.notice(`[Could not import that color scheme: ${e instanceof Error ? e.message : String(e)}]`);
    }
  }

  // --- Settings, zoom and the bell ---

  /** The effective terminal font size: what config.json asks for, plus the
   *  session's zoom. Unzoomed, the configured size is used exactly as written
   *  — the bounds belong to the zoom, not to the setting, and config.json is
   *  allowed to ask for sizes no amount of Cmd+= would reach. */
  private get fontSize(): number {
    if (this.zoomDelta === 0) return this.settings.fontSize;
    return clamp(this.settings.fontSize + this.zoomDelta, MIN_FONT_SIZE, MAX_FONT_SIZE);
  }

  /** The xterm options a pane should be running with right now. `active`
   *  decides the cursor blink and nothing else: only the focused pane blinks,
   *  whatever the setting says, or a four-way split becomes a light show. */
  private paneOptions(active: boolean): PaneOptions {
    return {
      fontFamily: this.settings.fontFamily,
      fontSize: this.fontSize,
      lineHeight: this.settings.lineHeight,
      cursorStyle: this.settings.cursorStyle,
      cursorBlink: active && this.settings.cursorBlink,
      scrollback: this.settings.scrollback,
      macOptionIsMeta: this.settings.optionIsMeta,
      copyOnSelect: this.settings.copyOnSelect,
    };
  }

  /** Push the current settings into every pane in every tab, then re-fit the
   *  ones on screen — which is what tells their shells the new size. Panes in
   *  background tabs hold their font back until their tab is next shown; see
   *  TerminalPane.applyOptions(). */
  private applySettingsToPanes(): void {
    for (const tab of this.tabs) {
      for (let i = 0; i < tab.panes.length; i++) {
        const isActive = tab === this.tab && i === this.activeIndex;
        tab.panes[i].pane.applyOptions(this.paneOptions(isActive));
      }
    }
    this.fitAll();
  }

  /** "Reload Settings": re-read the file and apply it to the running window. */
  private async reloadSettings(): Promise<void> {
    this.settings = await this.fetchSettings();
    this.applySettingsToPanes();
  }

  /** Session-only font zoom. Deliberately not persisted: zoom is what the user
   *  needs for the next ten minutes, and the size they actually want lives in
   *  config.json — which is what Cmd+0 goes back to. */
  private zoomBy(delta: number): void {
    const current = this.fontSize;
    const next = clamp(current + delta, MIN_FONT_SIZE, MAX_FONT_SIZE);
    // A configured size outside the zoom range can only be moved *into* it: a
    // 72px setting must not be made bigger by Cmd+= clamping it down to 40.
    if (next === current || Math.sign(next - current) !== Math.sign(delta)) return;
    this.zoomDelta = next - this.settings.fontSize;
    this.applySettingsToPanes();
  }

  private zoomReset(): void {
    if (this.zoomDelta === 0) return;
    this.zoomDelta = 0;
    this.applySettingsToPanes();
  }

  /** A pane rang the bell. What that means is the user's to configure, and the
   *  attention dot exists because the pane that rang is often not the one being
   *  looked at — a long build in a background tab is the whole point of a bell.
   *
   *  Coalesced per pane, before anything else happens. xterm has no BEL throttle
   *  of its own, so every BEL byte the shell writes arrives here: `cat` of a
   *  binary is thousands in a second, and each one would be a binding call, a
   *  system alert sound and a border flash. Bells inside the window are dropped
   *  rather than queued — replaying eight thousand of them afterwards would say
   *  nothing the first one didn't. (The Go side rate-limits the sound as well;
   *  this is the half that keeps the calls off the bridge to begin with.) */
  private handleBell(info: PaneInfo, tab: Tab): void {
    const mode = this.settings.bell;
    if (mode === 'none') return;

    const now = performance.now();
    const since = this.bellLastAt.get(info.element);
    if (since !== undefined && now - since < BELL_COALESCE_MS) return;
    this.bellLastAt.set(info.element, now);

    if (mode === 'sound' || mode === 'both') {
      window.go.main.App.Bell().catch(() => { /* a bell nobody hears is not an error */ });
    }
    if (mode !== 'visual' && mode !== 'both') return;

    const el = info.element;
    el.classList.add('bell-flash');
    const running = this.bellTimers.get(el);
    if (running) clearTimeout(running);
    this.bellTimers.set(el, setTimeout(() => {
      this.bellTimers.delete(el);
      el.classList.remove('bell-flash');
    }, BELL_FLASH_MS));

    if (tab !== this.tab && !tab.attention) {
      tab.attention = true;
      this.renderTabBar();
    }
  }

  // --- Find in scrollback ---

  /** Cmd+F. One bar at a time, on the active pane; a second Cmd+F while it is
   *  open puts the keyboard back in the field with the term selected, so the
   *  next thing typed replaces it. */
  private openFind(): void {
    // Never behind a dialog. The bar takes the keyboard the moment it exists, so
    // opening one under the palette or the AI prompt would leave the user typing
    // into a field they cannot see.
    if (this.isModalOpen()) return;
    const info = this.panes[this.activeIndex];
    if (!info) return;
    if (this.findBar?.pane === info.pane) {
      this.findBar.focusInput();
      return;
    }
    this.closeFind();
    this.findBar = new FindBar(info.element, info.pane, {
      onClose: () => {
        this.findBar = null;
        this.focusActivePane();
      },
    });
  }

  /** Take the bar down without moving the focus — the caller is already moving
   *  it somewhere (another pane, another tab), or the pane is going away. */
  private closeFind(): void {
    const bar = this.findBar;
    if (!bar) return;
    this.findBar = null;
    bar.dispose();
  }

  /** Say something in the pane the user is looking at, on the app's own behalf. */
  private notice(text: string): void {
    const pane = this.panes[this.activeIndex] || this.panes[0];
    pane?.pane.writeNotice(text);
  }

  // --- Status Bar ---

  private renderStatusBar(): void {
    const updateBadge = this.updateInfo?.available
      ? `<a class="status-update" id="status-update-link">Update v${escHtml(this.updateInfo.latestVersion)} available</a>`
      : '';

    let statsBadge = '';
    if (this.systemStats) {
      const cpu = this.systemStats.cpuPercent.toFixed(1);
      const memPct = this.systemStats.memoryPercent.toFixed(0);
      const usedGB = (this.systemStats.memoryUsedMB / 1024).toFixed(1);
      const totalGB = (this.systemStats.memoryTotalMB / 1024).toFixed(0);
      const cpuLevel = severityClass(this.systemStats.cpuPercent);
      const memLevel = severityClass(this.systemStats.memoryPercent);
      statsBadge = `<span class="status-stats">`
        + `<span class="status-stat ${cpuLevel}" aria-label="CPU usage">${ICON_CPU}<span class="status-stat-value">${cpu}%</span></span>`
        + `<span class="status-stat ${memLevel}" aria-label="Memory usage">${ICON_MEM}<span class="status-stat-value">${usedGB}/${totalGB}G ${memPct}%</span></span>`
        + `</span>`;
    }

    const leftSep = updateBadge && statsBadge ? '<span class="status-sep">|</span>' : '';

    this.statusbar.innerHTML = `
      <div class="status-left">${updateBadge}${leftSep}${statsBadge}</div>
      <div class="status-right">
        <span class="status-key">${CMD.COMMAND_PALETTE.shortcut}</span><span class="status-label">cmds</span>
        <span class="status-sep">|</span>
        <span class="status-key">${CMD.AI_COMMAND.shortcut}</span><span class="status-label">ai</span>
        <span class="status-sep">|</span>
        <span class="status-key">${CMD.FIND.shortcut}</span><span class="status-label">find</span>
        <span class="status-sep">|</span>
        <span class="status-key">${CMD.NEW_TAB.shortcut}</span><span class="status-label">tab</span>
        <span class="status-sep">|</span>
        <span class="status-key">${CMD.SPLIT_VERTICAL.shortcut}</span><span class="status-label">vsplit</span>
        <span class="status-sep">|</span>
        <span class="status-key">${CMD.SPLIT_HORIZONTAL.shortcut}</span><span class="status-label">hsplit</span>
        <span class="status-sep">|</span>
        <span class="status-key">${CMD.CLEAR_TERMINAL.shortcut}</span><span class="status-label">clear</span>
      </div>
    `;

    // Safe: innerHTML above destroys old elements, so new addEventListener doesn't accumulate
    if (this.updateInfo?.available) {
      document.getElementById('status-update-link')?.addEventListener('click', () => {
        this.promptUpdate();
      });
    }

  }

  private async promptUpdate(): Promise<void> {
    if (!this.updateInfo?.available) return;
    // Captured now: a later check can clear updateInfo out from under the
    // button's handler.
    const latestVersion = this.updateInfo.latestVersion;

    // Show confirmation overlay
    const overlay = document.createElement('div');
    overlay.className = 'update-overlay';
    overlay.innerHTML = `<div class="update-dialog">
      <div class="update-dialog-title">Update Available</div>
      <div class="update-dialog-body">
        v${escHtml(this.updateInfo.latestVersion)} is ready to install.<br>
        The app will restart automatically.
      </div>
      <div class="update-dialog-actions">
        <button class="theme-btn theme-btn-cancel" id="update-cancel">Later</button>
        <button class="theme-btn theme-btn-save" id="update-now">Update Now</button>
      </div>
    </div>`;
    document.body.appendChild(overlay);

    document.getElementById('update-cancel')?.addEventListener('click', () => overlay.remove());
    document.getElementById('update-now')?.addEventListener('click', async () => {
      const btn = document.getElementById('update-now') as HTMLButtonElement;
      btn.textContent = 'Downloading...';
      btn.disabled = true;
      try {
        await window.go.main.App.ApplyUpdate();
      } catch (e) {
        btn.textContent = 'Failed — retry';
        btn.disabled = false;
        logError(`Failed to apply update v${latestVersion}`, e);
      }
    });
  }

  // --- Custom Commands ---

  private async refreshCustomCommands(): Promise<void> {
    try {
      const globalCmds = await window.go.main.App.GetGlobalCommands() || [];
      let localCmds: CustomCommand[] = [];
      try {
        if (this.tab?.panes[this.activeIndex]?.pane?.sessionId) {
          const cwd = await this.tab.panes[this.activeIndex].pane.getCWD();
          if (cwd) localCmds = await window.go.main.App.GetLocalCommands(cwd) || [];
        }
      } catch (_) {}
      this.customCommands = [...localCmds, ...globalCmds];
    } catch (e) {
      this.customCommands = [];
    }
  }

  private getBuiltInCommands(): PaletteCommand[] {
    return [
      { name: CMD.NEW_TAB.name, desc: CMD.NEW_TAB.desc, category: CMD.NEW_TAB.category, shortcutDisplay: CMD.NEW_TAB.shortcut, action: () => this.createTab() },
      { name: CMD.CLOSE_TAB.name, desc: CMD.CLOSE_TAB.desc, category: CMD.CLOSE_TAB.category, shortcutDisplay: CMD.CLOSE_TAB.shortcut, action: () => this.confirmCloseTab(this.activeTabIndex) },
      { name: CMD.RENAME_TAB.name, desc: CMD.RENAME_TAB.desc, category: CMD.RENAME_TAB.category, action: () => { this.palette.hide(); this.renamingTabIndex = this.activeTabIndex; this.renderTabBar(); } },
      { name: CMD.NEXT_TAB.name, desc: CMD.NEXT_TAB.desc, category: CMD.NEXT_TAB.category, shortcutDisplay: CMD.NEXT_TAB.shortcut, action: () => this.cycleTab(1) },
      { name: CMD.PREV_TAB.name, desc: CMD.PREV_TAB.desc, category: CMD.PREV_TAB.category, shortcutDisplay: CMD.PREV_TAB.shortcut, action: () => this.cycleTab(-1) },
      { name: CMD.REOPEN_TAB.name, desc: CMD.REOPEN_TAB.desc, category: CMD.REOPEN_TAB.category, shortcutDisplay: CMD.REOPEN_TAB.shortcut, action: () => { this.reopenClosedTab().catch(e => logError('Failed to reopen the closed tab', e)); } },
      { name: CMD.SPLIT_VERTICAL.name, desc: CMD.SPLIT_VERTICAL.desc, category: CMD.SPLIT_VERTICAL.category, shortcutDisplay: CMD.SPLIT_VERTICAL.shortcut, action: () => this.splitPane('vertical') },
      { name: CMD.SPLIT_HORIZONTAL.name, desc: CMD.SPLIT_HORIZONTAL.desc, category: CMD.SPLIT_HORIZONTAL.category, shortcutDisplay: CMD.SPLIT_HORIZONTAL.shortcut, action: () => this.splitPane('horizontal') },
      { name: CMD.CLOSE_PANE.name, desc: CMD.CLOSE_PANE.desc, category: CMD.CLOSE_PANE.category, shortcutDisplay: CMD.CLOSE_PANE.shortcut, action: () => this.confirmCloseActivePane() },
      { name: CMD.NEXT_PANE.name, desc: CMD.NEXT_PANE.desc, category: CMD.NEXT_PANE.category, shortcutDisplay: CMD.NEXT_PANE.shortcut, action: () => this.navigateSpatial('right') },
      { name: CMD.PREV_PANE.name, desc: CMD.PREV_PANE.desc, category: CMD.PREV_PANE.category, shortcutDisplay: CMD.PREV_PANE.shortcut, action: () => this.navigateSpatial('left') },
      { name: CMD.NAV_PREV_COMMAND.name, desc: CMD.NAV_PREV_COMMAND.desc, category: CMD.NAV_PREV_COMMAND.category, shortcutDisplay: CMD.NAV_PREV_COMMAND.shortcut, action: () => { this.panes[this.activeIndex]?.pane.shellIntegration.navigateToBlock('prev'); } },
      { name: CMD.NAV_NEXT_COMMAND.name, desc: CMD.NAV_NEXT_COMMAND.desc, category: CMD.NAV_NEXT_COMMAND.category, shortcutDisplay: CMD.NAV_NEXT_COMMAND.shortcut, action: () => { this.panes[this.activeIndex]?.pane.shellIntegration.navigateToBlock('next'); } },
      { name: CMD.SEARCH_HISTORY.name, desc: CMD.SEARCH_HISTORY.desc, category: CMD.SEARCH_HISTORY.category, shortcutDisplay: CMD.SEARCH_HISTORY.shortcut, action: () => { this.closePaletteIfOpen(); this.historyModal.show(); } },
      { name: CMD.AI_COMMAND.name, desc: CMD.AI_COMMAND.desc, category: CMD.AI_COMMAND.category, shortcutDisplay: CMD.AI_COMMAND.shortcut, action: () => { this.closePaletteIfOpen(); this.askAI.show(); } },
      ...(this.modelUpdateAvailable ? [{ name: CMD.UPDATE_MODEL.name, desc: CMD.UPDATE_MODEL.desc, category: CMD.UPDATE_MODEL.category, action: () => { this.closePaletteIfOpen(); this.handleModelDownload(); } }] : []),
      { name: CMD.SESSION_STATUS.name, desc: CMD.SESSION_STATUS.desc, category: CMD.SESSION_STATUS.category, shortcutDisplay: CMD.SESSION_STATUS.shortcut, action: () => { this.closePaletteIfOpen(); this.statusModal.show(); } },
      { name: CMD.COMMAND_PALETTE.name, desc: CMD.COMMAND_PALETTE.desc, category: CMD.COMMAND_PALETTE.category, shortcutDisplay: CMD.COMMAND_PALETTE.shortcut, action: () => this.palette.show() },
      { name: CMD.CLEAR_TERMINAL.name, desc: CMD.CLEAR_TERMINAL.desc, category: CMD.CLEAR_TERMINAL.category, shortcutDisplay: CMD.CLEAR_TERMINAL.shortcut, action: () => this.clearActiveTerminal() },
      { name: CMD.FIND.name, desc: CMD.FIND.desc, category: CMD.FIND.category, shortcutDisplay: CMD.FIND.shortcut, action: () => { this.closePaletteIfOpen(); this.openFind(); } },
      { name: CMD.REVEAL_LOGS.name, desc: CMD.REVEAL_LOGS.desc, category: CMD.REVEAL_LOGS.category, action: () => { this.closePaletteIfOpen(); this.revealLogs(); } },
      { name: CMD.COPY_LAST_OUTPUT.name, desc: CMD.COPY_LAST_OUTPUT.desc, category: CMD.COPY_LAST_OUTPUT.category, action: () => { const output = this.panes[this.activeIndex]?.pane.shellIntegration.getLastCommandOutput(); if (output) navigator.clipboard.writeText(output); this.closePaletteIfOpen(); } },
      { name: CMD.CREATE_COMMAND.name, desc: CMD.CREATE_COMMAND.desc, category: CMD.CREATE_COMMAND.category, shortcutDisplay: CMD.CREATE_COMMAND.shortcut, action: () => { this.palette.hide(); const input = this.panes[this.activeIndex]?.pane?.getCurrentInput() || ''; this.wizard.show(input); } },
      ...this.themes.map(t => ({
        name: `Theme: ${t.name}`, desc: `Switch to ${t.name} theme`, category: 'Appearance',
        isTheme: true,
        themeData: {
          name: t.name,
          background: t.background, foreground: t.foreground,
          accent: t.accent, accentDim: t.accentDim,
          border: t.border, borderActive: t.borderActive,
          statusBg: t.statusBg, statusFg: t.statusFg,
          cursorColor: t.cursorColor, selectionBg: t.selectionBg,
          black: t.xterm.black, red: t.xterm.red, green: t.xterm.green, yellow: t.xterm.yellow,
          blue: t.xterm.blue, magenta: t.xterm.magenta, cyan: t.xterm.cyan, white: t.xterm.white,
          brightBlack: t.xterm.brightBlack, brightRed: t.xterm.brightRed,
          brightGreen: t.xterm.brightGreen, brightYellow: t.xterm.brightYellow,
          brightBlue: t.xterm.brightBlue, brightMagenta: t.xterm.brightMagenta,
          brightCyan: t.xterm.brightCyan, brightWhite: t.xterm.brightWhite,
        },
        action: () => this.setTheme(t.name),
      })),
      { name: 'Create Theme', desc: 'Design a new color theme', category: 'Appearance', action: () => { this.closePaletteIfOpen(); this.themeWizard.show(); } },
      { name: CMD.IMPORT_SCHEME.name, desc: CMD.IMPORT_SCHEME.desc, category: CMD.IMPORT_SCHEME.category, action: () => { this.closePaletteIfOpen(); this.importColorScheme(); } },
      { name: CMD.ZOOM_IN.name, desc: CMD.ZOOM_IN.desc, category: CMD.ZOOM_IN.category, shortcutDisplay: CMD.ZOOM_IN.shortcut, action: () => this.zoomBy(1) },
      { name: CMD.ZOOM_OUT.name, desc: CMD.ZOOM_OUT.desc, category: CMD.ZOOM_OUT.category, shortcutDisplay: CMD.ZOOM_OUT.shortcut, action: () => this.zoomBy(-1) },
      { name: CMD.ZOOM_RESET.name, desc: CMD.ZOOM_RESET.desc, category: CMD.ZOOM_RESET.category, shortcutDisplay: CMD.ZOOM_RESET.shortcut, action: () => this.zoomReset() },
      { name: CMD.OPEN_SETTINGS.name, desc: CMD.OPEN_SETTINGS.desc, category: CMD.OPEN_SETTINGS.category, action: () => { this.closePaletteIfOpen(); window.go.main.App.OpenSettingsFile().catch(e => logError('Failed to open config.json', e)); } },
      { name: CMD.RELOAD_SETTINGS.name, desc: CMD.RELOAD_SETTINGS.desc, category: CMD.RELOAD_SETTINGS.category, action: () => { this.reloadSettings().catch(e => logError('Failed to reload settings', e)); } },
    ];
  }

  private editTheme(cmd: PaletteCommand): void {
    this.palette.hide();
    if (cmd.themeData) {
      this.themeWizard.showForEdit(cmd.themeData.name, cmd.themeData);
    }
  }

  private async deleteTheme(cmd: PaletteCommand): Promise<void> {
    if (!cmd.themeData) return;
    const themeName = cmd.themeData.name;
    try {
      await window.go.main.App.DeleteTheme(themeName);
      const themeDTOs = await window.go.main.App.GetThemes();
      this.themes = themeDTOs.map(themeFromDTO);
      // If we deleted the active theme, switch to the first one
      if (this.currentTheme.name === themeName && this.themes.length > 0) {
        this.currentTheme = this.themes[0];
        applyThemeToCSS(this.currentTheme);
        for (const tab of this.tabs) {
          for (const p of tab.panes) p.pane.setTheme(this.currentTheme.xterm);
        }
      }
    } catch (e) {
      logError(`Failed to delete theme "${themeName}"`, e);
      // Same failure as a refused save: the backend will not rewrite a
      // themes.json it could not read, and says which file it means. The theme
      // is still in the list and the palette is closing, so the pane is the only
      // place left to say so.
      this.notice(`[Could not delete "${themeName}": ${e instanceof Error ? e.message : String(e)}]`);
    }
  }

  private editCustomCommand(cmd: PaletteCommand): void {
    this.palette.hide();
    this.wizard.showForEdit({
      name: cmd.name,
      command: cmd.command,
      desc: cmd.desc,
      scope: cmd.scope,
      shortcutKey: cmd.shortcutKey,
    });
  }

  // --- Helpers for callbacks ---

  private async getActivePaneCWD(): Promise<string> {
    const ap = this.panes[this.activeIndex];
    if (!ap) return '';
    return await ap.pane.getCWD();
  }

  private focusActivePane(): void {
    if (this.panes[this.activeIndex]) this.panes[this.activeIndex].pane.focus();
  }

  private closePaletteIfOpen(): void {
    if (this.palette.isOpen()) this.palette.hide();
  }

  /** Show the log file in Finder, so a user can attach it to a bug report
   *  without having to be told where the app keeps it. */
  private revealLogs(): void {
    window.go.main.App.RevealLogs().catch((e) => {
      logError('Failed to reveal the log file in Finder', e);
    });
  }

  private clearActiveTerminal(): void {
    const ap = this.panes[this.activeIndex];
    if (!ap) return;
    ap.pane.clear();
    // Send Ctrl+L to the shell so it redraws the full prompt — a dead pane has
    // no shell to ask, and writing to it would just reject.
    if (ap.pane.sessionId) {
      window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64('\x0c')).catch(() => {});
    }
  }

  // --- AI Command (Cmd+K) ---

  private setAILoading(loading: boolean): void {
    this.aiGenerating = loading;
    this.renderStatusBar();
    const ap = this.panes[this.activeIndex];
    if (ap) {
      ap.element.classList.toggle('ai-loading', loading);
    }
  }

  private async handleAskAI(): Promise<void> {
    if (this.aiGenerating) return;
    const ap = this.panes[this.activeIndex];
    if (!ap?.pane.sessionId) return;

    // Read what the user typed on the current line
    const query = ap.pane.getCurrentInput();
    if (!query.trim()) return;

    // Track immediately before any async work so the recording callback can filter it
    this.aiPrompts.add(query.trim());

    // Check model, trigger download dialog if needed
    const ready = await window.go.main.App.IsModelReady();
    if (!ready) {
      await this.handleModelDownload();
      // Recheck after dialog closes
      if (!(await window.go.main.App.IsModelReady())) {
        this.aiPrompts.delete(query.trim());
        return;
      }
    }

    // The shell can exit while the model is downloading or loading. There is
    // nowhere to put an answer then, so don't spend a generation on it.
    if (!ap.pane.sessionId) {
      this.aiPrompts.delete(query.trim());
      return;
    }

    // Show generating state — rotating border + status bar
    this.setAILoading(true);

    // Clear current line (Ctrl+U clears everything before cursor in most shells)
    window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64('\x15')).catch(() => {});

    try {
      const cwd = await ap.pane.getCWD();
      const command = await window.go.main.App.AskAI(query, cwd);

      // Write the generated command WITHOUT executing (no newline). Generation
      // is slow enough that the shell may be gone by the time it lands.
      if (command && ap.pane.sessionId) {
        window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64(command)).catch(() => {});
      }
    } catch (err) {
      // Write a comment so the user sees what happened without accidentally executing the prompt
      if (ap.pane.sessionId) {
        window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64(`# AI failed: ${query}`)).catch(() => {});
      }
      // The prompt itself is the user's text and stays out of the log file.
      logError(`AI command generation failed for session ${ap.pane.sessionId || '(exited)'}`, err);
    }

    this.setAILoading(false);
  }

  private async checkModelUpdate(): Promise<void> {
    try {
      this.modelUpdateAvailable = await window.go.main.App.CheckModelUpdate();
    } catch { /* best-effort */ }
  }

  private async handleModelDownload(): Promise<void> {
    // Show dialog (same style as the app update dialog)
    const overlay = document.createElement('div');
    overlay.className = 'update-overlay';
    overlay.innerHTML = `<div class="update-dialog">
      <div class="update-dialog-title">${this.modelUpdateAvailable ? 'Update AI Model' : 'Download AI Model'}</div>
      <div class="update-dialog-body">
        <div id="model-dl-status">Connecting...</div>
        <div class="model-dl-bar-track"><div class="model-dl-bar-fill" id="model-dl-bar"></div></div>
      </div>
      <div class="update-dialog-actions">
        <button class="theme-btn theme-btn-cancel" id="model-dl-cancel">Cancel</button>
      </div>
    </div>`;
    document.body.appendChild(overlay);

    const statusEl = document.getElementById('model-dl-status')!;
    const barEl = document.getElementById('model-dl-bar')!;

    // Cancel button + Escape
    let cancelled = false;
    const cancel = () => {
      cancelled = true;
      window.go.main.App.SkipDownload();
    };
    document.getElementById('model-dl-cancel')?.addEventListener('click', cancel);
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') cancel();
      e.stopPropagation();
    };
    document.addEventListener('keydown', onKey, true);

    const unsub = window.runtime.EventsOn('model:download-progress', (data: { downloaded: number; total: number }) => {
      if (data.total > 0) {
        const pct = Math.round((data.downloaded / data.total) * 100);
        const mbDown = (data.downloaded / 1024 / 1024).toFixed(1);
        const mbTotal = (data.total / 1024 / 1024).toFixed(1);
        statusEl.textContent = `Downloading... ${mbDown} / ${mbTotal} MB (${pct}%)`;
        barEl.style.width = `${pct}%`;
      }
    });

    try {
      await window.go.main.App.DownloadModel();

      if (!cancelled) {
        statusEl.textContent = 'Loading model...';
        barEl.style.width = '100%';
        await window.go.main.App.InitLLM();
        this.modelUpdateAvailable = false;
        statusEl.textContent = 'AI model ready!';
        setTimeout(() => {
          overlay.remove();
          this.focusActivePane();
        }, 800);
      } else {
        overlay.remove();
        this.focusActivePane();
      }
    } catch {
      if (cancelled) {
        overlay.remove();
      } else {
        statusEl.textContent = 'Download failed. Check your network.';
        barEl.style.width = '0%';
        const cancelBtn = document.getElementById('model-dl-cancel');
        if (cancelBtn) cancelBtn.textContent = 'Close';
      }
      this.focusActivePane();
    } finally {
      unsub();
    }

    document.removeEventListener('keydown', onKey, true);
  }

  // --- Keyboard ---

  /** Run one of the app's named actions. Both the keyboard and the native menu
   *  bar arrive here, and both name the same actions, because a menu item and
   *  its shortcut have to do exactly one thing — the same thing.
   *
   *  WKWebView routes a Cmd-key to the page or to the menu, never to both, and
   *  the window below exists because "never" is a claim about a webview rather
   *  than something this app can check. A second arrival of the same action
   *  from the *other* source inside it is one gesture counted twice, and is
   *  dropped. Two arrivals from the same source are two presses — holding
   *  Cmd+= to zoom is exactly that — and always run. */
  private dispatchAction(action: string, source: 'key' | 'menu'): void {
    // Nothing the menu bar can ask for happens behind a dialog. The keyboard
    // half of this never gets here — handleKeydown gives the modal the event and
    // cancels it — but a menu item reaches the frontend through an event, with
    // no keystroke to cancel: either the user picked it with the mouse while a
    // dialog was up, or the webview let the accelerator through to the menu
    // after all. Both would close the tab, split the pane or open the find bar
    // *behind* whatever the user is actually typing into.
    if (source === 'menu' && this.isModalOpen()) {
      logInfo(`Ignoring menu action "${action}": a dialog is over the panes`);
      return;
    }

    const now = performance.now();
    const last = this.lastAction;
    if (last && last.name === action && last.source !== source && now - last.at < MENU_ACTION_DEDUP_MS) {
      return;
    }
    this.lastAction = { name: action, at: now, source };

    switch (action) {
      case 'new-tab': this.createTab(); return;
      case 'close-tab': this.confirmCloseTab(this.activeTabIndex); return;
      case 'close-pane': this.confirmCloseActivePane(); return;
      case 'split-vertical': this.splitPane('vertical'); return;
      case 'split-horizontal': this.splitPane('horizontal'); return;
      case 'find': this.openFind(); return;
      case 'clear': this.clearActiveTerminal(); return;
      case 'zoom-in': this.zoomBy(1); return;
      case 'zoom-out': this.zoomBy(-1); return;
      case 'zoom-reset': this.zoomReset(); return;
      case 'next-tab': this.cycleTab(1); return;
      case 'prev-tab': this.cycleTab(-1); return;
      case 'reopen-tab':
        this.reopenClosedTab().catch(e => logError('Failed to reopen the closed tab', e));
        return;
      case 'palette': this.palette.show(); return;
      case 'status': this.statusModal.show(); return;
      case 'history': this.historyModal.show(); return;
      case 'reload-settings':
        this.reloadSettings().catch(e => logError('Failed to reload settings', e));
        return;
      case 'import-scheme': this.importColorScheme(); return;
      default:
        // A menu built against a newer frontend than this one. Nothing to do,
        // but worth a line: it is the only sign the two have drifted apart.
        logInfo(`Ignoring unknown menu action "${action}"`);
    }
  }

  /** End a Cmd-key here, so the native menu never gets a second look at it.
   *
   *  A dialog is over the panes and the event has just been offered to it. If it
   *  wanted the key it cancelled the event itself (every modal calls
   *  preventDefault on the combos it handles, the palette's Cmd+E and Cmd+D
   *  included) and there is nothing left to do. If it did not, the key must
   *  still stop here: WKWebView's performKeyEquivalent: gives the page the first
   *  pass, and when the page leaves the event un-prevented WebKit re-injects it
   *  so the main menu's accelerator can fire. `stopPropagation` does not count —
   *  only `defaultPrevented` does. That second pass is how Cmd+W closes the tab
   *  behind an open AI prompt, and Cmd+F opens a find bar underneath it.
   *
   *  Two kinds of key are deliberately let past. Ctrl and Alt combos, because
   *  none of this app's own shortcuts use them and ⌃⌘F is the menu's Toggle Full
   *  Screen. And the roles' own items — the Edit menu, which is the only reason
   *  Cmd+C/V/X/A reach a focused text field in a webview at all, and Quit, Hide
   *  and Minimize, which have to work whatever is on screen. */
  private sealMetaKey(e: KeyboardEvent): void {
    if (!e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.defaultPrevented) return;
    if (MODAL_PASSTHROUGH_META_KEYS.has(e.key.toLowerCase())) return;
    e.preventDefault();
  }

  private handleKeydown(e: KeyboardEvent): void {
    const isMeta = e.metaKey;

    // Smart render panel (check all panes)
    for (const tab of this.tabs) {
      for (const p of tab.panes) {
        if (p.pane.smartRender.isPanelOpen()) {
          if (p.pane.smartRender.handleKeydown(e)) return;
        }
      }
    }

    // History modal
    if (this.historyModal.isOpen()) {
      e.stopPropagation();
      this.historyModal.handleKeydown(e);
      this.sealMetaKey(e);
      return;
    }

    // Status modal takes highest priority after wizards
    if (this.statusModal.isOpen()) {
      e.stopPropagation();
      this.statusModal.handleKeydown(e);
      this.sealMetaKey(e);
      return;
    }

    // Theme wizard takes highest priority
    if (this.themeWizard.isOpen()) {
      this.themeWizard.handleKeydown(e);
      this.sealMetaKey(e);
      return;
    }

    // Wizard takes priority
    if (this.wizard.isOpen()) {
      e.stopPropagation();
      this.wizard.handleKeydown(e);
      this.sealMetaKey(e);
      return;
    }

    // AI modal takes priority
    if (this.askAI.isOpen()) {
      e.stopPropagation();
      this.askAI.handleKeydown(e);
      this.sealMetaKey(e);
      return;
    }

    // Palette takes second priority
    if (this.palette.isOpen()) {
      this.palette.handleKeydown(e);
      this.sealMetaKey(e);
      return;
    }

    // The find bar, when there is one. It claims only what it needs — the
    // typing in its own field, Enter, Escape, and Cmd+G from anywhere — and
    // lets every other shortcut past, so Cmd+T still opens a tab with the
    // cursor in the search field.
    if (this.findBar?.handleKeydown(e)) return;

    // Custom command shortcuts (checked first — they use Ctrl/Alt combos
    // that don't overlap with built-in Cmd shortcuts)
    if (this.customCommands.length > 0) {
      const parts: string[] = [];
      if (e.metaKey) parts.push('Cmd');
      if (e.ctrlKey) parts.push('Ctrl');
      if (e.shiftKey) parts.push('Shift');
      if (e.altKey) parts.push('Alt');
      const keyName = e.key.length === 1 ? e.key.toUpperCase() : e.key;
      if (!['Control', 'Meta', 'Shift', 'Alt'].includes(keyName)) {
        parts.push(keyName);
        const pressed = parts.join('+');
        for (const c of this.customCommands) {
          if (c.shortcut && c.shortcut.toLowerCase() === pressed.toLowerCase()) {
            e.preventDefault(); e.stopImmediatePropagation();
            const ap = this.panes[this.activeIndex];
            // A pane whose shell has exited has nothing to run the command in;
            // the shortcut is still swallowed, exactly as when it does run.
            if (!ap?.pane.sessionId) return;
            const data = c.command.includes('\n')
              ? '\x1b[200~' + c.command + '\x1b[201~\n'
              : c.command + '\n';
            window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64(data)).catch(() => {});
            return;
          }
        }
      }
    }

    // Ctrl+L is not intercepted: vim, less, tmux and Claude Code all bind it,
    // so it belongs to the foreground program. Cmd+L is this app's own clear.

    // Ctrl+Tab / Ctrl+Shift+Tab cycle tabs, as they do everywhere else on the
    // desktop. This one has to leave the event stream entirely rather than
    // merely be cancelled: xterm turns Ctrl+Tab into a literal tab character
    // for the shell, and it reads the event rather than asking whether anyone
    // else already dealt with it — only never reaching its listener stops that.
    if (e.ctrlKey && !e.metaKey && !e.altKey && e.key === 'Tab') {
      e.preventDefault();
      e.stopImmediatePropagation();
      this.dispatchAction(e.shiftKey ? 'prev-tab' : 'next-tab', 'key');
      return;
    }

    // Built-in shortcuts (Cmd-based).
    //
    // Ctrl and Alt held is somebody else's gesture, and the test has to be here
    // rather than on each binding: cancelling ⌃⌘F to open the find bar is what
    // stopped Toggle Full Screen from ever firing, because a Cmd-key the page
    // prevents is one the main menu is never offered. None of the app's own
    // shortcuts use either modifier — the ones that do are custom commands, and
    // they are matched above, with their modifiers spelled out.
    if (isMeta && !e.ctrlKey && !e.altKey) {
      if (e.shiftKey && e.key.toLowerCase() === 'c') {
        e.preventDefault();
        const input = this.panes[this.activeIndex]?.pane?.getCurrentInput() || '';
        this.wizard.show(input);
        return;
      }
      if (e.shiftKey && e.key.toLowerCase() === 'r') {
        e.preventDefault();
        this.dispatchAction('history', 'key');
        return;
      }
      if (e.shiftKey && e.key.toLowerCase() === 'd') {
        e.preventDefault();
        this.dispatchAction('split-horizontal', 'key');
        return;
      }
      if (e.shiftKey && e.key.toLowerCase() === 't') {
        e.preventDefault();
        this.dispatchAction('reopen-tab', 'key');
        return;
      }
      if (e.shiftKey && e.key === '\\') {
        e.preventDefault();
        this.dispatchAction('split-vertical', 'key');
        return;
      }
      // With Shift held a US layout reports `}` / `{` / `+`, not the unshifted
      // key; both spellings are accepted so a layout that reports either works.
      if (e.shiftKey && (e.key === ']' || e.key === '}')) {
        e.preventDefault();
        this.dispatchAction('next-tab', 'key');
        return;
      }
      if (e.shiftKey && (e.key === '[' || e.key === '{')) {
        e.preventDefault();
        this.dispatchAction('prev-tab', 'key');
        return;
      }
      if (e.shiftKey && (e.key === '+' || e.key === '=')) {
        e.preventDefault();
        this.dispatchAction('zoom-in', 'key');
        return;
      }
      if (e.shiftKey && e.key === 'ArrowUp') {
        e.preventDefault();
        this.panes[this.activeIndex]?.pane.shellIntegration.navigateToBlock('prev');
        return;
      }
      if (e.shiftKey && e.key === 'ArrowDown') {
        e.preventDefault();
        this.panes[this.activeIndex]?.pane.shellIntegration.navigateToBlock('next');
        return;
      }
      if (e.shiftKey && e.key.toLowerCase() === 'x') {
        e.preventDefault();
        this.dispatchAction('close-pane', 'key');
        return;
      }
      // Cmd+G / Cmd+Shift+G belong to the find bar and are answered above while
      // one is open. With none open there is nothing to step through, so they
      // fall through to the shell rather than opening a bar the user did not
      // ask for.

      switch (e.key.toLowerCase()) {
        case 'k': e.preventDefault(); this.handleAskAI(); return;
        case 'i': e.preventDefault(); this.dispatchAction('status', 'key'); return;
        case 'p': e.preventDefault(); this.dispatchAction('palette', 'key'); return;
        case 't': e.preventDefault(); this.dispatchAction('new-tab', 'key'); return;
        case 'w': e.preventDefault(); this.dispatchAction('close-tab', 'key'); return;
        case 'f': e.preventDefault(); this.dispatchAction('find', 'key'); return;
        // Cmd+D splits vertically, Cmd+Shift+D horizontally — iTerm2's and
        // Ghostty's arrangement. Cmd+| is kept as an alias because it looks
        // like what it does; Cmd+- is now Zoom Out, which is what freed it.
        case 'd': case '|': e.preventDefault(); this.dispatchAction('split-vertical', 'key'); return;
        case '=': case '+': e.preventDefault(); this.dispatchAction('zoom-in', 'key'); return;
        case '-': e.preventDefault(); this.dispatchAction('zoom-out', 'key'); return;
        case '0': e.preventDefault(); this.dispatchAction('zoom-reset', 'key'); return;
        case 'l': e.preventDefault(); this.dispatchAction('clear', 'key'); return;
        case '1': case '2': case '3': case '4': case '5': case '6': case '7': case '8': case '9':
          e.preventDefault();
          const tabIdx = parseInt(e.key) - 1;
          if (tabIdx < this.tabs.length) this.switchToTab(tabIdx);
          return;
        case 'arrowright': case 'arrowleft': case 'arrowup': case 'arrowdown':
          e.preventDefault();
          this.navigateSpatial(e.key.toLowerCase().replace('arrow', '') as 'left' | 'right' | 'up' | 'down');
          return;
      }
    }
  }

  private navigateSpatial(direction: 'left' | 'right' | 'up' | 'down'): void {
    if (this.panes.length <= 1) return;
    const current = this.panes[this.activeIndex].element.getBoundingClientRect();
    const cx = current.left + current.width / 2, cy = current.top + current.height / 2;
    const isHorizontal = direction === 'left' || direction === 'right';
    let bestIndex = -1, bestDist = Infinity;
    for (let i = 0; i < this.panes.length; i++) {
      if (i === this.activeIndex) continue;
      const rect = this.panes[i].element.getBoundingClientRect();
      const px = rect.left + rect.width / 2, py = rect.top + rect.height / 2;
      // Early-out: skip panes that are clearly in the wrong direction
      if (isHorizontal) {
        if (direction === 'left' && px >= cx - SPATIAL_NAV_THRESHOLD) continue;
        if (direction === 'right' && px <= cx + SPATIAL_NAV_THRESHOLD) continue;
      } else {
        if (direction === 'up' && py >= cy - SPATIAL_NAV_THRESHOLD) continue;
        if (direction === 'down' && py <= cy + SPATIAL_NAV_THRESHOLD) continue;
      }
      const dist = Math.abs(px - cx) + Math.abs(py - cy);
      if (dist < bestDist) { bestDist = dist; bestIndex = i; }
    }
    if (bestIndex >= 0) this.setActive(bestIndex);
  }
}

document.addEventListener('DOMContentLoaded', async () => { const app = new ElTerminalo(); await app.init(); });
