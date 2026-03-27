import { escHtml, utf8ToBase64 } from '../utils';

export interface AskAICallbacks {
  getActiveSessionId(): string;
  getActivePaneCWD(): Promise<string>;
  focusActivePane(): void;
  setAILoading(loading: boolean): void;
}

export class AskAI {
  private open = false;
  private loading = false;
  private error = '';
  private overlay: HTMLElement;
  private callbacks: AskAICallbacks;

  constructor(overlay: HTMLElement, callbacks: AskAICallbacks) {
    this.overlay = overlay;
    this.callbacks = callbacks;
  }

  show(): void {
    this.open = true;
    this.loading = false;
    this.error = '';
    this.render();
    this.overlay.classList.remove('hidden');
    requestAnimationFrame(() => {
      (this.overlay.querySelector('.ai-input') as HTMLInputElement)?.focus();
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

    if (e.key === 'Escape') {
      e.preventDefault();
      this.hide();
      return true;
    }

    if (e.key === 'Enter' && !this.loading) {
      e.preventDefault();
      const input = this.overlay.querySelector('.ai-input') as HTMLInputElement;
      if (input?.value.trim()) {
        this.submit(input.value.trim());
      }
      return true;
    }

    return true;
  }

  private async submit(prompt: string): Promise<void> {
    this.loading = true;
    this.error = '';
    this.render();
    this.callbacks.setAILoading(true);

    try {
      const cwd = await this.callbacks.getActivePaneCWD();
      const command = await window.go.main.App.AskAI(prompt, cwd);

      const sessionId = this.callbacks.getActiveSessionId();
      if (sessionId && command) {
        window.go.main.App.WriteToSession(sessionId, utf8ToBase64(command));
      }

      this.callbacks.setAILoading(false);
      this.hide();
    } catch (err: any) {
      this.callbacks.setAILoading(false);
      this.loading = false;
      this.error = err?.message || String(err) || 'Failed to generate command';
      this.render();
      requestAnimationFrame(() => {
        (this.overlay.querySelector('.ai-input') as HTMLInputElement)?.focus();
      });
    }
  }

  private render(): void {
    const errorHtml = this.error
      ? `<div class="ai-error">${escHtml(this.error)}</div>`
      : '';

    const placeholder = this.loading ? 'Generating...' : 'Describe what you want to do...';

    this.overlay.innerHTML = `
      <div class="ai-box">
        <div class="ai-header">
          <span class="ai-title">AI Command</span>
          <kbd class="ai-badge">Cmd+K</kbd>
        </div>
        <input
          class="ai-input"
          type="text"
          placeholder="${placeholder}"
          ${this.loading ? 'disabled' : ''}
        />
        ${this.loading ? '<div class="ai-loader"><div class="ai-loader-bar"></div></div>' : ''}
        ${errorHtml}
        <div class="ai-hint">
          <kbd>Enter</kbd> generate &middot; <kbd>Esc</kbd> close
        </div>
        <div class="ai-tip">Tip: type directly in terminal and press <kbd>Cmd+K</kbd> to convert inline</div>
      </div>
    `;

    const input = this.overlay.querySelector('.ai-input') as HTMLInputElement;
    if (input && !this.loading) {
      input.addEventListener('keydown', (e) => e.stopPropagation());
    }
  }
}
