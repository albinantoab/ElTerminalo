// Wails runtime bindings — typed declarations for Go backend methods.

export interface WailsApp {
  CreateSession(cols: number, rows: number, cwd: string): Promise<string>;
  WriteToSession(sessionID: string, data: string): Promise<void>;
  ResizeSession(sessionID: string, cols: number, rows: number): void;
  CloseSession(sessionID: string): void;
  GetSessionCWD(sessionID: string): Promise<string>;
  GetAllSessionCWDs(): Promise<Record<string, string>>;
  GetThemes(): Promise<ThemeDTO[]>;
  SaveAppState(stateJSON: string): Promise<void>;
  LoadAppState(): Promise<string>;
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
