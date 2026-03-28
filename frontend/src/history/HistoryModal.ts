import type { HistoryEntry } from '../types/wails.d.ts';
import { escHtml, utf8ToBase64 } from '../utils';
import { CMD } from '../constants';

export interface HistoryModalCallbacks {
  getActiveSessionId(): string;
  getActivePaneCWD(): Promise<string>;
  focusActivePane(): void;
}

export class HistoryModal {
  private open = false;
  private query = '';
  private cursor = 0;
  private cwdEntries: HistoryEntry[] = [];
  private globalEntries: HistoryEntry[] = [];
  private overlay: HTMLElement;
  private callbacks: HistoryModalCallbacks;
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;
  private delegationAttached = false;

  constructor(overlay: HTMLElement, callbacks: HistoryModalCallbacks) {
    this.overlay = overlay;
    this.callbacks = callbacks;
  }

  async show(): Promise<void> {
    this.open = true;
    this.query = '';
    this.cursor = 0;
    await this.fetchResults();
    this.overlay.classList.remove('hidden');
    requestAnimationFrame(() => {
      (this.overlay.querySelector('.history-input') as HTMLInputElement)?.focus();
    });
  }

  hide(): void {
    if (this.debounceTimer) {
      clearTimeout(this.debounceTimer);
      this.debounceTimer = null;
    }
    this.open = false;
    this.overlay.classList.add('hidden');
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

    const all = this.getAllEntries();

    if (e.key === 'ArrowUp') {
      e.preventDefault();
      this.cursor = Math.max(0, this.cursor - 1);
      this.updateCursor();
      return true;
    }

    if (e.key === 'ArrowDown') {
      e.preventDefault();
      this.cursor = Math.min(all.length - 1, this.cursor + 1);
      this.updateCursor();
      return true;
    }

    if (e.key === 'Enter') {
      e.preventDefault();
      const entry = all[this.cursor];
      if (entry) {
        const sessionId = this.callbacks.getActiveSessionId();
        if (sessionId) {
          const data = e.metaKey ? entry.command + '\n' : entry.command;
          window.go.main.App.WriteToSession(sessionId, utf8ToBase64(data));
        }
        this.hide();
      }
      return true;
    }

    return true;
  }

  private async fetchResults(): Promise<void> {
    const cwd = await this.callbacks.getActivePaneCWD();
    const result = await window.go.main.App.SearchHistory(this.query, cwd, 50);
    this.cwdEntries = result.cwdMatches || [];
    this.globalEntries = result.globalMatches || [];
    this.cursor = 0;
    this.render();
  }

  private getAllEntries(): HistoryEntry[] {
    return [...this.cwdEntries, ...this.globalEntries];
  }

  private updateCursor(): void {
    const rows = this.overlay.querySelectorAll('.history-row');
    rows.forEach((row, i) => {
      row.classList.toggle('selected', i === this.cursor);
    });
    this.overlay.querySelector('.history-row.selected')?.scrollIntoView({ block: 'nearest' });
  }

  private render(): void {
    try {
      const MAX_DISPLAY = 200;
      const all = this.getAllEntries();
      let idx = 0;
      let items = '';
      let truncated = false;

      if (this.cwdEntries.length > 0) {
        items += '<div class="history-group">Current Directory</div>';
        for (const entry of this.cwdEntries) {
          if (idx >= MAX_DISPLAY) { truncated = true; break; }
          items += this.renderRow(entry, idx, false);
          idx++;
        }
      }

      if (!truncated && this.globalEntries.length > 0) {
        items += '<div class="history-group">All History</div>';
        for (const entry of this.globalEntries) {
          if (idx >= MAX_DISPLAY) { truncated = true; break; }
          items += this.renderRow(entry, idx, true);
          idx++;
        }
      }

      if (truncated) {
        items += `<div class="history-empty">Showing first ${MAX_DISPLAY} results. Refine your search for more.</div>`;
      }

      if (all.length === 0) {
        items = '<div class="history-empty">No history found</div>';
      }

      const existingList = this.overlay.querySelector('.history-list');
      if (existingList) {
        // Update only the list — preserve input focus
        existingList.innerHTML = items;
      } else {
        // First render — build entire DOM
        this.overlay.innerHTML = `
          <div class="history-box">
            <input class="history-input" type="text" placeholder="Search command history..." value="${escHtml(this.query)}" />
            <div class="history-list">${items}</div>
            <div class="history-footer">
              <kbd>ENTER</kbd> paste · <kbd>${escHtml(CMD.FILL.shortcut)}</kbd> execute · <kbd>ESC</kbd> close
            </div>
          </div>
        `;

        const input = this.overlay.querySelector('.history-input') as HTMLInputElement;
        if (input) {
          input.oninput = (e) => {
            this.query = (e.target as HTMLInputElement).value;
            if (this.debounceTimer) clearTimeout(this.debounceTimer);
            this.debounceTimer = setTimeout(() => this.fetchResults(), 150);
          };
        }
      }

      this.overlay.querySelector('.history-row.selected')?.scrollIntoView({ block: 'nearest' });

      if (!this.delegationAttached) {
        this.delegationAttached = true;
        this.overlay.addEventListener('click', (e) => {
          const row = (e.target as HTMLElement).closest('.history-row[data-idx]');
          if (!row) return;
          const i = parseInt(row.getAttribute('data-idx') || '0');
          const entry = this.getAllEntries()[i];
          if (entry) {
            const sessionId = this.callbacks.getActiveSessionId();
            if (sessionId) {
              window.go.main.App.WriteToSession(sessionId, utf8ToBase64(entry.command));
            }
            this.hide();
          }
        });
      }
    } catch (e) {
      console.error('Render error:', e);
      this.overlay.innerHTML = '<div class="error">Something went wrong</div>';
    }
  }

  private renderRow(entry: HistoryEntry, idx: number, showCwd: boolean): string {
    const selected = idx === this.cursor ? 'selected' : '';
    const dotClass = entry.exitCode === 0 ? 'history-dot success' : 'history-dot fail';
    const time = this.relativeTime(entry.timestamp);
    const cwdHtml = showCwd ? `<span class="history-cwd">${escHtml(this.shortenPath(entry.cwd))}</span>` : '';

    return `<div class="history-row ${selected}" data-idx="${idx}">
      <span class="${dotClass}"></span>
      <div class="history-row-text">
        <span class="history-cmd">${escHtml(entry.command)}</span>
        ${cwdHtml}
      </div>
      <span class="history-time">${escHtml(time)}</span>
    </div>`;
  }

  private relativeTime(timestamp: number): string {
    const diff = Math.floor(Date.now() / 1000) - timestamp;
    if (diff < 60) return 'just now';
    if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
    if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
    return `${Math.floor(diff / 86400)}d ago`;
  }

  private shortenPath(path: string): string {
    const home = '/Users/' + path.split('/')[2];
    if (path.startsWith(home)) return '~' + path.slice(home.length);
    return path;
  }
}
