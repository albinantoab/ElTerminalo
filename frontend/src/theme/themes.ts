import type { XtermTheme } from '../terminal/TerminalPane';

export interface AppTheme {
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
  xterm: XtermTheme;
}

export function themeFromDTO(dto: any): AppTheme {
  return {
    name: dto.name,
    background: dto.background,
    foreground: dto.foreground,
    accent: dto.accent,
    accentDim: dto.accentDim,
    border: dto.border,
    borderActive: dto.borderActive,
    statusBg: dto.statusBg,
    statusFg: dto.statusFg,
    cursorColor: dto.cursorColor,
    selectionBg: dto.selectionBg,
    xterm: {
      background: dto.background,
      foreground: dto.foreground,
      cursor: dto.cursorColor,
      selectionBackground: dto.selectionBg,
      black: dto.black,
      red: dto.red,
      green: dto.green,
      yellow: dto.yellow,
      blue: dto.blue,
      magenta: dto.magenta,
      cyan: dto.cyan,
      white: dto.white,
      brightBlack: dto.brightBlack,
      brightRed: dto.brightRed,
      brightGreen: dto.brightGreen,
      brightYellow: dto.brightYellow,
      brightBlue: dto.brightBlue,
      brightMagenta: dto.brightMagenta,
      brightCyan: dto.brightCyan,
      brightWhite: dto.brightWhite,
    },
  };
}

export function applyThemeToCSS(theme: AppTheme): void {
  const root = document.documentElement.style;
  root.setProperty('--bg', theme.background);
  root.setProperty('--fg', theme.foreground);
  root.setProperty('--accent', theme.accent);
  root.setProperty('--accent-dim', theme.accentDim);
  root.setProperty('--border', theme.border);
  root.setProperty('--border-active', theme.borderActive);
  root.setProperty('--status-bg', theme.statusBg);
  root.setProperty('--status-fg', theme.statusFg);
  root.setProperty('--cursor-color', theme.cursorColor);
  root.setProperty('--selection-bg', theme.selectionBg);
}
