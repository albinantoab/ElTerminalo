export const MAX_TABS = 9;
export const DOUBLE_CLICK_DELAY_MS = 250;
export const RESIZE_DEBOUNCE_MS = 100;
export const STATE_SAVE_INTERVAL_MS = 30_000;
export const MIN_SPLIT_RATIO = 0.1;
export const MAX_SPLIT_RATIO = 0.9;
export const DEFAULT_SPLIT_RATIO = 0.5;
export const SPATIAL_NAV_THRESHOLD = 10;
export const STATE_VERSION = 2;

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

  // Panes
  SPLIT_VERTICAL:   cmd('Split Vertical',     'Split pane side by side',                  'CMD+|',        'Panes'),
  SPLIT_HORIZONTAL: cmd('Split Horizontal',   'Split pane top/bottom',                    'CMD+-',        'Panes'),
  CLOSE_PANE:       cmd('Close Pane',         'Close the active pane',                    'CMD+X',        'Panes'),
  NEXT_PANE:        cmd('Next Pane',          'Focus the next pane',                      'CMD+→',        'Panes'),
  PREV_PANE:        cmd('Previous Pane',      'Focus the previous pane',                  'CMD+←',        'Panes'),

  // Navigation
  NAV_PREV_COMMAND: cmd('Previous Command',   'Jump to previous command prompt',           'CMD+SHIFT+↑',  'Navigation'),
  NAV_NEXT_COMMAND: cmd('Next Command',       'Jump to next command prompt',               'CMD+SHIFT+↓',  'Navigation'),

  // General
  AI_COMMAND:       cmd('AI Command',         'Generate a shell command from natural language', 'CMD+K',   'General'),
  SESSION_STATUS:   cmd('Session Status',     'Show running commands across all panes',   'CMD+I',        'General'),
  COMMAND_PALETTE:  cmd('Command Palette',    'Open command palette',                     'CMD+P',        'General'),
  CLEAR_TERMINAL:   cmd('Clear Terminal',     'Clear the active terminal',                'CMD+L',        'General'),
  CREATE_COMMAND:   cmd('Create Command',     'Create a custom command',                  'CMD+SHIFT+C',  'Commands'),

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
  'cmd+|': CMD.SPLIT_VERTICAL.name,
  'cmd+-': CMD.SPLIT_HORIZONTAL.name,
  'cmd+x': CMD.CLOSE_PANE.name,
  'cmd+l': CMD.CLEAR_TERMINAL.name,
  'cmd+shift+c': CMD.CREATE_COMMAND.name,
  'cmd+e': CMD.EDIT_COMMAND.name,
  'cmd+d': CMD.DELETE_COMMAND.name,
  'cmd+t': CMD.NEW_TAB.name,
  'cmd+w': CMD.CLOSE_TAB.name,
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
  'cmd+f': 'macOS: Find',
  'cmd+r': 'macOS: Reload',
  'cmd+,': 'macOS: Preferences',
  'cmd+tab': 'macOS: App Switcher',
  'cmd+space': 'macOS: Spotlight',
};
