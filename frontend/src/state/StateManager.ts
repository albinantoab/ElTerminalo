import { Tab, SavedState, SavedSplitNode, SavedTab, SplitNode } from '../types';
import { STATE_VERSION } from '../constants';
import { logError } from '../log';

export interface StateCallbacks {
  getTabs(): Tab[];
  getActiveTabIndex(): number;
  getCurrentThemeName(): string;
  /** True while the window is still being rebuilt from disk. */
  isRestoring(): boolean;
}

export class StateManager {
  private callbacks: StateCallbacks;
  // The write currently in flight, or null when idle. A save reads every pane's
  // cwd first, so it is slow enough that the 30s autosave routinely overlaps a
  // user-triggered one (divider drag, close pane, the quit handshake); two
  // SaveAppState calls racing each other can leave a corrupt state.json.
  private inFlight: Promise<void> | null = null;
  // The single follow-up write that covers everything asked for while
  // `inFlight` was running, shared by every caller that asked. Awaiting it
  // means resolving only once *this* caller's state has reached disk.
  private queued: Promise<void> | null = null;

  constructor(callbacks: StateCallbacks) {
    this.callbacks = callbacks;
  }

  async save(): Promise<void> {
    // Mid-restore the window is only half rebuilt — the tabs and panes still
    // waiting to be recreated don't exist yet, so writing now would replace the
    // user's real layout with whatever has been reached so far.
    if (this.callbacks.isRestoring()) return;
    if (this.inFlight) {
      // Coalesce: however many saves are asked for while one is writing, they
      // are all served by one more pass afterwards.
      if (!this.queued) {
        // The catch keeps a hypothetical rejection from stranding `queued`
        // non-null, which would kill saving for the rest of the session.
        this.queued = this.inFlight.catch(() => {}).then(() => {
          this.queued = null;
          return this.beginWrite();
        });
      }
      return this.queued;
    }
    return this.beginWrite();
  }

  /** Start one write and publish it as the in-flight one. */
  private beginWrite(): Promise<void> {
    const p = this.writeState().finally(() => {
      if (this.inFlight === p) this.inFlight = null;
    });
    this.inFlight = p;
    return p;
  }

  /** Serialize the window and hand it to the backend. Never rejects: a save
   *  that fails is logged, because the callers that await it (the quit
   *  handshake, a rename) must not be blocked by it. */
  private async writeState(): Promise<void> {
    // Everything, the host callbacks included, is inside the try: they are the
    // host's code, so a throw from one of them would break the promise this
    // method makes to its callers.
    try {
      // Re-checked here because a coalesced write runs later than the save()
      // call that asked for it.
      if (this.callbacks.isRestoring()) return;
      const tabs = this.callbacks.getTabs();
      if (tabs.length === 0) return;
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
      logError('Failed to save the window layout', e);
    }
  }

  async load(): Promise<SavedState | null> {
    try {
      const json = await window.go.main.App.LoadAppState();
      if (!json) return null;
      const state: SavedState = JSON.parse(json);
      return state;
    } catch (e) {
      logError('Failed to load the saved window layout', e);
      return null;
    }
  }

  async serializeLayout(node: SplitNode): Promise<SavedSplitNode> {
    if (node.type === 'leaf' && node.paneInfo) {
      const cwd = await node.paneInfo.pane.getCWD();
      // Omit the key rather than persisting an empty string. This autosave runs
      // every 30s, so writing "" here would durably replace a real folder with
      // $HOME on the next launch — a momentary read failure must never be able
      // to destroy the layout.
      return cwd ? { type: 'leaf', cwd } : { type: 'leaf' };
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
