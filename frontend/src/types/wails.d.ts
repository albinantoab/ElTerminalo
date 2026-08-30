// Wails runtime bindings — typed declarations for Go backend methods.

export interface WailsApp {
  CreateSession(cols: number, rows: number, cwd: string): Promise<string>;
  WriteToSession(sessionID: string, data: string): Promise<void>;
  // Wails wraps every bound method in a promise, including the ones that
  // return nothing: a failure here rejects, it does not throw.
  ResizeSession(sessionID: string, cols: number, rows: number): Promise<void>;
  CloseSession(sessionID: string): Promise<void>;
  /** Claim a session's output, and report whether it is still there. Call it
   *  immediately after subscribing to `pty:output:<id>` and `pty:exit:<id>`:
   *  the backend flushes whatever the shell wrote before those listeners
   *  existed, so nothing printed during startup is lost.
   *
   *  The session is dropped from the backend's map *before* `pty:exit:<id>` is
   *  emitted, so the return value is unambiguous for a caller that has already
   *  subscribed: `true` means the exit event is still to come, `false` means it
   *  has already been emitted — possibly before there was a listener for it. */
  AttachSession(sessionID: string): Promise<boolean>;
  /** Acknowledge `n` raw (decoded) bytes of this session's output, once xterm
   *  has finished writing them. This is the frontend half of the PTY's flow
   *  control: the backend stops reading the pty at 1 MiB unacknowledged and
   *  resumes below 256 KiB, so every byte handed to the terminal must be acked
   *  or the shell stalls. Unknown session ids are ignored. */
  AckOutput(sessionID: string, n: number): Promise<void>;
  GetSessionCWD(sessionID: string): Promise<string>;
  GetAllSessionCWDs(): Promise<Record<string, string>>;
  GetThemes(): Promise<ThemeDTO[]>;
  SaveAppState(stateJSON: string): Promise<void>;
  LoadAppState(): Promise<string>;
  /** Move an unusable state file aside as `state.json.failed-<unix>` and return
   *  the new path, or "" when there was nothing to move or the rename failed.
   *  The next save then starts from a clean file without destroying evidence. */
  QuarantineAppState(): Promise<string>;
  GetGlobalCommands(): Promise<CustomCommandDTO[]>;
  GetLocalCommands(cwd: string): Promise<CustomCommandDTO[]>;
  SaveCommand(scope: string, name: string, command: string, description: string, shortcut: string, cwd: string): Promise<void>;
  DeleteCommand(scope: string, name: string, cwd: string): Promise<void>;
  UpdateCommand(scope: string, oldName: string, newName: string, newCommand: string, newDescription: string, newShortcut: string, cwd: string): Promise<void>;
  SaveTheme(name: string, background: string, foreground: string, accent: string, accentDim: string, border: string, borderActive: string, statusBg: string, statusFg: string, cursorColor: string, selectionBg: string, black: string, red: string, green: string, yellow: string, blue: string, magenta: string, cyan: string, white: string, brightBlack: string, brightRed: string, brightGreen: string, brightYellow: string, brightBlue: string, brightMagenta: string, brightCyan: string, brightWhite: string): Promise<void>;
  DeleteTheme(name: string): Promise<void>;
  ConfirmQuit(): Promise<void>;
  /** Append one line to the app's log file. Fire-and-forget: see `log.ts`, the
   *  only place that should call this. */
  LogMessage(level: 'info' | 'warn' | 'error', message: string): Promise<void>;
  /** Show the log file in Finder. */
  RevealLogs(): Promise<void>;
  GetAllSessionStatuses(): Promise<Record<string, SessionStatusDTO>>;
  GetVersion(): Promise<string>;
  GetHostname(): Promise<string>;
  CheckForUpdate(): Promise<UpdateInfo>;
  ApplyUpdate(): Promise<void>;
  AskAI(prompt: string, cwd: string): Promise<string>;
  RecordCommand(command: string, cwd: string, exitCode: number, sessionID: string): Promise<void>;
  SearchHistory(query: string, cwd: string, limit: number): Promise<HistorySearchResult>;
  ClearHistory(): Promise<void>;
  IsModelReady(): Promise<boolean>;
  IsModelDownloaded(): Promise<boolean>;
  DownloadModel(): Promise<void>;
  SkipDownload(): Promise<void>;
  CheckModelUpdate(): Promise<boolean>;
  InitLLM(): Promise<void>;
  GetSystemStats(): Promise<SystemStats>;
  /** The user's settings, already validated and defaulted on the Go side.
   *  The frontend normalizes the result anyway — see `normalizeSettings` in
   *  main.ts: a hand-edited config.json reaches xterm through this call, and
   *  xterm *throws* on an out-of-range `lineHeight` or an unknown
   *  `cursorStyle`, which would take the whole window down. */
  GetSettings(): Promise<Settings>;
  /** Open config.json in the user's editor. */
  OpenSettingsFile(): Promise<void>;
  /** Set the native window title. It is hidden behind the custom titlebar, but
   *  it is what the Window menu and the app switcher read. */
  SetWindowTitle(title: string): Promise<void>;
  /** Play the system alert sound, for a terminal bell the user asked to hear. */
  Bell(): Promise<void>;
  /** Ask the user for a color scheme file and import it. Resolves with the
   *  imported theme's name, or "" when the user cancelled; rejects when the
   *  file could not be parsed. */
  ImportColorScheme(): Promise<string>;

  // ── Attention ──
  /** Set the Dock icon's badge, or clear it with "". The frontend sends the
   *  total number of panes waiting for the user, debounced. */
  SetDockBadge(label: string): Promise<void>;
  /** Post a native notification. `paneKey` is handed back on the
   *  `notification:activated` event when the user clicks the banner, so it has
   *  to be the pane's own id. Resolves false when nothing was posted. */
  Notify(title: string, body: string, paneKey: string): Promise<boolean>;
  /** Whether the app may post notifications at all — asked once at startup.
   *  False means the user (or the OS) said no, and Notify is never called. */
  NotificationsReady(): Promise<boolean>;

  // ── Transcripts ──
  /** Start recording everything a session prints. Resolves with the file it is
   *  being written to. */
  StartTranscript(sessionID: string): Promise<string>;
  StopTranscript(sessionID: string): Promise<void>;
  /** Where a session's transcript is being written, or "" when it is not
   *  recording. */
  TranscriptPath(sessionID: string): Promise<string>;

  // ── Open here ──
  /** Show a path in Finder. */
  RevealPath(path: string): Promise<void>;
  /** Open a path in the user's editor. The backend resolves $VISUAL, then
   *  $EDITOR, then Visual Studio Code, then TextEdit (files only), and
   *  falls back to the OS default — it is not the OS's choice. */
  OpenPath(path: string): Promise<void>;

  // ── Workspaces ──
  /** Store a whole window — the same JSON `SaveAppState` takes — under a name
   *  the user chose. Saving over an existing name replaces it. */
  SaveWorkspace(name: string, stateJSON: string): Promise<void>;
  ListWorkspaces(): Promise<WorkspaceInfo[]>;
  /** The saved state JSON for a workspace, by slug. */
  LoadWorkspace(slug: string): Promise<string>;
  DeleteWorkspace(slug: string): Promise<void>;
}

/** One entry of `ListWorkspaces`. `slug` is the key every other workspace call
 *  takes; `name` is what the user typed. */
export interface WorkspaceInfo {
  name: string;
  slug: string;
  /** Seconds since the epoch, as the Go side wrote it. */
  savedUnix: number;
  tabs: number;
  panes: number;
}

/** Payload of the `notification:activated` event: the user clicked a banner
 *  this app posted, and `paneKey` is the `PaneInfo.id` it was posted for. A key
 *  naming a pane that has since gone is ignored. */
/** Payload of `transcript:stopped`: a recording the backend ended on its own,
 *  either at the size cap or because the file could not be written. A user
 *  stop never emits this — the frontend already knows about those. */
export interface TranscriptStoppedPayload {
  sessionID: string;
  path: string;
  reason: 'cap' | 'error';
}

export interface NotificationActivatedPayload {
  paneKey: string;
}

/** The subset of config.json this app reads. The Go side owns the file, its
 *  validation and its defaults; these are the same defaults, kept here so the
 *  frontend can still start with something sane if the call fails or returns a
 *  value neither side expected. See DEFAULT_SETTINGS in constants.ts. */
export interface Settings {
  fontFamily: string;
  /** Points. 6-72 on disk; the session's zoom is applied on top of it. */
  fontSize: number;
  lineHeight: number;
  cursorStyle: 'block' | 'underline' | 'bar';
  cursorBlink: boolean;
  scrollback: number;
  /** Informational here — the backend is what spawns the shell. */
  shell: string;
  /** Whether Option sends Meta rather than composing a character. */
  optionIsMeta: boolean;
  bell: 'none' | 'sound' | 'visual' | 'both';
  copyOnSelect: boolean;
  /** Go-side only: the frontend never asks this question itself. */
  confirmQuit: 'running' | 'always' | 'never';
}

/** Payload of the `menu:action` event — one of the action names the native
 *  menu bar can send. Routed to the same functions the keyboard uses; see
 *  `dispatchAction`. */
export interface MenuActionPayload {
  action: string;
}

/** Payload of the `themes:changed` event: the theme list on disk changed and
 *  `name` is the one to switch to. */
export interface ThemesChangedPayload {
  name: string;
}

/** Payload of the `pty:exit:<sessionId>` event. `exitCode` is 0–255 for a
 *  process that ran to completion, or -1 otherwise — `signal` then names the
 *  signal that killed it (e.g. "SIGHUP").
 *
 *  `exitCode === -1` with an empty `signal` is the *status unknown* case: the
 *  backend's shutdown ladder gave up on a wedged process, so the session is
 *  over but nothing can be said about how it ended. It is not a clean exit. */
export interface PtyExitPayload {
  exitCode: number;
  signal: string;
}

/** Payload of the `files:dropped` event: Wails' native file drop, already
 *  resolved to absolute paths on the Go side. `x`/`y` are the drop point in the
 *  window's CSS pixel space — the macOS webview reports the drag location in
 *  view points with the Y axis flipped to match the web's — which is exactly
 *  what `document.elementFromPoint` expects. */
export interface FilesDroppedPayload {
  x: number;
  y: number;
  paths: string[];
}

export interface SystemStats {
  cpuPercent: number;
  memoryUsedMB: number;
  memoryTotalMB: number;
  memoryPercent: number;
}

export interface HistoryEntry {
  id: number;
  command: string;
  cwd: string;
  exitCode: number;
  shell: string;
  timestamp: number;
  sessionId: string;
}

export interface HistorySearchResult {
  cwdMatches: HistoryEntry[];
  globalMatches: HistoryEntry[];
}

export interface UpdateInfo {
  available: boolean;
  currentVersion: string;
  latestVersion: string;
  url: string;
}

export interface ThemeDTO {
  name: string;
  background: string;
  foreground: string;
  accent: string;
  accentDim: string;
  border: string;
  borderActive: string;
  statusBg: string;
  statusFg: string;
  cursorColor: string;
  selectionBg: string;
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

export interface SessionStatusDTO {
  sessionId: string;
  cwd: string;
  command: string;
  isIdle: boolean;
}

export interface CustomCommandDTO {
  name: string;
  command: string;
  description: string;
  shortcut: string;
  scope: string;
}

export interface WailsRuntime {
  EventsOn(eventName: string, callback: (...args: any[]) => void): () => void;
  /** Like `EventsOn`, but the listener fires at most once and is torn down by
   *  Wails' own dispatch loop (`EventsOnMultiple(..., 1)`: the listener reports
   *  `destroy` from its callback and is spliced out of the list the loop is
   *  about to write back). That is the only removal that survives being decided
   *  during the event's own dispatch — `EventsOff` and the unsubscribe function
   *  below both mutate the live map, which the write-back then overwrites.
   *  Returns the same unsubscribe function as `EventsOn`, for the case where
   *  the event never arrives. */
  EventsOnce(eventName: string, callback: (...args: any[]) => void): () => void;
  EventsOff(eventName: string): void;
  /** Hand a URL to the OS, which opens it in the user's browser. The only way
   *  out of the webview: `window.open` inside a WKWebView with no UI delegate
   *  goes nowhere, and navigating the frame would replace the app. */
  BrowserOpenURL(url: string): void;
}

declare global {
  interface Window {
    go: {
      main: {
        App: WailsApp;
      };
    };
    runtime: WailsRuntime;
  }
}
