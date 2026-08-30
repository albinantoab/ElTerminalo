import { Terminal, IMarker, IDecoration, IDisposable } from '@xterm/xterm';
import type { CommandStatus } from '../types';

export interface CommandBlock {
  promptMarker: IMarker;
  commandStartMarker: IMarker | null;
  outputStartMarker: IMarker | null;
  outputEndMarker: IMarker | null;
  exitCode: number | null;
  commandText: string | null;
  decorations: IDecoration[];
  /** `Date.now()` when the command started running (the C mark) and when it
   *  finished (D), or null. Both are wall clock rather than
   *  `performance.now()`: the only thing they are used for is the "ran for
   *  1.4s" in a mark's tooltip. */
  startedAt: number | null;
  endedAt: number | null;
}

export class ShellIntegration {
  private terminal: Terminal;
  private blocks: CommandBlock[] = [];
  private currentBlock: CommandBlock | null = null;
  private oscHandler: IDisposable | null = null;
  private active = false;
  // Where the shell is in the command it is running. Driven entirely by the
  // OSC 133 marks — nothing here polls, and a shell without integration stays
  // 'idle' forever, which is the honest answer for a pane that cannot say.
  private status: CommandStatus = 'idle';
  public onCommandFinished: ((exitCode: number) => void) | null = null;
  private commandFinishedListeners: ((block: CommandBlock, exitCode: number) => void)[] = [];
  private blockChangedListeners: ((block: CommandBlock) => void)[] = [];
  private statusListeners: ((status: CommandStatus) => void)[] = [];

  constructor(terminal: Terminal) {
    this.terminal = terminal;
    this.attach();
  }

  private attach(): void {
    this.oscHandler = this.terminal.parser.registerOscHandler(133, (data: string) => {
      // Only track marks in normal buffer (not alternate screen / TUI apps)
      if (this.terminal.buffer.active.type !== 'normal') return true;

      const parts = data.split(';');
      const mark = parts[0];
      switch (mark) {
        case 'A': this.handlePromptStart(); break;
        case 'B': this.handleCommandStart(); break;
        case 'C': this.handleOutputStart(data); break;
        case 'D': this.handleCommandFinished(parseInt(parts[1] || '0', 10)); break;
      }
      return true;
    });
  }

  private handlePromptStart(): void {
    this.active = true;

    // Finalize previous block if it has no D mark
    if (this.currentBlock && this.currentBlock.exitCode === null) {
      this.blocks.push(this.currentBlock);
    }

    const marker = this.terminal.registerMarker(0);
    if (!marker) return;

    const block: CommandBlock = {
      promptMarker: marker,
      commandStartMarker: null,
      outputStartMarker: null,
      outputEndMarker: null,
      exitCode: null,
      commandText: null,
      decorations: [],
      startedAt: null,
      endedAt: null,
    };

    // Clean up block when marker is disposed (scrollback trimmed)
    marker.onDispose(() => {
      const idx = this.blocks.indexOf(block);
      if (idx >= 0) this.blocks.splice(idx, 1);
      block.decorations.forEach(d => d.dispose());
    });

    this.currentBlock = block;
    this.addPromptDecoration(block);
  }

  private handleCommandStart(): void {
    if (!this.currentBlock) return;
    const marker = this.terminal.registerMarker(0);
    if (marker) this.currentBlock.commandStartMarker = marker;
  }

  private handleOutputStart(data: string): void {
    if (!this.currentBlock) return;
    const marker = this.terminal.registerMarker(0);
    if (marker) this.currentBlock.outputStartMarker = marker;
    this.currentBlock.startedAt = Date.now();

    // Parse command text from C mark: "C;cmd=<base64>"
    const cmdMatch = data.match(/cmd=([A-Za-z0-9+/=]+)/);
    if (cmdMatch) {
      try {
        this.currentBlock.commandText = atob(cmdMatch[1]);
      } catch { /* invalid base64 */ }
    }

    // The command is off: everything watching this pane — the tab's status dot,
    // the gutter mark that has just become worth drawing — hears it here.
    this.setStatus('running');
    this.emitBlockChanged(this.currentBlock);
  }

  private handleCommandFinished(exitCode: number): void {
    if (!this.currentBlock) return;
    const marker = this.terminal.registerMarker(0);
    if (marker) this.currentBlock.outputEndMarker = marker;
    this.currentBlock.exitCode = exitCode;
    this.currentBlock.endedAt = Date.now();
    this.addExitCodeDecoration(this.currentBlock);
    this.blocks.push(this.currentBlock);
    const finishedBlock = this.currentBlock;
    this.currentBlock = null;
    // Deliberately not back to 'idle': what the pane last did is what the tab's
    // dot is about, and it stays true until the next command starts.
    this.setStatus(exitCode === 0 ? 'done' : 'failed');
    this.emitBlockChanged(finishedBlock);
    if (this.onCommandFinished) this.onCommandFinished(exitCode);
    for (const listener of this.commandFinishedListeners) listener(finishedBlock, exitCode);
  }

  private addPromptDecoration(_block: CommandBlock): void {
    // Deliberately empty. xterm decorations overlay terminal *text* — they are
    // drawn in the cell grid — so they cannot be gutter marks. The marks live
    // outside the terminal entirely, as absolutely positioned elements over the
    // pane; see CommandMarks, which subscribes through onBlockChangedAdd.
  }

  private addExitCodeDecoration(_block: CommandBlock): void {
    // Same story: the exit code colours the block's gutter mark, not a
    // decoration inside the buffer.
  }

  private setStatus(next: CommandStatus): void {
    if (this.status === next) return;
    this.status = next;
    for (const listener of this.statusListeners) listener(next);
  }

  private emitBlockChanged(block: CommandBlock): void {
    for (const listener of this.blockChangedListeners) listener(block);
  }

  // --- Public API ---

  isActive(): boolean {
    return this.active;
  }

  /** Where this pane's shell is in the command it is running. */
  getStatus(): CommandStatus {
    return this.status;
  }

  /** Called whenever that changes. Returns an unsubscribe. */
  onStatusChangeAdd(listener: (status: CommandStatus) => void): () => void {
    this.statusListeners.push(listener);
    return () => {
      const idx = this.statusListeners.indexOf(listener);
      if (idx >= 0) this.statusListeners.splice(idx, 1);
    };
  }

  /** Called when a block starts running and again when it finishes — the two
   *  moments a gutter mark has to appear and then take its colour. Returns an
   *  unsubscribe. */
  onBlockChangedAdd(listener: (block: CommandBlock) => void): () => void {
    this.blockChangedListeners.push(listener);
    return () => {
      const idx = this.blockChangedListeners.indexOf(listener);
      if (idx >= 0) this.blockChangedListeners.splice(idx, 1);
    };
  }

  getBlocks(): CommandBlock[] {
    return this.blocks;
  }

  getLastExitCode(): number | null {
    if (this.blocks.length === 0) return null;
    return this.blocks[this.blocks.length - 1].exitCode;
  }

  onCommandFinishedAdd(listener: (block: CommandBlock, exitCode: number) => void): () => void {
    this.commandFinishedListeners.push(listener);
    return () => {
      const idx = this.commandFinishedListeners.indexOf(listener);
      if (idx >= 0) this.commandFinishedListeners.splice(idx, 1);
    };
  }

  /** Extract plain text output of a specific block from the terminal buffer. */
  getBlockOutput(block: CommandBlock): string | null {
    if (!block.outputStartMarker || !block.outputEndMarker) return null;

    const buf = this.terminal.buffer.active;
    const startLine = block.outputStartMarker.line;
    const endLine = block.outputEndMarker.line;
    const lines: string[] = [];

    for (let y = startLine; y < endLine; y++) {
      const line = buf.getLine(y);
      if (!line) continue;
      const text = line.translateToString(true);
      if (line.isWrapped && lines.length > 0) {
        lines[lines.length - 1] += text;
      } else {
        lines.push(text);
      }
    }

    return lines.join('\n').trim() || null;
  }

  /** Extract plain text output of the last completed command from the terminal buffer. */
  getLastCommandOutput(): string | null {
    if (this.blocks.length === 0) return null;
    return this.getBlockOutput(this.blocks[this.blocks.length - 1]);
  }

  navigateToBlock(direction: 'prev' | 'next'): void {
    if (this.blocks.length === 0) return;

    const buf = this.terminal.buffer.active;
    // Use the viewport top as reference, not cursor (cursor is always at latest prompt)
    const viewportTop = buf.viewportY;

    if (direction === 'prev') {
      for (let i = this.blocks.length - 1; i >= 0; i--) {
        const line = this.blocks[i].promptMarker.line;
        if (line < viewportTop) {
          this.terminal.scrollToLine(line);
          return;
        }
      }
      this.terminal.scrollToTop();
    } else {
      for (let i = 0; i < this.blocks.length; i++) {
        const line = this.blocks[i].promptMarker.line;
        if (line > viewportTop + 1) {
          this.terminal.scrollToLine(line);
          return;
        }
      }
      this.terminal.scrollToBottom();
    }
  }

  /** Forget every tracked block, keeping the OSC handler attached. Needed when
   *  a pane resets its terminal for a fresh shell: the buffers are replaced
   *  wholesale, so markers into the old ones would silently point at nothing.
   *  Disposing them cascades to whatever hangs off them (smart-render badges). */
  reset(): void {
    const all = this.currentBlock ? [...this.blocks, this.currentBlock] : [...this.blocks];
    for (const block of all) {
      block.decorations.forEach(d => d.dispose());
      block.promptMarker.dispose();
      block.commandStartMarker?.dispose();
      block.outputStartMarker?.dispose();
      block.outputEndMarker?.dispose();
    }
    this.blocks = [];
    this.currentBlock = null;
    this.active = false;
    // A fresh shell has run nothing, so the tab's dot must stop claiming the
    // dead one's last exit code.
    this.setStatus('idle');
  }

  dispose(): void {
    if (this.oscHandler) {
      this.oscHandler.dispose();
      this.oscHandler = null;
    }
    this.blockChangedListeners = [];
    this.statusListeners = [];
    for (const block of this.blocks) {
      block.decorations.forEach(d => d.dispose());
    }
    if (this.currentBlock) {
      this.currentBlock.decorations.forEach(d => d.dispose());
    }
    this.blocks = [];
    this.currentBlock = null;
  }
}
