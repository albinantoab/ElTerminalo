import { CustomCommand } from '../types';
import { escHtml } from '../utils';
import { BUILT_IN_SHORTCUTS, SYSTEM_SHORTCUTS } from '../constants';

export interface WizardCallbacks {
  getActivePaneCWD(): Promise<string>;
  focusActivePane(): void;
  refreshCustomCommands(): Promise<void>;
  getCustomCommands(): CustomCommand[];
}

export class CommandWizard {
  private open = false;
  private step = 0;
  private scopeCursor = 0;
  private data = { scope: '', command: '', name: '', description: '', shortcut: '' };
  private shortcutConflict = '';
  private capturingShortcut = false;
  private editingOriginalName: string | null = null;
  private editingScope: string | null = null;
  private overlay: HTMLElement;
  private callbacks: WizardCallbacks;

  constructor(overlay: HTMLElement, callbacks: WizardCallbacks) {
    this.overlay = overlay;
    this.callbacks = callbacks;
  }

  show(prefillCommand?: string): void {
    this.open = true;
    this.step = 0;
    this.data = { scope: '', command: prefillCommand || '', name: '', description: '', shortcut: '' };
    this.shortcutConflict = '';
    this.capturingShortcut = false;
    this.editingOriginalName = null;
    this.editingScope = null;
    this.render();
    this.overlay.classList.remove('hidden');
  }

  showForEdit(cmd: { name: string; command?: string; desc?: string; scope?: string; shortcutKey?: string }): void {
    this.open = true;
    this.step = 1;
    this.data = {
      scope: cmd.scope || 'global',
      command: cmd.command || '',
      name: cmd.name,
      description: cmd.desc !== cmd.command ? (cmd.desc || '') : '',
      shortcut: cmd.shortcutKey || '',
    };
    this.shortcutConflict = '';
    this.capturingShortcut = false;
    this.editingOriginalName = cmd.name;
    this.editingScope = cmd.scope || null;
    this.render();
    this.overlay.classList.remove('hidden');
  }

  hide(): void {
    this.open = false;
    this.overlay.classList.add('hidden');
    this.editingOriginalName = null;
    this.editingScope = null;
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

    if (this.step === 0) {
      if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') {
        e.preventDefault();
        this.scopeCursor = 0;
        this.render();
        return true;
      }
      if (e.key === 'ArrowRight' || e.key === 'ArrowDown') {
        e.preventDefault();
        this.scopeCursor = 1;
        this.render();
        return true;
      }
      if (e.key === 'Enter') {
        e.preventDefault();
        this.data.scope = this.scopeCursor === 0 ? 'global' : 'local';
        this.step = 1;
        this.render();
        return true;
      }
      return true;
    }

    if (this.step === 4) {
      if (e.key === 'Enter') {
        e.preventDefault();
        if (!this.shortcutConflict) this.finish();
        return true;
      }
      if (e.key === 'Backspace') {
        e.preventDefault();
        this.data.shortcut = '';
        this.shortcutConflict = '';
        this.render();
        return true;
      }
      if (e.key !== 'Meta' && e.key !== 'Control' && e.key !== 'Alt' && e.key !== 'Shift') {
        e.preventDefault();
        const parts: string[] = [];
        if (e.metaKey) parts.push('Cmd');
        if (e.ctrlKey) parts.push('Ctrl');
        if (e.shiftKey) parts.push('Shift');
        if (e.altKey) parts.push('Alt');
        parts.push(e.key.length === 1 ? e.key.toUpperCase() : e.key);
        const shortcut = parts.join('+');
        this.data.shortcut = shortcut;
        this.shortcutConflict = this.checkShortcutConflict(shortcut);
        this.render();
        return true;
      }
      return true;
    }

    // Step 1 (command textarea): Enter advances, Shift+Enter inserts newline
    if (this.step === 1 && e.key === 'Enter') {
      if (e.shiftKey) {
        // Don't preventDefault — let Shift+Enter insert newline in textarea
        return true;
      }
      e.preventDefault();
      const val = (document.getElementById('wizard-field') as HTMLTextAreaElement)?.value.trim() || '';
      if (!val) return true;
      this.data.command = val;
      this.step = 2;
      const firstLine = val.split('\n')[0];
      this.data.name = firstLine.split(' ').slice(0, 3).map(w => w.charAt(0).toUpperCase() + w.slice(1)).join(' ');
      this.render();
      return true;
    }

    // Steps 2-3: Enter advances
    if (e.key === 'Enter' && this.step >= 2 && this.step <= 3) {
      e.preventDefault();
      const val = (document.getElementById('wizard-field') as HTMLInputElement)?.value.trim() || '';
      if (this.step === 2) {
        const namePattern = /^[a-zA-Z0-9_\- ]+$/;
        if (!val || val.length > 64 || !namePattern.test(val)) {
          const field = document.getElementById('wizard-field') as HTMLInputElement;
          if (field) {
            field.classList.add('input-error');
            setTimeout(() => field.classList.remove('input-error'), 1500);
          }
          return true;
        }
        this.data.name = val;
        this.step = 3;
        this.render();
      } else if (this.step === 3) {
        this.data.description = val;
        this.step = 4;
        this.shortcutConflict = '';
        this.render();
      }
      return true;
    }
    return true;
  }

  private render(): void {
    let content = '';
    if (this.step === 0) {
      const scopes = [
        { key: 'global', label: 'Global', hint: '~/.config/elterminalo' },
        { key: 'local', label: 'Project', hint: '.elterminalo/ in cwd' },
      ];
      const btns = scopes.map((s, i) =>
        `<div class="wizard-scope-btn ${i === this.scopeCursor ? 'wizard-scope-active' : ''}" data-scope="${s.key}">${s.label}<br><span style="font-size:10px;color:var(--border)">${s.hint}</span></div>`
      ).join('');
      content = `<div class="wizard-title">Create Command</div><div class="wizard-step-label">Where should this command be saved?</div><div class="wizard-scope-btns">${btns}</div><div class="wizard-hint">← → select · enter confirm · esc cancel</div>`;
    } else if (this.step === 1) {
      content = `<div class="wizard-title">Create Command — ${this.data.scope}</div><div class="wizard-step-label">Command to execute</div><textarea class="wizard-input wizard-textarea" id="wizard-field" placeholder="e.g. npm run build" rows="3">${escHtml(this.data.command)}</textarea><div class="wizard-hint">shift+enter for new line · enter to continue · esc to cancel</div>`;
    } else if (this.step === 2) {
      content = `<div class="wizard-title">Create Command — ${this.data.scope}</div><div class="wizard-step-label">Display name</div><input class="wizard-input" id="wizard-field" type="text" placeholder="e.g. Build Project" value="${escHtml(this.data.name)}" /><div class="wizard-hint">enter to continue · esc to cancel</div>`;
    } else if (this.step === 3) {
      content = `<div class="wizard-title">Create Command — ${this.data.scope}</div><div class="wizard-step-label">Description (optional)</div><input class="wizard-input" id="wizard-field" type="text" placeholder="e.g. Builds the project for production" value="${escHtml(this.data.description)}" /><div class="wizard-hint">enter to continue · esc to cancel</div>`;
    } else if (this.step === 4) {
      const display = this.data.shortcut || 'Press a key combo...';
      const conflict = this.shortcutConflict
        ? `<div class="wizard-conflict">Conflicts with: ${escHtml(this.shortcutConflict)}</div>`
        : '';
      content = `<div class="wizard-title">Create Command — ${this.data.scope}</div><div class="wizard-step-label">Keyboard shortcut (optional)</div><div class="wizard-shortcut-capture" id="wizard-shortcut-box">${escHtml(display)}</div>${conflict}<div class="wizard-hint">press a key combo · backspace to clear · enter to save · esc to cancel</div>`;
    }

    this.overlay.innerHTML = `<div class="wizard-box">${content}</div>`;

    if (this.step === 0) {
      this.overlay.querySelectorAll('.wizard-scope-btn').forEach(btn => {
        btn.addEventListener('click', () => {
          this.data.scope = btn.getAttribute('data-scope') || 'global';
          this.step = 1;
          this.render();
        });
      });
    }

    requestAnimationFrame(() => {
      const input = document.getElementById('wizard-field') as HTMLInputElement;
      if (input) {
        input.focus();
        input.selectionStart = input.selectionEnd = input.value.length;
      }
    });
  }

  private async finish(): Promise<void> {
    const { scope, name, command, description, shortcut } = this.data;
    const cwd = await this.callbacks.getActivePaneCWD();
    try {
      if (this.editingOriginalName) {
        await window.go.main.App.UpdateCommand(
          this.editingScope || scope,
          this.editingOriginalName,
          name, command, description, shortcut, cwd
        );
        this.editingOriginalName = null;
        this.editingScope = null;
      } else {
        await window.go.main.App.SaveCommand(scope, name, command, description, shortcut, cwd);
      }
    } catch (e) {
      console.error('Failed to save command:', e);
    }
    this.hide();
    await this.callbacks.refreshCustomCommands();
  }

  private checkShortcutConflict(shortcut: string): string {
    const s = shortcut.toLowerCase();
    if (SYSTEM_SHORTCUTS[s]) return SYSTEM_SHORTCUTS[s];
    if (BUILT_IN_SHORTCUTS[s]) return BUILT_IN_SHORTCUTS[s];
    for (const c of this.callbacks.getCustomCommands()) {
      if (c.shortcut && c.shortcut.toLowerCase() === s) return c.name;
    }
    return '';
  }
}
