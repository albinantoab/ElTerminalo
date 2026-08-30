import type { WorkspaceInfo } from '../types/wails.d.ts';
import { escHtml } from '../utils';
import { logError } from '../log';

export interface WorkspaceModalCallbacks {
  /** What the name field is pre-filled with — the active tab's name. */
  getDefaultName(): string;
  focusActivePane(): void;
  /** Persist the current window under this name. */
  onSave(name: string): Promise<void>;
  /** Rebuild the whole window from a saved workspace. */
  onOpen(slug: string): Promise<void>;
  /** Say something in the pane the user is looking at. */
  notice(text: string): void;
  /** The host's own confirmation dialog, so a delete looks like every other
   *  destructive action in this app. */
  confirm(title: string, body: string, confirmLabel: string, onConfirm: () => void): void;
}

type Mode = 'save' | 'open';

/** Save Workspace… / Open Workspace…
 *
 *  One class, two faces, because they are the same list of names seen from
 *  either end — and because the app has one modal idiom (a class over its own
 *  overlay, `handleKeydown` fed by the host, `hide()` putting the keyboard back
 *  in the terminal) and a second dialog class would be a third of it copied. */
export class WorkspaceModal {
  private open = false;
  private mode: Mode = 'open';
  private cursor = 0;
  private name = '';
  private workspaces: WorkspaceInfo[] = [];
  private overlay: HTMLElement;
  private callbacks: WorkspaceModalCallbacks;
  private delegationAttached = false;

  constructor(overlay: HTMLElement, callbacks: WorkspaceModalCallbacks) {
    this.overlay = overlay;
    this.callbacks = callbacks;
  }

  async show(mode: Mode): Promise<void> {
    this.mode = mode;
    this.cursor = 0;
    this.name = mode === 'save' ? this.callbacks.getDefaultName() : '';
    this.workspaces = [];
    // The list is what "Open" *is*, and it is also what tells "Save" which
    // names are already taken, so both modes fetch it.
    await this.refresh();
    // Only now: a failed fetch says so in the pane rather than opening an empty
    // dialog over it.
    this.open = true;
    this.overlay.innerHTML = '';
    this.render();
    this.overlay.classList.remove('hidden');
    requestAnimationFrame(() => {
      (this.overlay.querySelector('.workspace-input') as HTMLInputElement | null)?.select();
    });
  }

  hide(): void {
    this.open = false;
    this.overlay.classList.add('hidden');
    this.callbacks.focusActivePane();
  }

  isOpen(): boolean {
    return this.open;
  }

  handleKeydown(e: KeyboardEvent): boolean {
    if (!this.open) return false;
    // The host's confirm dialog is layered over this one and installs its own
    // handler on the document, which runs *after* this window-level one. While
    // it is up the keyboard is its, so nothing here acts — but the key is still
    // claimed, so it cannot fall through to a shortcut either.
    if (document.querySelector('.confirm-overlay')) return true;

    if (e.key === 'Escape') {
      e.preventDefault();
      this.hide();
      return true;
    }

    if (this.mode === 'save') {
      if (e.key === 'Enter') {
        e.preventDefault();
        void this.save();
      }
      return true;
    }

    if (e.key === 'ArrowUp') {
      e.preventDefault();
      this.cursor = Math.max(0, this.cursor - 1);
      this.updateCursor();
      return true;
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      this.cursor = Math.min(this.workspaces.length - 1, this.cursor + 1);
      this.updateCursor();
      return true;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      const ws = this.workspaces[this.cursor];
      if (ws) {
        this.hide();
        this.callbacks.onOpen(ws.slug).catch(e2 => logError(`Failed to open workspace "${ws.name}"`, e2));
      }
      return true;
    }
    if (e.metaKey && e.key === 'Backspace') {
      e.preventDefault();
      const ws = this.workspaces[this.cursor];
      if (ws) {
        this.callbacks.confirm(
          'Delete Workspace?',
          `"${escHtml(ws.name)}" will be deleted. The tabs it describes are not affected.`,
          'Delete',
          () => { void this.remove(ws); },
        );
      }
      return true;
    }
    return true;
  }

  private async refresh(): Promise<void> {
    try {
      this.workspaces = (await window.go.main.App.ListWorkspaces()) || [];
    } catch (e) {
      logError('Failed to list the saved workspaces', e);
      this.workspaces = [];
      this.callbacks.notice('[Could not read the saved workspaces]');
    }
    this.cursor = Math.min(this.cursor, Math.max(0, this.workspaces.length - 1));
  }

  private async save(): Promise<void> {
    const trimmed = this.name.trim();
    if (!trimmed) return;
    this.hide();
    try {
      await this.callbacks.onSave(trimmed);
    } catch (e) {
      logError(`Failed to save the workspace "${trimmed}"`, e);
      this.callbacks.notice(`[Could not save the workspace: ${e instanceof Error ? e.message : String(e)}]`);
    }
  }

  private async remove(ws: WorkspaceInfo): Promise<void> {
    try {
      await window.go.main.App.DeleteWorkspace(ws.slug);
    } catch (e) {
      logError(`Failed to delete the workspace "${ws.name}"`, e);
      this.callbacks.notice(`[Could not delete "${ws.name}": ${e instanceof Error ? e.message : String(e)}]`);
      return;
    }
    // The dialog can be gone by the time the delete lands — Escape, or a
    // workspace opened from under it.
    if (!this.open) return;
    await this.refresh();
    this.render();
  }

  private updateCursor(): void {
    const rows = this.overlay.querySelectorAll('.workspace-row');
    rows.forEach((row, i) => row.classList.toggle('selected', i === this.cursor));
    this.overlay.querySelector('.workspace-row.selected')?.scrollIntoView({ block: 'nearest' });
  }

  private render(): void {
    try {
      const list = this.mode === 'open' ? this.overlay.querySelector('.workspace-list') : null;
      if (list) {
        // Re-render after a delete without rebuilding the box the user is
        // looking at.
        list.innerHTML = this.renderRows();
      } else {
        this.overlay.innerHTML = this.mode === 'save' ? this.renderSave() : this.renderOpen();
        const input = this.overlay.querySelector('.workspace-input') as HTMLInputElement | null;
        if (input) {
          input.oninput = (e) => { this.name = (e.target as HTMLInputElement).value; };
        }
      }

      this.overlay.querySelector('.workspace-row.selected')?.scrollIntoView({ block: 'nearest' });

      if (!this.delegationAttached) {
        this.delegationAttached = true;
        this.overlay.addEventListener('click', (e) => {
          const row = (e.target as HTMLElement).closest('.workspace-row[data-index]');
          if (!row) return;
          const idx = parseInt(row.getAttribute('data-index') || '0', 10);
          const ws = this.workspaces[idx];
          if (!ws) return;
          this.hide();
          this.callbacks.onOpen(ws.slug).catch(e2 => logError(`Failed to open workspace "${ws.name}"`, e2));
        });
      }
    } catch (e) {
      logError('Workspace modal failed to render', e);
      this.overlay.innerHTML = '<div class="error">Something went wrong</div>';
    }
  }

  private renderSave(): string {
    const existing = this.workspaces.length > 0
      ? `<div class="workspace-existing">${this.workspaces.map(w => escHtml(w.name)).join(' · ')}</div>`
      : '';
    return `<div class="workspace-box">
      <div class="workspace-header">Save Workspace</div>
      <input class="workspace-input" type="text" placeholder="Workspace name" value="${escHtml(this.name)}" />
      <div class="workspace-note">Saves every tab and pane in this window, and the folder each one is in.</div>
      ${existing}
      <div class="workspace-footer"><kbd>ENTER</kbd> save · <kbd>ESC</kbd> cancel</div>
    </div>`;
  }

  private renderOpen(): string {
    return `<div class="workspace-box">
      <div class="workspace-header">Open Workspace</div>
      <div class="workspace-list">${this.renderRows()}</div>
      <div class="workspace-footer"><kbd>ENTER</kbd> open · <kbd>CMD+DELETE</kbd> delete · <kbd>ESC</kbd> close</div>
    </div>`;
  }

  private renderRows(): string {
    if (this.workspaces.length === 0) {
      return '<div class="workspace-empty">No saved workspaces</div>';
    }
    return this.workspaces.map((w, i) => {
      const selected = i === this.cursor ? ' selected' : '';
      const counts = `${w.tabs} tab${w.tabs === 1 ? '' : 's'} · ${w.panes} pane${w.panes === 1 ? '' : 's'}`;
      return `<div class="workspace-row${selected}" data-index="${i}">
        <div class="workspace-row-text">
          <span class="workspace-name">${escHtml(w.name)}</span>
          <span class="workspace-meta">${escHtml(counts)}</span>
        </div>
        <span class="workspace-time">${escHtml(relativeTime(w.savedUnix))}</span>
      </div>`;
    }).join('');
  }
}

/** "just now", "4m ago", "3d ago" — the same scale the history modal uses. */
function relativeTime(unixSeconds: number): string {
  if (!unixSeconds || !Number.isFinite(unixSeconds)) return '';
  const diff = Math.floor(Date.now() / 1000) - unixSeconds;
  if (diff < 0) return 'just now';
  if (diff < 60) return 'just now';
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}
