import { escHtml } from '../utils';

export interface ThemeWizardCallbacks {
  onSave(): Promise<void>;  // called after saving — host should reload themes
  focusActivePane(): void;
}

export class ThemeWizard {
  private open = false;
  private overlay: HTMLElement;
  private callbacks: ThemeWizardCallbacks;
  private editingName: string | null = null;

  constructor(overlay: HTMLElement, callbacks: ThemeWizardCallbacks) {
    this.overlay = overlay;
    this.callbacks = callbacks;
  }

  show(): void {
    this.editingName = null;
    this.open = true;
    this.render();
    this.overlay.classList.remove('hidden');
  }

  showForEdit(themeName: string, themeData: Record<string, string>): void {
    this.editingName = themeName;
    this.open = true;
    this.render(themeData);
    this.overlay.classList.remove('hidden');
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
    if (e.key === 'Escape') {
      e.preventDefault();
      this.hide();
      return true;
    }
    // Let other keys pass through to the form inputs
    return true;
  }

  private render(prefill?: Record<string, string>): void {
    try {
    const title = this.editingName ? `Edit Theme — ${escHtml(this.editingName)}` : 'Create Theme';
    const v = (field: string) => prefill ? (prefill[field] || '') : '';

    const colorField = (label: string, field: string, defaultVal: string = '') => {
      const val = v(field) || defaultVal;
      return `<div class="theme-field">
        <label class="theme-field-label">${label}</label>
        <div class="theme-field-input-wrap">
          <div class="theme-color-swatch" style="background:${escHtml(val)}"></div>
          <input class="theme-field-input" type="text" data-field="${field}" value="${escHtml(val)}" placeholder="#000000" spellcheck="false" />
        </div>
      </div>`;
    };

    this.overlay.innerHTML = `<div class="theme-wizard-box">
      <div class="theme-wizard-header">
        <div class="wizard-title">${title}</div>
      </div>
      <div class="theme-wizard-body">
        <div class="theme-field theme-field-name">
          <label class="theme-field-label">Theme Name</label>
          <input class="theme-field-input theme-name-input" type="text" data-field="name" value="${escHtml(v('name'))}" placeholder="My Theme" spellcheck="false" />
        </div>

        <div class="theme-group-label">UI Colors</div>
        <div class="theme-fields-grid">
          ${colorField('Background', 'background', '#0a0a12')}
          ${colorField('Foreground', 'foreground', '#e0e0e8')}
          ${colorField('Accent', 'accent', '#5e17eb')}
          ${colorField('Accent Dim', 'accentDim', '#4311b0')}
          ${colorField('Border', 'border', '#1a1a2e')}
          ${colorField('Border Active', 'borderActive', '#5e17eb')}
          ${colorField('Status Bg', 'statusBg', '#06060c')}
          ${colorField('Status Fg', 'statusFg', '#5e17eb')}
          ${colorField('Cursor', 'cursorColor', '#5e17eb')}
          ${colorField('Selection', 'selectionBg', '#2a1a4e')}
        </div>

        <div class="theme-group-label">ANSI Colors</div>
        <div class="theme-fields-grid">
          ${colorField('Black', 'black', '#0a0a12')}
          ${colorField('Red', 'red', '#ff5572')}
          ${colorField('Green', 'green', '#7dd6a0')}
          ${colorField('Yellow', 'yellow', '#f0c674')}
          ${colorField('Blue', 'blue', '#7aa2f7')}
          ${colorField('Magenta', 'magenta', '#bb9af7')}
          ${colorField('Cyan', 'cyan', '#7dcfff')}
          ${colorField('White', 'white', '#e0e0e8')}
        </div>

        <div class="theme-group-label">Bright ANSI Colors</div>
        <div class="theme-fields-grid">
          ${colorField('Bright Black', 'brightBlack', '#4311b0')}
          ${colorField('Bright Red', 'brightRed', '#ff7a93')}
          ${colorField('Bright Green', 'brightGreen', '#a8e6b0')}
          ${colorField('Bright Yellow', 'brightYellow', '#f5d8a0')}
          ${colorField('Bright Blue', 'brightBlue', '#9ab8f7')}
          ${colorField('Bright Magenta', 'brightMagenta', '#d0b8ff')}
          ${colorField('Bright Cyan', 'brightCyan', '#a0dcff')}
          ${colorField('Bright White', 'brightWhite', '#ffffff')}
        </div>
      </div>
      <div class="theme-wizard-footer">
        <button class="theme-btn theme-btn-cancel">Cancel</button>
        <button class="theme-btn theme-btn-save">Save Theme</button>
      </div>
    </div>`;

    // Wire events
    this.overlay.querySelector('.theme-btn-cancel')?.addEventListener('click', () => this.hide());
    this.overlay.querySelector('.theme-btn-save')?.addEventListener('click', () => this.save());

    // Live swatch preview on input
    this.overlay.querySelectorAll('.theme-field-input[data-field]').forEach(input => {
      input.addEventListener('input', (e) => {
        const el = e.target as HTMLInputElement;
        const swatch = el.parentElement?.querySelector('.theme-color-swatch') as HTMLElement;
        if (swatch && el.value.match(/^#[0-9a-fA-F]{3,8}$/)) {
          swatch.style.background = el.value;
        }
      });
    });

    // Focus name field
    requestAnimationFrame(() => {
      const nameInput = this.overlay.querySelector('.theme-name-input') as HTMLInputElement;
      if (nameInput) {
        nameInput.focus();
        nameInput.select();
      }
    });
    } catch (e) {
      console.error('ThemeWizard render error:', e);
      this.overlay.innerHTML = '<div class="theme-wizard-box"><div class="wizard-title">Something went wrong</div></div>';
    }
  }

  private async save(): Promise<void> {
    const getVal = (field: string): string => {
      const input = this.overlay.querySelector(`[data-field="${field}"]`) as HTMLInputElement;
      return input?.value.trim() || '';
    };

    const name = getVal('name');
    if (!name) {
      const nameInput = this.overlay.querySelector('.theme-name-input') as HTMLInputElement;
      if (nameInput) {
        nameInput.classList.add('input-error');
        setTimeout(() => nameInput.classList.remove('input-error'), 1500);
      }
      return;
    }

    try {
      await window.go.main.App.SaveTheme(
        name,
        getVal('background'), getVal('foreground'),
        getVal('accent'), getVal('accentDim'),
        getVal('border'), getVal('borderActive'),
        getVal('statusBg'), getVal('statusFg'),
        getVal('cursorColor'), getVal('selectionBg'),
        getVal('black'), getVal('red'), getVal('green'), getVal('yellow'),
        getVal('blue'), getVal('magenta'), getVal('cyan'), getVal('white'),
        getVal('brightBlack'), getVal('brightRed'), getVal('brightGreen'),
        getVal('brightYellow'), getVal('brightBlue'), getVal('brightMagenta'),
        getVal('brightCyan'), getVal('brightWhite')
      );
      this.hide();
      await this.callbacks.onSave();
    } catch (e) {
      console.error('Failed to save theme:', e);
    }
  }
}
