import { Terminal } from '@xterm/xterm';
import { ShellIntegration, CommandBlock } from '../ShellIntegration';
import { detect, DetectionResult } from './OutputDetector';
import { renderJson } from './renderers/JsonRenderer';
import { renderTable } from './renderers/TableRenderer';
import { renderError } from './renderers/ErrorRenderer';
import { stripAnsi } from '../../utils';

const MAX_BADGES = 50;

interface SmartBadge {
  block: CommandBlock;
  detection: DetectionResult;
  element: HTMLElement;
}

export class SmartRenderManager {
  private terminal: Terminal;
  private paneElement: HTMLElement;
  private shellIntegration: ShellIntegration;
  private badges: SmartBadge[] = [];
  private panel: HTMLElement | null = null;
  private activeBadge: SmartBadge | null = null;
  private cellHeight = 0;
  private _raf = 0;
  private needsReposition = false;
  private unsubCommandFinished: (() => void) | null = null;
  private resizeObserver: ResizeObserver | null = null;

  public onBadgesChanged: (() => void) | null = null;

  constructor(terminal: Terminal, paneElement: HTMLElement, shellIntegration: ShellIntegration) {
    this.terminal = terminal;
    this.paneElement = paneElement;
    this.shellIntegration = shellIntegration;

    this.unsubCommandFinished = this.shellIntegration.onCommandFinishedAdd((block) => {
      this.handleCommandFinished(block);
    });

    this.terminal.onScroll(() => this.scheduleReposition());
    this.resizeObserver = new ResizeObserver(() => this.scheduleReposition());
    this.resizeObserver.observe(paneElement);
    this.terminal.onWriteParsed(() => this.cleanupInvalid());
  }

  private scheduleReposition(): void {
    if (!this.needsReposition) {
      this.needsReposition = true;
      this._raf = requestAnimationFrame(() => {
        this.needsReposition = false;
        this.updateCellHeight();
        this.repositionAll();
        if (this.panel) this.repositionPanel();
      });
    }
  }

  private updateCellHeight(): void {
    const screen = this.terminal.element?.querySelector('.xterm-screen') as HTMLElement;
    if (screen && this.terminal.rows > 0) {
      this.cellHeight = screen.clientHeight / this.terminal.rows;
    }
  }

  private handleCommandFinished(block: CommandBlock): void {
    const output = this.shellIntegration.getBlockOutput(block);
    if (!output) return;
    const clean = stripAnsi(output);
    const detection = detect(clean, block.exitCode);
    if (detection.type === 'none') return;
    this.addBadge(block, detection);
  }

  private addBadge(block: CommandBlock, detection: DetectionResult): void {
    while (this.badges.length >= MAX_BADGES) {
      const old = this.badges.shift();
      old?.element.remove();
    }
    this.updateCellHeight();

    const el = document.createElement('div');
    el.className = 'smart-badge';
    const label = detection.type === 'json' ? 'JSON'
      : detection.type === 'table' ? 'TABLE' : 'ERR';
    const dotClass = detection.type === 'error' ? 'smart-badge-dot-error' : 'smart-badge-dot';
    el.innerHTML = `<span class="${dotClass}"></span>${label}`;

    const badge: SmartBadge = { block, detection, element: el };
    this.badges.push(badge);

    el.addEventListener('click', () => this.togglePanel(badge));

    const endMarker = block.outputEndMarker;
    if (endMarker) {
      endMarker.onDispose(() => {
        const idx = this.badges.indexOf(badge);
        if (idx >= 0) this.badges.splice(idx, 1);
        el.remove();
        if (this.activeBadge === badge) this.closePanel();
      });
    }

    try {
      document.body.appendChild(el);
      this.positionBadge(badge);
      this.scheduleReposition();
    } catch (e) {
      el.remove();
      const idx = this.badges.indexOf(badge);
      if (idx >= 0) this.badges.splice(idx, 1);
    }
  }

  private positionBadge(badge: SmartBadge): void {
    if (this.cellHeight === 0) this.updateCellHeight();
    if (this.cellHeight === 0) {
      badge.element.style.display = 'none';
      return;
    }

    const paneRect = this.paneElement.getBoundingClientRect();
    const buf = this.terminal.buffer.active;
    const marker = badge.block.outputEndMarker || badge.block.promptMarker;
    // outputEndMarker sits on the same line as the next prompt — go one line up
    const line = Math.max(0, marker === badge.block.outputEndMarker ? marker.line - 1 : marker.line);
    const viewportTop = buf.viewportY;
    const viewportBottom = viewportTop + this.terminal.rows;

    if (line < viewportTop || line >= viewportBottom) {
      badge.element.style.display = 'none';
      return;
    }

    const rowOffset = (line - viewportTop) * this.cellHeight;
    const top = paneRect.top + rowOffset;

    // Clip to pane bounds
    if (top < paneRect.top || top > paneRect.bottom - 20) {
      badge.element.style.display = 'none';
      return;
    }

    badge.element.style.display = '';
    badge.element.style.top = `${top}px`;
    badge.element.style.left = `${paneRect.right - 70}px`;
  }

  private repositionAll(): void {
    this.updateCellHeight();
    for (const badge of this.badges) this.positionBadge(badge);
  }

  private cleanupInvalid(): void {
    for (let i = this.badges.length - 1; i >= 0; i--) {
      const b = this.badges[i];
      const endLine = b.block.outputEndMarker?.line ?? -1;
      if (endLine < 0) {
        b.element.remove();
        this.badges.splice(i, 1);
        if (this.activeBadge === b) this.closePanel();
      }
    }
  }

  private togglePanel(badge: SmartBadge): void {
    if (this.activeBadge === badge && this.panel) {
      this.closePanel();
      return;
    }
    this.closePanel();
    this.activeBadge = badge;
    badge.element.classList.add('smart-badge-active');

    const panel = document.createElement('div');
    panel.className = 'smart-panel';

    try {
      const header = document.createElement('div');
      header.className = 'smart-panel-header';
      const title = document.createElement('span');
      title.className = 'smart-panel-title';
      title.textContent = badge.detection.type === 'json' ? 'JSON Output'
        : badge.detection.type === 'table' ? 'Table Output' : 'Error Output';
      const actions = document.createElement('div');
      actions.className = 'smart-panel-actions';
      const copyBtn = document.createElement('span');
      copyBtn.className = 'smart-panel-action';
      copyBtn.textContent = 'Copy';
      copyBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        const raw = (badge.detection as { raw: string }).raw || '';
        if (raw) navigator.clipboard.writeText(raw);
        copyBtn.textContent = 'Copied';
        setTimeout(() => { copyBtn.textContent = 'Copy'; }, 1000);
      });
      const closeBtn = document.createElement('span');
      closeBtn.className = 'smart-panel-close';
      closeBtn.textContent = '\u00D7';
      closeBtn.addEventListener('click', (e) => { e.stopPropagation(); this.closePanel(); });
      actions.appendChild(copyBtn);
      actions.appendChild(closeBtn);
      header.appendChild(title);
      header.appendChild(actions);
      panel.appendChild(header);

      const content = document.createElement('div');
      content.className = 'smart-panel-content';
      switch (badge.detection.type) {
        case 'json': renderJson(badge.detection.parsed, content); break;
        case 'table': renderTable(badge.detection.headers, badge.detection.rows, content); break;
        case 'error': renderError(badge.detection.raw, badge.detection.errorLines, badge.detection.exitCode, content); break;
      }
      panel.appendChild(content);

      document.body.appendChild(panel);
      this.panel = panel;
      this.repositionPanel();
      this.scheduleReposition();
    } catch (e) {
      panel.remove();
      this.panel = null;
      this.activeBadge = null;
      badge.element.classList.remove('smart-badge-active');
    }
  }

  private repositionPanel(): void {
    if (!this.panel) return;
    const paneRect = this.paneElement.getBoundingClientRect();
    const statusBar = document.getElementById('statusbar');
    const bottom = statusBar ? statusBar.getBoundingClientRect().top : paneRect.bottom;
    this.panel.style.bottom = `${window.innerHeight - bottom}px`;
    this.panel.style.left = `${paneRect.left}px`;
    this.panel.style.width = `${paneRect.width}px`;
    this.panel.style.maxHeight = `${paneRect.height * 0.45}px`;
    this.panel.style.top = '';
  }

  closePanel(): void {
    if (this.panel) { this.panel.remove(); this.panel = null; }
    if (this.activeBadge) {
      this.activeBadge.element.classList.remove('smart-badge-active');
      this.activeBadge = null;
    }
  }

  isPanelOpen(): boolean { return this.panel !== null; }

  handleKeydown(e: KeyboardEvent): boolean {
    if (this.panel && e.key === 'Escape') {
      e.preventDefault();
      this.closePanel();
      return true;
    }
    return false;
  }

  renderBadgesHTML(): string { return ''; }
  attachBadgeListeners(_c: HTMLElement): void {}

  dispose(): void {
    cancelAnimationFrame(this._raf);
    this.unsubCommandFinished?.();
    this.unsubCommandFinished = null;
    this.resizeObserver?.disconnect();
    this.resizeObserver = null;
    this.closePanel();
    for (const b of this.badges) b.element.remove();
    this.badges = [];
  }
}
