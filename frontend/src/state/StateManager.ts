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

  /** The window as it stands, in the shape that goes to disk.
   *
   *  Public because a workspace is the same thing under a different name: it is
   *  written by `SaveWorkspace` and read back by `restoreState`'s own code
   *  path, so the two must agree byte for byte — hence one serializer rather
   *  than a second one that drifts. Resolves null when there is nothing worth
   *  saving: no tabs, or a restore still in flight. */
  async serializeState(): Promise<SavedState | null> {
    // Re-checked by every caller's own path too, but this is the one place that
    // can answer it for all of them: mid-restore the window is half built, and
    // what it looks like right now is not a window anyone had.
    if (this.callbacks.isRestoring()) return null;
    const tabs = this.callbacks.getTabs();
    if (tabs.length === 0) return null;
    const savedTabs: SavedTab[] = [];
    for (const tab of tabs) {
      const layout = tab.layoutRoot
        ? await this.serializeLayout(tab.layoutRoot)
        : { type: 'leaf' as const };
      savedTabs.push({ name: tab.name, renamed: tab.renamed, layout });
    }
    return {
      version: STATE_VERSION,
      themeName: this.callbacks.getCurrentThemeName(),
      activeTabIndex: this.callbacks.getActiveTabIndex(),
      tabs: savedTabs,
    };
  }

  /** Serialize the window and hand it to the backend. Never rejects: a save
   *  that fails is logged, because the callers that await it (the quit
   *  handshake, a rename) must not be blocked by it. */
  private async writeState(): Promise<void> {
    // Everything, the host callbacks included, is inside the try: they are the
    // host's code, so a throw from one of them would break the promise this
    // method makes to its callers.
    try {
      const state = await this.serializeState();
      if (!state) return;
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

  /** The same tree, built without asking anyone anything.
   *
   *  Every leaf's directory comes from the pane's own cache — the shell's last
   *  OSC 7 report, or the directory the pane was opened in — so there is no
   *  binding call and nothing to await. That is the whole point: the closed-tab
   *  stack is filled in the same breath as the panes are disposed, and a
   *  snapshot that resolved a tick later would be built from panes that no
   *  longer exist, or land after the Cmd+Shift+T that wanted it. The autosave
   *  keeps using serializeLayout(), which can afford to probe a shell without
   *  integration for the answer. */
  serializeLayoutSync(node: SplitNode): SavedSplitNode {
    if (node.type === 'leaf' && node.paneInfo) {
      const cwd = node.paneInfo.pane.cwd;
      // Same rule as the async pass: no key beats an empty one.
      return cwd ? { type: 'leaf', cwd } : { type: 'leaf' };
    }
    if (node.type === 'split' && node.children) {
      return {
        type: 'split',
        direction: node.direction,
        ratio: node.ratio,
        children: [
          this.serializeLayoutSync(node.children[0]),
          this.serializeLayoutSync(node.children[1]),
        ],
      };
    }
    return { type: 'leaf' };
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
