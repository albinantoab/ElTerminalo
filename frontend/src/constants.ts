export const MAX_TABS = 9;
export const DOUBLE_CLICK_DELAY_MS = 250;
export const RESIZE_DEBOUNCE_MS = 100;
export const STATE_SAVE_INTERVAL_MS = 30_000;
export const MIN_SPLIT_RATIO = 0.1;
export const MAX_SPLIT_RATIO = 0.9;
export const DEFAULT_SPLIT_RATIO = 0.5;
export const SPATIAL_NAV_THRESHOLD = 10;
export const STATE_VERSION = 2;

export const BUILT_IN_SHORTCUTS: Record<string, string> = {
  'cmd+i': 'Session Status',
  'cmd+p': 'Command Palette',
  'cmd+b': 'Split Vertical',
  'cmd+g': 'Split Horizontal',
  'cmd+x': 'Close Pane',
  'cmd+l': 'Clear Terminal',
  'cmd+shift+c': 'Create Command',
  'cmd+e': 'Edit Command (in palette)',
  'cmd+d': 'Delete Command (in palette)',
  'cmd+t': 'New Tab',
  'cmd+w': 'Close Tab',
  'cmd+1': 'Switch to Tab 1',
  'cmd+2': 'Switch to Tab 2',
  'cmd+3': 'Switch to Tab 3',
  'cmd+4': 'Switch to Tab 4',
  'cmd+5': 'Switch to Tab 5',
  'cmd+6': 'Switch to Tab 6',
  'cmd+7': 'Switch to Tab 7',
  'cmd+8': 'Switch to Tab 8',
  'cmd+9': 'Switch to Tab 9',
  'cmd+arrowright': 'Next Pane',
  'cmd+arrowleft': 'Previous Pane',
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
