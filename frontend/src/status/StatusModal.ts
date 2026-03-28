import { Tab } from '../types';
import { escHtml } from '../utils';

export interface StatusModalCallbacks {
  getTabs(): Tab[];
  getActiveTabIndex(): number;
  focusActivePane(): void;
  switchToPane(tabIndex: number, paneIndex: number): void;
}

interface StatusEntry {
  tabName: string;
  tabIndex: number;
  paneIndex: number;
  paneLabel: string;
  command: string;
  cwd: string;
  isIdle: boolean;
  isActiveTab: boolean;
}

export class StatusModal {
  private open = false;
  private cursor = 0;
  private overlay: HTMLElement;
  private callbacks: StatusModalCallbacks;
  private refreshTimer: ReturnType<typeof setInterval> | null = null;
  private entries: StatusEntry[] = [];
  private delegationAttached = false;

  constructor(overlay: HTMLElement, callbacks: StatusModalCallbacks) {
    this.overlay = overlay;
    this.callbacks = callbacks;
  }

  async show(): Promise<void> {
    if (this.refreshTimer) {
      clearInterval(this.refreshTimer);
      this.refreshTimer = null;
    }
    this.open = true;
    this.cursor = 0;
    await this.fetchStatus();
    this.overlay.classList.remove('hidden');
    this.refreshTimer = setInterval(() => this.fetchStatus(), 2000);
  }

  hide(): void {
    this.open = false;
    this.overlay.classList.add('hidden');
    if (this.refreshTimer) {
      clearInterval(this.refreshTimer);
      this.refreshTimer = null;
    }
    this.callbacks.focusActivePane();
  }

  isOpen(): boolean {
    return this.open;
  }

  handleKeydown(e: KeyboardEvent): boolean {
    if (!this.open) return false;
    if (e.key === 'Escape') {
      e.preventDefault();
      this.hide();
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
      this.cursor = Math.min(this.entries.length - 1, this.cursor + 1);
      this.updateCursor();
      return true;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      const entry = this.entries[this.cursor];
      if (entry) {
        this.hide();
        this.callbacks.switchToPane(entry.tabIndex, entry.paneIndex);
      }
      return true;
    }
    return true;
  }

  private async fetchStatus(): Promise<void> {
    try {
      const statuses = await window.go.main.App.GetAllSessionStatuses();
      const tabs = this.callbacks.getTabs();
      const activeTabIndex = this.callbacks.getActiveTabIndex();

      const prevCursor = this.cursor;
      this.entries = [];
      for (let ti = 0; ti < tabs.length; ti++) {
        const tab = tabs[ti];
        for (let pi = 0; pi < tab.panes.length; pi++) {
          const pane = tab.panes[pi];
          const sid = pane.pane.sessionId;
          const status = statuses[sid];
          const paneLabel = String.fromCharCode(65 + pi);
          this.entries.push({
            tabName: tab.name,
            tabIndex: ti,
            paneIndex: pi,
            paneLabel,
            command: status?.command || '',
            cwd: this.shortenPath(status?.cwd || ''),
            isIdle: status?.isIdle ?? true,
            isActiveTab: ti === activeTabIndex,
          });
        }
      }
      // Keep cursor in bounds after refresh
      this.cursor = Math.min(prevCursor, Math.max(0, this.entries.length - 1));
      this.render();
    } catch (e) {
      console.error('Failed to fetch session status:', e);
    }
  }

  private shortenPath(cwd: string): string {
    try {
      if (!cwd || typeof cwd !== 'string') return '';
      const home = cwd.match(/^\/Users\/[^/]+/)?.[0] || cwd.match(/^\/home\/[^/]+/)?.[0];
      if (home && cwd.startsWith(home)) {
        return '~' + cwd.slice(home.length);
      }
      return cwd;
    } catch {
      return cwd || '';
    }
  }

  private updateCursor(): void {
    const rows = this.overlay.querySelectorAll('.status-modal-row');
    rows.forEach((row, i) => {
      row.classList.toggle('selected', i === this.cursor);
    });
    this.overlay.querySelector('.status-modal-row.selected')?.scrollIntoView({ block: 'nearest' });
  }

  private render(): void {
    try {
      let lastTabIndex = -1;
      const rows = this.entries.map((e, i) => {
        let groupHeader = '';
        if (e.tabIndex !== lastTabIndex) {
          lastTabIndex = e.tabIndex;
          const activeTag = e.isActiveTab ? ' <span class="status-modal-active-tag">active</span>' : '';
          groupHeader = `<div class="status-modal-group">${escHtml(e.tabName)}${activeTag}</div>`;
        }

        const selected = i === this.cursor ? ' selected' : '';
        const indicator = e.isIdle
          ? '<span class="status-modal-dot idle"></span>'
          : '<span class="status-modal-dot running"></span>';

        const cmdDisplay = e.isIdle
          ? '<span class="status-modal-idle">idle</span>'
          : `<span class="status-modal-cmd">${escHtml(e.command)}</span>`;

        return `${groupHeader}<div class="status-modal-row${selected}" data-index="${i}">
          <div class="status-modal-left">
            ${indicator}
            <span class="status-modal-pane">Pane ${escHtml(e.paneLabel)}</span>
          </div>
          <div class="status-modal-mid">${cmdDisplay}</div>
          <div class="status-modal-right">${escHtml(e.cwd)}</div>
        </div>`;
      }).join('');

      const empty = this.entries.length === 0
        ? '<div class="status-modal-empty">No active sessions</div>'
        : '';

      const existingList = this.overlay.querySelector('.status-modal-list');
      if (existingList) {
        existingList.innerHTML = rows || empty;
      } else {
        this.overlay.innerHTML = `<div class="status-modal-box">
          <div class="status-modal-header">
            <span class="status-modal-title">Session Status</span>
            <span class="status-modal-refresh">auto-refreshing</span>
          </div>
          <div class="status-modal-list">${rows || empty}</div>
          <div class="status-modal-footer"><kbd>↑↓</kbd> navigate · <kbd>Enter</kbd> jump · <kbd>Esc</kbd> close</div>
        </div>`;
      }

      this.overlay.querySelector('.status-modal-row.selected')?.scrollIntoView({ block: 'nearest' });

      if (!this.delegationAttached) {
        this.delegationAttached = true;
        this.overlay.addEventListener('click', (e) => {
          const row = (e.target as HTMLElement).closest('.status-modal-row[data-index]');
          if (!row) return;
          const idx = parseInt(row.getAttribute('data-index') || '0');
          const entry = this.entries[idx];
          if (entry) {
            this.hide();
            this.callbacks.switchToPane(entry.tabIndex, entry.paneIndex);
          }
        });
      }
    } catch (e) {
      console.error('Render error:', e);
      this.overlay.innerHTML = '<div class="error">Something went wrong</div>';
    }
  }
}
