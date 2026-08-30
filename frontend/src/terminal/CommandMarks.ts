import { Terminal } from '@xterm/xterm';
import { ShellIntegration, CommandBlock } from './ShellIntegration';
import { MAX_COMMAND_MARKS } from '../constants';

/** What a mark's action row can ask of the pane it belongs to. */
export interface CommandMarkActions {
  /** Type text into the shell *without* a newline — the palette's Cmd+Enter
   *  convention: fill the command line, let the user decide to run it. */
  fillInput(text: string): void;
}

interface CommandMark {
  block: CommandBlock;
  element: HTMLElement;
}

/** A small coloured mark in the left margin of every prompt line that ran a
 *  command: neutral while it runs, green when it exits 0, red otherwise.
 *
 *  Not built on xterm decorations — those are drawn in the cell grid and would
 *  sit *on top of* the first column of text. This is the approach
 *  SmartRenderManager already uses for its badges: absolutely positioned
 *  elements in `document.body`, placed from the marker's buffer line and the
 *  viewport's scroll position, repositioned on the same rAF-batched path, and
 *  disposed with the marker they hang off when the scrollback is trimmed. */
export class CommandMarks {
  private terminal: Terminal;
  private paneElement: HTMLElement;
  private shellIntegration: ShellIntegration;
  private actions: CommandMarkActions;
  private marks: CommandMark[] = [];
  private byBlock = new Map<CommandBlock, CommandMark>();
  private actionRow: HTMLElement | null = null;
  private activeMark: CommandMark | null = null;
  private cellHeight = 0;
  private _raf = 0;
  private needsReposition = false;
  private unsubBlockChanged: (() => void) | null = null;
  private resizeObserver: ResizeObserver | null = null;
  // Whether the last write left the terminal on the alternate screen. Only
  // worth a reposition pass when it *changes*: while a TUI is up there is
  // nothing to draw, and every mark is already hidden.
  private wasAlternate = false;
  private onDocumentMouseDown = (e: MouseEvent): void => {
    if (!this.actionRow) return;
    const target = e.target as HTMLElement | null;
    if (target?.closest('.cmd-mark-actions') || target?.closest('.cmd-mark')) return;
    this.closeActions();
  };

  constructor(
    terminal: Terminal,
    paneElement: HTMLElement,
    shellIntegration: ShellIntegration,
    actions: CommandMarkActions,
  ) {
    this.terminal = terminal;
    this.paneElement = paneElement;
    this.shellIntegration = shellIntegration;
    this.actions = actions;

    this.unsubBlockChanged = this.shellIntegration.onBlockChangedAdd((block) => {
      this.upsertMark(block);
    });

    this.terminal.onScroll(() => this.scheduleReposition());
    this.resizeObserver = new ResizeObserver(() => this.scheduleReposition());
    this.resizeObserver.observe(paneElement);
    this.terminal.onWriteParsed(() => {
      this.cleanupInvalid();
      const alternate = this.isAlternate();
      if (alternate !== this.wasAlternate) {
        this.wasAlternate = alternate;
        this.scheduleReposition();
      }
    });
    document.addEventListener('mousedown', this.onDocumentMouseDown, true);
  }

  private isAlternate(): boolean {
    return this.terminal.buffer.active.type !== 'normal';
  }

  private scheduleReposition(): void {
    if (this.needsReposition) return;
    this.needsReposition = true;
    this._raf = requestAnimationFrame(() => {
      this.needsReposition = false;
      this.updateCellHeight();
      for (const mark of this.marks) this.positionMark(mark);
      this.positionActions();
    });
  }

  private updateCellHeight(): void {
    const screen = this.terminal.element?.querySelector('.xterm-screen') as HTMLElement | null;
    if (screen && this.terminal.rows > 0) {
      this.cellHeight = screen.clientHeight / this.terminal.rows;
    }
  }

  /** Create this block's mark, or re-colour the one it already has. */
  private upsertMark(block: CommandBlock): void {
    const existing = this.byBlock.get(block);
    if (existing) {
      this.styleMark(existing);
      this.positionMark(existing);
      return;
    }
    // Marks are only made for blocks that actually ran something: a prompt the
    // user looked at and walked away from has no output to select and no
    // command to re-run.
    if (!block.outputStartMarker) return;

    while (this.marks.length >= MAX_COMMAND_MARKS) {
      const old = this.marks.shift();
      if (!old) break;
      this.byBlock.delete(old.block);
      old.element.remove();
      if (this.activeMark === old) this.closeActions();
    }

    const el = document.createElement('div');
    el.className = 'cmd-mark';
    const mark: CommandMark = { block, element: el };
    el.addEventListener('click', (e) => {
      e.stopPropagation();
      this.toggleActions(mark);
    });

    block.promptMarker.onDispose(() => this.removeMark(mark));

    try {
      document.body.appendChild(el);
    } catch {
      return;
    }
    this.marks.push(mark);
    this.byBlock.set(block, mark);
    this.styleMark(mark);
    this.updateCellHeight();
    this.positionMark(mark);
  }

  private removeMark(mark: CommandMark): void {
    const idx = this.marks.indexOf(mark);
    if (idx >= 0) this.marks.splice(idx, 1);
    this.byBlock.delete(mark.block);
    mark.element.remove();
    if (this.activeMark === mark) this.closeActions();
  }

  private styleMark(mark: CommandMark): void {
    const code = mark.block.exitCode;
    mark.element.classList.toggle('is-running', code === null);
    mark.element.classList.toggle('is-ok', code === 0);
    mark.element.classList.toggle('is-fail', code !== null && code !== 0);
    const command = (mark.block.commandText || '').trim().replace(/\s+/g, ' ').slice(0, 200);
    const detail = code === null
      ? 'running…'
      : `exit ${code} · ${formatDuration(mark.block.startedAt, mark.block.endedAt)}`;
    mark.element.title = command ? `${command}\n${detail}` : detail;
  }

  private positionMark(mark: CommandMark): void {
    const el = mark.element;
    // A pane in a background tab is out of the document altogether, and a TUI
    // has replaced the buffer these lines are in.
    if (!this.paneElement.isConnected || this.isAlternate()) {
      el.style.display = 'none';
      return;
    }
    if (this.cellHeight === 0) this.updateCellHeight();
    if (this.cellHeight === 0) {
      el.style.display = 'none';
      return;
    }

    const paneRect = this.paneElement.getBoundingClientRect();
    const line = mark.block.promptMarker.line;
    const viewportTop = this.terminal.buffer.active.viewportY;
    if (line < viewportTop || line >= viewportTop + this.terminal.rows) {
      el.style.display = 'none';
      return;
    }

    const top = paneRect.top + (line - viewportTop) * this.cellHeight;
    if (top < paneRect.top || top > paneRect.bottom - this.cellHeight) {
      el.style.display = 'none';
      return;
    }

    el.style.display = '';
    el.style.top = `${top}px`;
    el.style.left = `${paneRect.left}px`;
    el.style.height = `${Math.max(4, this.cellHeight)}px`;
  }

  /** Drop marks whose marker has gone — the scrollback was trimmed past them,
   *  or the buffer was replaced. Cheap enough to run per parsed write: the list
   *  is capped at MAX_COMMAND_MARKS. */
  private cleanupInvalid(): void {
    for (let i = this.marks.length - 1; i >= 0; i--) {
      if (this.marks[i].block.promptMarker.line < 0) this.removeMark(this.marks[i]);
    }
  }

  private toggleActions(mark: CommandMark): void {
    if (this.activeMark === mark && this.actionRow) {
      this.closeActions();
      return;
    }
    this.closeActions();
    this.activeMark = mark;
    mark.element.classList.add('cmd-mark-active');

    // Show the user which block this is about, in the terminal itself.
    this.selectBlock(mark.block);

    const row = document.createElement('div');
    row.className = 'smart-panel cmd-mark-actions';

    const label = document.createElement('span');
    label.className = 'cmd-mark-label';
    const command = (mark.block.commandText || '').trim().replace(/\s+/g, ' ');
    label.textContent = command ? command.slice(0, 60) : 'Command';
    label.title = command;
    row.appendChild(label);

    const copy = document.createElement('span');
    copy.className = 'smart-panel-action';
    copy.textContent = 'Copy output';
    copy.addEventListener('click', (e) => {
      e.stopPropagation();
      const output = this.shellIntegration.getBlockOutput(mark.block);
      if (!output) {
        copy.textContent = 'No output';
        setTimeout(() => { copy.textContent = 'Copy output'; }, 1000);
        return;
      }
      navigator.clipboard.writeText(output).catch(() => { /* denied or unfocused */ });
      copy.textContent = 'Copied';
      setTimeout(() => { copy.textContent = 'Copy output'; }, 1000);
    });
    row.appendChild(copy);

    if (command) {
      const rerun = document.createElement('span');
      rerun.className = 'smart-panel-action';
      rerun.textContent = 'Re-run command';
      rerun.addEventListener('click', (e) => {
        e.stopPropagation();
        // Filled in, never executed: the same rule the palette and the AI
        // prompt follow, and the only safe one for a command the user is
        // reaching back into the scrollback for.
        this.actions.fillInput(mark.block.commandText || '');
        this.closeActions();
      });
      row.appendChild(rerun);
    }

    const close = document.createElement('span');
    close.className = 'smart-panel-close';
    close.textContent = '×';
    close.addEventListener('click', (e) => { e.stopPropagation(); this.closeActions(); });
    row.appendChild(close);

    try {
      document.body.appendChild(row);
    } catch {
      this.activeMark = null;
      mark.element.classList.remove('cmd-mark-active');
      return;
    }
    this.actionRow = row;
    this.positionActions();
  }

  /** Select the block's output in the terminal, so clicking a mark shows what
   *  the command actually printed. Falls back to the prompt line itself for a
   *  command that is still running or printed nothing. */
  private selectBlock(block: CommandBlock): void {
    const start = block.outputStartMarker?.line ?? block.promptMarker.line;
    // The end marker sits on the line the *next* prompt starts on.
    const end = block.outputEndMarker ? block.outputEndMarker.line - 1 : start;
    if (start < 0) return;
    this.terminal.clearSelection();
    this.terminal.selectLines(start, Math.max(start, end));
  }

  private positionActions(): void {
    const row = this.actionRow;
    const mark = this.activeMark;
    if (!row || !mark) return;
    if (mark.element.style.display === 'none' || !this.paneElement.isConnected) {
      row.style.display = 'none';
      return;
    }
    const paneRect = this.paneElement.getBoundingClientRect();
    const markTop = parseFloat(mark.element.style.top || '0');
    row.style.display = '';
    row.style.left = `${paneRect.left + 8}px`;
    row.style.width = `${Math.max(160, paneRect.width - 16)}px`;
    // Below the mark where there is room, above it at the bottom of the pane.
    const height = row.offsetHeight || 28;
    const below = markTop + this.cellHeight + 2;
    row.style.top = below + height > paneRect.bottom
      ? `${Math.max(paneRect.top, markTop - height - 2)}px`
      : `${below}px`;
    row.style.bottom = '';
  }

  closeActions(): void {
    if (this.actionRow) { this.actionRow.remove(); this.actionRow = null; }
    if (this.activeMark) {
      this.activeMark.element.classList.remove('cmd-mark-active');
      this.activeMark = null;
    }
  }

  isActionsOpen(): boolean {
    return this.actionRow !== null;
  }

  handleKeydown(e: KeyboardEvent): boolean {
    if (this.actionRow && e.key === 'Escape') {
      e.preventDefault();
      this.closeActions();
      return true;
    }
    return false;
  }

  /** Forget every mark, keeping the manager attached. The pane calls this when
   *  it resets its terminal for a fresh shell: the marks point into a buffer
   *  that is about to be replaced. */
  clear(): void {
    this.closeActions();
    for (const mark of this.marks) mark.element.remove();
    this.marks = [];
    this.byBlock.clear();
  }

  dispose(): void {
    cancelAnimationFrame(this._raf);
    document.removeEventListener('mousedown', this.onDocumentMouseDown, true);
    this.unsubBlockChanged?.();
    this.unsubBlockChanged = null;
    this.resizeObserver?.disconnect();
    this.resizeObserver = null;
    this.clear();
  }
}

/** "1.4s", "220ms", "3m 07s" — how long a command took, for a mark's tooltip. */
function formatDuration(startedAt: number | null, endedAt: number | null): string {
  if (startedAt === null || endedAt === null || endedAt < startedAt) return 'duration unknown';
  const ms = endedAt - startedAt;
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  const minutes = Math.floor(ms / 60_000);
  const seconds = Math.round((ms % 60_000) / 1000);
  return `${minutes}m ${String(seconds).padStart(2, '0')}s`;
}
