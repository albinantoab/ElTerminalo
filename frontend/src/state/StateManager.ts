import { Tab, SavedState, SavedSplitNode, SavedTab, SplitNode } from '../types';
import { STATE_VERSION } from '../constants';

export interface StateCallbacks {
  getTabs(): Tab[];
  getActiveTabIndex(): number;
  getCurrentThemeName(): string;
}

export class StateManager {
  private callbacks: StateCallbacks;

  constructor(callbacks: StateCallbacks) {
    this.callbacks = callbacks;
  }

  async save(): Promise<void> {
    const tabs = this.callbacks.getTabs();
    if (tabs.length === 0) return;
    try {
      const savedTabs: SavedTab[] = [];
      for (const tab of tabs) {
        const layout = tab.layoutRoot
          ? await this.serializeLayout(tab.layoutRoot)
          : { type: 'leaf' as const };
        savedTabs.push({ name: tab.name, layout });
      }
      const state: SavedState = {
        version: STATE_VERSION,
        themeName: this.callbacks.getCurrentThemeName(),
        activeTabIndex: this.callbacks.getActiveTabIndex(),
        tabs: savedTabs,
      };
      await window.go.main.App.SaveAppState(JSON.stringify(state));
    } catch (e) {
      console.error('Failed to save state:', e);
    }
  }

  async load(): Promise<SavedState | null> {
    try {
      const json = await window.go.main.App.LoadAppState();
      if (!json) return null;
      const state: SavedState = JSON.parse(json);
      return state;
    } catch (e) {
      console.error('Failed to load state:', e);
      return null;
    }
  }

  async serializeLayout(node: SplitNode): Promise<SavedSplitNode> {
    if (node.type === 'leaf' && node.paneInfo) {
      const cwd = await node.paneInfo.pane.getCWD();
      return { type: 'leaf', cwd };
    }
    if (node.type === 'split' && node.children) {
      const [a, b] = await Promise.all([
        this.serializeLayout(node.children[0]),
        this.serializeLayout(node.children[1]),
      ]);
      return { type: 'split', direction: node.direction, ratio: node.ratio, children: [a, b] };
    }
    return { type: 'leaf' };
  }
}
