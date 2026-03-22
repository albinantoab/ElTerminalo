import { PaletteCommand, CustomCommand } from '../types';

export interface PaletteCallbacks {
  getBuiltInCommands(): PaletteCommand[];
  getCustomCommands(): CustomCommand[];
  getActiveSessionId(): string;
  getActivePaneCWD(): Promise<string>;
  focusActivePane(): void;
  refreshCustomCommands(): Promise<void>;
  onEditCommand(cmd: PaletteCommand): void;
  onEditTheme(cmd: PaletteCommand): void;
  onDeleteTheme(cmd: PaletteCommand): Promise<void>;
}

export class CommandPalette {
  private open = false;
  private query = '';
  private cursor = 0;
  private overlay: HTMLElement;
  private callbacks: PaletteCallbacks;

  constructor(overlay: HTMLElement, callbacks: PaletteCallbacks) {
    this.overlay = overlay;
    this.callbacks = callbacks;
  }

  async show(): Promise<void> {
    this.open = true;
    this.query = '';
    this.cursor = 0;
    await this.callbacks.refreshCustomCommands();
    this.render();
    this.overlay.classList.remove('hidden');
    requestAnimationFrame(() => {
      (this.overlay.querySelector('.palette-input') as HTMLInputElement)?.focus();
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
    const isMeta = e.metaKey || e.ctrlKey;

    if (e.key === 'Escape') {
      e.preventDefault();
      this.hide();
      return true;
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault();
      this.cursor = Math.max(0, this.cursor - 1);
      this.render();
      (this.overlay.querySelector('.palette-input') as HTMLInputElement)?.focus();
      return true;
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      this.cursor = Math.min(this.getFilteredCommands().length - 1, this.cursor + 1);
      this.render();
      (this.overlay.querySelector('.palette-input') as HTMLInputElement)?.focus();
      return true;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      const cmd = this.getFilteredCommands()[this.cursor];
      this.hide();
      cmd?.action(isMeta);
      return true;
    }
    if (isMeta && e.key.toLowerCase() === 'e') {
      e.preventDefault();
      const cmd = this.getFilteredCommands()[this.cursor];
      if (cmd?.isCustom) this.callbacks.onEditCommand(cmd);
      else if (cmd?.isTheme) this.callbacks.onEditTheme(cmd);
      return true;
    }
    if (isMeta && e.key.toLowerCase() === 'd') {
      e.preventDefault();
      const cmd = this.getFilteredCommands()[this.cursor];
      if (cmd?.isCustom) this.deleteCommand(cmd);
      else if (cmd?.isTheme) this.deleteTheme(cmd);
      return true;
    }
    return true;
  }

  private getCommands(): PaletteCommand[] {
    const builtIn = this.callbacks.getBuiltInCommands();
    const customCommands = this.callbacks.getCustomCommands();

    const custom: PaletteCommand[] = customCommands.map(c => ({
      name: c.name,
      desc: c.description || c.command,
      category: c.scope === 'local' ? 'Project' : 'Global',
      shortcutDisplay: c.shortcut || '',
      isCustom: true,
      scope: c.scope,
      command: c.command,
      shortcutKey: c.shortcut || '',
      action: (metaKey?: boolean) => {
        const sessionId = this.callbacks.getActiveSessionId();
        if (!sessionId) return;
        window.go.main.App.WriteToSession(sessionId, btoa(c.command + (metaKey ? '' : '\n')));
      },
    }));

    return [...custom, ...builtIn];
  }

  private getFilteredCommands(): PaletteCommand[] {
    const q = this.query.toLowerCase();
    if (!q) return this.getCommands();
    return this.getCommands().filter(c =>
      c.name.toLowerCase().includes(q) ||
      c.desc.toLowerCase().includes(q) ||
      c.category.toLowerCase().includes(q)
    );
  }

  private render(): void {
    const commands = this.getFilteredCommands();
    let lastCategory = '';
    const items = commands.map((c, i) => {
      const shortcutBadge = c.shortcutDisplay ? `<kbd class="palette-item-shortcut">${c.shortcutDisplay}</kbd>` : '';
      let groupHeader = '';
      if (c.category !== lastCategory) {
        lastCategory = c.category;
        groupHeader = `<div class="palette-group-header">${c.category}</div>`;
      }
      return `${groupHeader}<div class="palette-item ${i === this.cursor ? 'selected' : ''}" data-index="${i}"><div><span class="palette-item-name">${c.name}</span><span class="palette-item-desc">${c.desc}</span></div>${shortcutBadge}</div>`;
    }).join('');

    this.overlay.innerHTML = `<div class="palette-box"><input class="palette-input" type="text" placeholder="Type a command..." value="${this.query}" /><div class="palette-list">${items || '<div class="palette-item"><span class="palette-item-desc">No matching commands</span></div>'}</div><div class="palette-hint"><kbd>Enter</kbd> execute · <kbd>Cmd + Enter</kbd> fill · <kbd>Cmd + E</kbd> edit · <kbd>Cmd + D</kbd> delete · <kbd>Esc</kbd> close</div></div>`;

    const input = this.overlay.querySelector('.palette-input') as HTMLInputElement;
    input.addEventListener('input', (e) => {
      this.query = (e.target as HTMLInputElement).value;
      this.cursor = 0;
      this.render();
      const ni = this.overlay.querySelector('.palette-input') as HTMLInputElement;
      ni?.focus();
      ni.selectionStart = ni.selectionEnd = ni.value.length;
    });
    this.overlay.querySelector('.palette-item.selected')?.scrollIntoView({ block: 'nearest' });
    this.overlay.querySelectorAll('.palette-item[data-index]').forEach(el => {
      el.addEventListener('click', () => {
        const cmd = commands[parseInt(el.getAttribute('data-index') || '0')];
        this.hide();
        cmd?.action(false);
      });
    });
  }

  private async deleteTheme(cmd: PaletteCommand): Promise<void> {
    try {
      await this.callbacks.onDeleteTheme(cmd);
      this.render();
      (this.overlay.querySelector('.palette-input') as HTMLInputElement)?.focus();
    } catch (e) {
      console.error('Failed to delete theme:', e);
    }
  }

  async deleteCommand(cmd: { name: string; scope?: string }): Promise<void> {
    const cwd = await this.callbacks.getActivePaneCWD();
    try {
      await window.go.main.App.DeleteCommand(cmd.scope || 'global', cmd.name, cwd);
      await this.callbacks.refreshCustomCommands();
      this.render();
      (this.overlay.querySelector('.palette-input') as HTMLInputElement)?.focus();
    } catch (e) {
      console.error('Failed to delete command:', e);
    }
  }
}
