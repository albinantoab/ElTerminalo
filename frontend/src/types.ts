import { TerminalPane } from './terminal/TerminalPane';

export interface PaneInfo {
  id: string;
  pane: TerminalPane;
  element: HTMLElement;
}

export interface SplitNode {
  type: 'leaf' | 'split';
  direction?: 'vertical' | 'horizontal';
  ratio?: number;
  paneInfo?: PaneInfo;
  children?: [SplitNode, SplitNode];
}

export interface Tab {
  id: string;
  /** The tab's own name. Only what the *user* typed is authoritative — see
   *  `renamed`; otherwise this is the "Terminalo N" placeholder the tab was
   *  born with, and the last fallback behind the shell's title and its cwd. */
  name: string;
  /** True once the user has renamed this tab by hand. From then on the name
   *  they chose wins over anything the shell reports about itself. */
  renamed: boolean;
  /** A background tab whose bell rang. Cleared the next time it is activated. */
  attention: boolean;
  panes: PaneInfo[];
  activeIndex: number;
  layoutRoot: SplitNode | null;
}

export interface SavedSplitNode {
  type: 'leaf' | 'split';
  direction?: 'vertical' | 'horizontal';
  ratio?: number;
  cwd?: string;
  children?: [SavedSplitNode, SavedSplitNode];
}

export interface SavedTab {
  name: string;
  /** Absent in state written before this existed, which is exactly right:
   *  those tabs were never renamed by hand. */
  renamed?: boolean;
  layout: SavedSplitNode;
}

export interface SavedState {
  version: number;
  themeName: string;
  activeTabIndex: number;
  tabs: SavedTab[];
  layout?: SavedSplitNode; // v1 migration
}

export interface CustomCommand {
  name: string;
  command: string;
  description: string;
  scope: string;
  shortcut: string;
}

export interface PaletteCommand {
  name: string;
  desc: string;
  category: string;
  isCustom?: boolean;
  isTheme?: boolean;
  scope?: string;
  command?: string;
  shortcutDisplay?: string;
  shortcutKey?: string;
  themeData?: Record<string, string>;
  action: (metaKey?: boolean) => void;
}
