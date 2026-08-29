import type { Settings } from './types/wails.d.ts';

export const MAX_TABS = 9;
export const DOUBLE_CLICK_DELAY_MS = 250;
export const RESIZE_DEBOUNCE_MS = 100;
export const STATE_SAVE_INTERVAL_MS = 30_000;
export const MIN_SPLIT_RATIO = 0.1;
export const MAX_SPLIT_RATIO = 0.9;
export const DEFAULT_SPLIT_RATIO = 0.5;
export const SPATIAL_NAV_THRESHOLD = 10;
export const STATE_VERSION = 2;

// PTY output flow control. Every byte xterm writes has to be acknowledged or
// the backend stops reading the shell, so acks are coalesced rather than
// skipped: one binding call per 64 KiB written, or per 50ms of quiet, instead
// of one per chunk (a `cat` of a large file arrives in thousands of them).
// The window either threshold has to stay inside is the backend's: it pauses
// at 1 MiB unacknowledged and resumes below 256 KiB.
export const ACK_FLUSH_BYTES = 64 * 1024;
export const ACK_FLUSH_MS = 50;

// One file drop becomes one write to the pty, and a pty write blocks until the
// foreground program reads it — a shell at its prompt does, `vim` mid-edit does
// not. That is true of any large paste and is not this feature's to fix, but a
// drop is the one place the app can produce a huge write on its own, so it caps
// itself: the first this many paths go in and the pane says how many did not.
export const MAX_DROP_PATHS = 200;

// How many closed tabs Cmd+Shift+T can walk back through. Each entry holds a
// name and a tree of directories — nothing heavy — but the point of the stack
// is the last few minutes of work, not the whole session.
export const MAX_CLOSED_TABS = 10;

// Session-only font zoom bounds. Below 8px a terminal stops being readable at
// all; above 40 a split pane holds barely a sentence.
export const MIN_FONT_SIZE = 8;
export const MAX_FONT_SIZE = 40;
// What config.json is allowed to ask for, mirroring the Go side's validation.
export const SETTINGS_FONT_SIZE_MIN = 6;
export const SETTINGS_FONT_SIZE_MAX = 72;

// A keyboard shortcut and the native menu item behind it name the same action.
// WKWebView routes a Cmd-key to one or the other, never both — but "never" is
// a claim about a webview, so the second arrival within this window is dropped.
// Long enough to cover any plausible ordering, short enough that a user cannot
// deliberately trigger the same action twice inside it.
export const MENU_ACTION_DEDUP_MS = 150;

// An OSC 0/2 title is chosen by whatever is on the far end of the pty, and
// ends up in the tab bar and in the native window title. This is as much of it
// as either can show.
export const TITLE_MAX_LENGTH = 160;

// A shell that redraws its prompt reports its title and its directory in the
// same burst, and a `cd` in a loop reports one per iteration. The tab bar is
// rebuilt from scratch each time, so the reports are coalesced.
export const TITLE_REFRESH_DEBOUNCE_MS = 100;

// How long a pane's border stays lit for a visual bell. Long enough to catch
// the eye out of the corner of it, short enough not to read as an error state.
export const BELL_FLASH_MS = 150;

// The shortest gap between two bells the app is willing to act on, per pane.
// xterm throttles nothing: every BEL byte in the stream becomes one onBell, and
// `cat` of a binary is thousands of them in a second — each one otherwise a
// binding call, a system alert sound and a border flash. Bells that land inside
// the window are dropped rather than queued: a bell is a notification, and the
// same notification eight thousand times is still one notification.
export const BELL_COALESCE_MS = 200;

// Find: the shortest term worth lighting every match of. The search addon
// re-scans the whole buffer to build its highlights, and a single character
// matches most of it — the cost is at its highest exactly where the result is
// least useful. One-character terms still search; they just don't decorate.
export const FIND_MIN_HIGHLIGHT_TERM = 2;
// Find: how often the bar samples its pane's output, and the rate above which
// that pane counts as streaming. Two busy samples in a row — more than a second
// of it — is what suspends highlighting; see FindBar.
export const FIND_STREAM_SAMPLE_MS = 500;
export const FIND_STREAM_WRITES_PER_SEC = 10;

// Cmd-keys a modal must let past to the native menu bar.
//
// WKWebView offers a Cmd-key to the page first and re-injects it for the main
// menu only if the page left it un-prevented, so a modal has to cancel the
// combos it does not want — otherwise Cmd+W closes the tab behind it. These are
// the exceptions: the Edit menu role is how Cmd+C/V/X/A reach a focused text
// field in a webview at all, and Quit, Hide and Minimize must work whatever is
// on screen. Everything else the menu binds is one of this app's own actions,
// and those are precisely what a modal must not let through.
export const MODAL_PASSTHROUGH_META_KEYS: ReadonlySet<string> =
  new Set(['a', 'c', 'v', 'x', 'z', 'q', 'h', 'm']);

// Copy-on-select writes the clipboard from xterm's selection-change event,
// which fires on every mouse move that extends a drag. The trailing edge of
// this window is the only write that matters.
export const SELECTION_COPY_DEBOUNCE_MS = 80;

// The drag type a tab carries while being reordered. The document-level drop
// guards test for it: a tab dragged along the tab bar is the app's own gesture,
// not a file or a selection dropped in from outside.
export const TAB_DRAG_MIME = 'application/x-elterminalo-tab';

// What the app runs with when config.json is missing, unreadable, or asks
// for something neither side expects. The same values the Go side defaults to.
export const DEFAULT_SETTINGS: Settings = {
  fontFamily: "'MonaspiceNe NFM', 'SF Mono', 'Menlo', monospace",
  fontSize: 12,
  lineHeight: 1.2,
  cursorStyle: 'block',
  cursorBlink: true,
  scrollback: 10000,
  shell: '',
  optionIsMeta: true,
  bell: 'sound',
  copyOnSelect: true,
  confirmQuit: 'running',
};

// ── Command Registry ──
// Single source of truth for every command's name, description, shortcut, and category.
// Every UI surface (palette, context menu, status bar, hints) reads from here.

interface CommandDef {
  name: string;
  desc: string;
  shortcut: string;
  category: string;
}

function cmd(name: string, desc: string, shortcut: string, category: string): CommandDef {
  return { name, desc, shortcut, category };
}

export const CMD = {
  // Tabs
  NEW_TAB:          cmd('New Tab',            'Open a new terminal tab',                  'CMD+T',        'Tabs'),
  CLOSE_TAB:        cmd('Close Tab',          'Close current tab',                        'CMD+W',        'Tabs'),
  RENAME_TAB:       cmd('Rename Tab',         'Rename current tab',                       '',             'Tabs'),
  NEXT_TAB:         cmd('Next Tab',           'Switch to the tab on the right',           'CMD+SHIFT+]',  'Tabs'),
  PREV_TAB:         cmd('Previous Tab',       'Switch to the tab on the left',            'CMD+SHIFT+[',  'Tabs'),
  REOPEN_TAB:       cmd('Reopen Closed Tab',  'Bring back the tab that was closed last',  'CMD+SHIFT+T',  'Tabs'),

  // Panes
  // Cmd+D / Cmd+Shift+D follow iTerm2 and Ghostty. Cmd+- used to split
  // horizontally and is now Zoom Out; Cmd+| stays as an alias for a vertical
  // split, because it is the one that looks like what it does.
  SPLIT_VERTICAL:   cmd('Split Vertical',     'Split pane side by side',                  'CMD+D',        'Panes'),
  SPLIT_HORIZONTAL: cmd('Split Horizontal',   'Split pane top/bottom',                    'CMD+SHIFT+D',  'Panes'),
  CLOSE_PANE:       cmd('Close Pane',         'Close the active pane',                    'CMD+SHIFT+X',  'Panes'),
  NEXT_PANE:        cmd('Next Pane',          'Focus the next pane',                      'CMD+→',        'Panes'),
  PREV_PANE:        cmd('Previous Pane',      'Focus the previous pane',                  'CMD+←',        'Panes'),

  // Navigation
  NAV_PREV_COMMAND: cmd('Previous Command',   'Jump to previous command prompt',           'CMD+SHIFT+↑',  'Navigation'),
  NAV_NEXT_COMMAND: cmd('Next Command',       'Jump to next command prompt',               'CMD+SHIFT+↓',  'Navigation'),

  // General
  SEARCH_HISTORY:   cmd('Search History',     'Search command history',                   'CMD+SHIFT+R',  'General'),
  AI_COMMAND:       cmd('AI Command',         'Generate a shell command from natural language', 'CMD+K',   'General'),
  SESSION_STATUS:   cmd('Session Status',     'Show running commands across all panes',   'CMD+I',        'General'),
  COMMAND_PALETTE:  cmd('Command Palette',    'Open command palette',                     'CMD+P',        'General'),
  CLEAR_TERMINAL:   cmd('Clear Terminal',     'Clear the active terminal',                'CMD+L',        'General'),
  FIND:             cmd('Find',               'Search this pane\'s scrollback',            'CMD+F',        'General'),
  REVEAL_LOGS:      cmd('Reveal Logs',        'Show the app log file in Finder',          '',             'General'),
  CREATE_COMMAND:   cmd('Create Command',     'Create a custom command',                  'CMD+SHIFT+C',  'Commands'),

  // Appearance
  ZOOM_IN:          cmd('Zoom In',            'Increase the terminal font size',          'CMD+=',        'Appearance'),
  ZOOM_OUT:         cmd('Zoom Out',           'Decrease the terminal font size',          'CMD+-',        'Appearance'),
  ZOOM_RESET:       cmd('Reset Zoom',         'Back to the font size in settings',        'CMD+0',        'Appearance'),
  IMPORT_SCHEME:    cmd('Import Color Scheme', 'Load an iTerm2 .itermcolors or Ghostty theme file', '',   'Appearance'),

  // Settings
  OPEN_SETTINGS:    cmd('Open Settings',      'Open config.json in your editor',          '',             'Settings'),
  RELOAD_SETTINGS:  cmd('Reload Settings',    'Re-read config.json and apply it live',    '',             'Settings'),

  // AI
  UPDATE_MODEL:     cmd('Update AI Model',    'A newer model version is available',       '',             'AI'),

  // Context menu / system
  COPY:             cmd('Copy',               'Copy selection',                           'CMD+C',        'Edit'),
  PASTE:            cmd('Paste',              'Paste from clipboard',                     'CMD+V',        'Edit'),
  SELECT_ALL:       cmd('Select All',         'Select all text',                          'CMD+A',        'Edit'),
  COPY_LAST_OUTPUT: cmd('Copy Last Output',   'Copy the last command output to clipboard','',             'General'),

  // Palette hint keys (not full commands, just display labels)
  EDIT_COMMAND:     cmd('Edit',               'Edit selected command',                    'CMD+E',        ''),
  DELETE_COMMAND:    cmd('Delete',             'Delete selected command',                  'CMD+D',        ''),
  FILL:             cmd('Fill',               'Fill without executing',                   'CMD+ENTER',    ''),
} as const;

// Internal lookup keys for conflict detection (lowercase for key-event matching).
export const BUILT_IN_SHORTCUTS: Record<string, string> = {
  'cmd+k': CMD.AI_COMMAND.name,
  'cmd+i': CMD.SESSION_STATUS.name,
  'cmd+p': CMD.COMMAND_PALETTE.name,
  'cmd+d': CMD.SPLIT_VERTICAL.name,
  'cmd+|': CMD.SPLIT_VERTICAL.name,
  'cmd+shift+d': CMD.SPLIT_HORIZONTAL.name,
  'cmd+shift+x': CMD.CLOSE_PANE.name,
  'cmd+l': CMD.CLEAR_TERMINAL.name,
  'cmd+f': CMD.FIND.name,
  'cmd+g': 'Find Next',
  'cmd+shift+g': 'Find Previous',
  'cmd+shift+c': CMD.CREATE_COMMAND.name,
  'cmd+e': CMD.EDIT_COMMAND.name,
  'cmd+t': CMD.NEW_TAB.name,
  'cmd+w': CMD.CLOSE_TAB.name,
  'cmd+shift+t': CMD.REOPEN_TAB.name,
  // Both spellings of each: the wizard records whatever `KeyboardEvent.key`
  // reports, and with Shift held a US layout reports `}` and `{`, not `]`/`[`.
  'cmd+shift+]': CMD.NEXT_TAB.name,
  'cmd+shift+}': CMD.NEXT_TAB.name,
  'cmd+shift+[': CMD.PREV_TAB.name,
  'cmd+shift+{': CMD.PREV_TAB.name,
  'ctrl+tab': CMD.NEXT_TAB.name,
  'ctrl+shift+tab': CMD.PREV_TAB.name,
  // Same story: Cmd+Shift+= reports `+`.
  'cmd+=': CMD.ZOOM_IN.name,
  'cmd+shift++': CMD.ZOOM_IN.name,
  'cmd+-': CMD.ZOOM_OUT.name,
  'cmd+0': CMD.ZOOM_RESET.name,
  'cmd+1': 'Switch to Tab 1',
  'cmd+2': 'Switch to Tab 2',
  'cmd+3': 'Switch to Tab 3',
  'cmd+4': 'Switch to Tab 4',
  'cmd+5': 'Switch to Tab 5',
  'cmd+6': 'Switch to Tab 6',
  'cmd+7': 'Switch to Tab 7',
  'cmd+8': 'Switch to Tab 8',
  'cmd+9': 'Switch to Tab 9',
  'cmd+arrowright': CMD.NEXT_PANE.name,
  'cmd+arrowleft': CMD.PREV_PANE.name,
  'cmd+arrowup': 'Pane Above',
  'cmd+arrowdown': 'Pane Below',
};

export const SYSTEM_SHORTCUTS: Record<string, string> = {
  'cmd+a': 'macOS: Select All',
  'cmd+c': 'macOS: Copy',
  'cmd+v': 'macOS: Paste',
  'cmd+z': 'macOS: Undo',
  'cmd+shift+z': 'macOS: Redo',
  'cmd+s': 'macOS: Save',
  'cmd+o': 'macOS: Open',
  'cmd+n': 'macOS: New Window',
  'cmd+q': 'macOS: Quit',
  'cmd+m': 'macOS: Minimize',
  'cmd+h': 'macOS: Hide',
  'cmd+r': 'macOS: Reload',
  'cmd+,': 'macOS: Preferences',
  'cmd+tab': 'macOS: App Switcher',
  'cmd+space': 'macOS: Spotlight',
};
