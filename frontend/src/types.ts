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
  name: string;
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
