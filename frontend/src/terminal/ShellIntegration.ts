import { Terminal, IMarker, IDecoration, IDisposable } from '@xterm/xterm';

export interface CommandBlock {
  promptMarker: IMarker;
  commandStartMarker: IMarker | null;
  outputStartMarker: IMarker | null;
  outputEndMarker: IMarker | null;
  exitCode: number | null;
  decorations: IDecoration[];
}

export class ShellIntegration {
  private terminal: Terminal;
  private blocks: CommandBlock[] = [];
  private currentBlock: CommandBlock | null = null;
  private oscHandler: IDisposable | null = null;
  private active = false;
  public onCommandFinished: ((exitCode: number) => void) | null = null;
  private commandFinishedListeners: ((block: CommandBlock, exitCode: number) => void)[] = [];

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
        case 'C': this.handleOutputStart(); break;
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
      decorations: [],
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

  private handleOutputStart(): void {
    if (!this.currentBlock) return;
    const marker = this.terminal.registerMarker(0);
    if (marker) this.currentBlock.outputStartMarker = marker;
  }

  private handleCommandFinished(exitCode: number): void {
    if (!this.currentBlock) return;
    const marker = this.terminal.registerMarker(0);
    if (marker) this.currentBlock.outputEndMarker = marker;
    this.currentBlock.exitCode = exitCode;
    this.addExitCodeDecoration(this.currentBlock);
    this.blocks.push(this.currentBlock);
    const finishedBlock = this.currentBlock;
    this.currentBlock = null;
    if (this.onCommandFinished) this.onCommandFinished(exitCode);
    for (const listener of this.commandFinishedListeners) listener(finishedBlock, exitCode);
  }

  private addPromptDecoration(_block: CommandBlock): void {
    // Decorations overlay terminal text — not usable for gutter marks.
    // Visual indicators are handled externally (pane border, status bar).
  }

  private addExitCodeDecoration(_block: CommandBlock): void {
    // Exit code visuals handled externally.
  }

  // --- Public API ---

  isActive(): boolean {
    return this.active;
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

  dispose(): void {
    if (this.oscHandler) {
      this.oscHandler.dispose();
      this.oscHandler = null;
    }
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
