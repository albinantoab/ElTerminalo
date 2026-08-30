import { TerminalPane } from './terminal/TerminalPane';

export interface PaneInfo {
  /** Unique for the life of the window, and the `paneKey` a native
   *  notification carries so a click on it can come back to this exact pane. */
  id: string;
  pane: TerminalPane;
  element: HTMLElement;
  /** The shell asked for the user while they were looking somewhere else — a
   *  bell, an OSC 9 or an OSC 777 notification. Cleared when the pane becomes
   *  the active one of the visible tab. */
  needsAttention: boolean;
  /** What the last such request said, kept for the native notification and for
   *  the marker's tooltip. */
  attentionTitle: string;
  attentionBody: string;
  /** `performance.now()` of the last native notification posted for this pane,
   *  or 0. One pane cannot post more often than NOTIFY_PER_PANE_MS. */
  lastNotifyAt: number;
  /** Where this pane's transcript is being written, or '' when it is not
   *  recording, together with the session it was started for — the session id
   *  is gone from the pane by the time an exit has to stop the recording. */
  transcriptPath: string;
  transcriptSessionId: string;
}

/** Where a pane's shell is in the command it is running, as told by the OSC 133
 *  marks. `done`/`failed` describe the *last* command; a pane that has never
 *  run one, or whose shell has no integration at all, stays `idle`. */
export type CommandStatus = 'idle' | 'running' | 'done' | 'failed';

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
  /** What finished in this tab while the user was not looking at it: 'failed'
   *  wins over 'done', and both are cleared the next time the tab is visited.
   *  A command *running* is not recorded here — that is read live off the
   *  panes, because it stops being true on its own. */
  finished: 'done' | 'failed' | null;
  panes: PaneInfo[];
  activeIndex: number;
  layoutRoot: SplitNode | null;
  /** The one pane filling this tab's whole pane area, or null. Deliberately
   *  outside `layoutRoot`: zooming hides the other panes, it does not
   *  restructure the split tree, and nothing about it is persisted. */
  zoomedPaneId: string | null;
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
