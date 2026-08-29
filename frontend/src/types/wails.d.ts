// Wails runtime bindings — typed declarations for Go backend methods.

export interface WailsApp {
  CreateSession(cols: number, rows: number, cwd: string): Promise<string>;
  WriteToSession(sessionID: string, data: string): Promise<void>;
  // Wails wraps every bound method in a promise, including the ones that
  // return nothing: a failure here rejects, it does not throw.
  ResizeSession(sessionID: string, cols: number, rows: number): Promise<void>;
  CloseSession(sessionID: string): Promise<void>;
  /** True while the backend still holds this session. The session is dropped
   *  from the backend's map *before* `pty:exit:<id>` is emitted, so the answer
   *  is unambiguous for a caller that has already subscribed: `true` means the
   *  exit event is still to come, `false` means it has already been emitted —
   *  possibly before there was a listener for it. */
  SessionExists(sessionID: string): Promise<boolean>;
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
  SaveDroppedFile(fileName: string, dataBase64: string): Promise<string>;
  ConfirmQuit(): Promise<void>;
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
