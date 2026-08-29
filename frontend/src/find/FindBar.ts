import type { IDisposable } from '@xterm/xterm';
import type { ISearchOptions } from '@xterm/addon-search';
import { TerminalPane } from '../terminal/TerminalPane';
import { logWarn } from '../log';
import {
  FIND_MIN_HIGHLIGHT_TERM, FIND_STREAM_SAMPLE_MS, FIND_STREAM_WRITES_PER_SEC,
} from '../constants';

/** What the count slot says while highlighting is suspended. */
const PAUSED_NOTE = 'Highlighting paused while output streams';

export interface FindBarCallbacks {
  /** The user closed the bar — Escape or the × — so the focus they took from
   *  the terminal has to go back to it. Not called when the host tears the bar
   *  down itself (a tab switch, a pane closing): the focus is already moving. */
  onClose(): void;
}

/** A CSS custom property, as `#RRGGBB`.
 *
 *  The search addon hands its colours to xterm's decoration API, which parses
 *  them as CSS and throws on anything it cannot read; the themes come from
 *  disk, so `#rgb` and an 8-digit `#rrggbbaa` are both possible and neither is
 *  what the addon documents. Anything else falls back rather than taking the
 *  find bar — and with it the pane — down. */
function themeColor(name: string, fallback: string): string {
  let raw = '';
  try {
    raw = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  } catch {
    return fallback;
  }
  const m = /^#([0-9a-f]{3,8})$/i.exec(raw);
  if (!m) return fallback;
  const h = m[1];
  // #rgb and #rgba expand; #rrggbb and #rrggbbaa drop the alpha the addon
  // neither wants nor documents.
  if (h.length === 3 || h.length === 4) return `#${h[0]}${h[0]}${h[1]}${h[1]}${h[2]}${h[2]}`;
  if (h.length === 6 || h.length === 8) return `#${h.slice(0, 6)}`;
  return fallback;
}

/** Find in scrollback: one small bar in the top-right corner of a pane.
 *
 *  There is at most one of these in the whole window — the host creates it for
 *  the active pane and disposes it when the active pane changes — because a
 *  find bar is a place the keyboard is, and two of them would be two answers
 *  to the question of where Enter goes. */
export class FindBar {
  /** The pane this bar is searching. The host compares against it to decide
   *  whether Cmd+F should re-focus this bar or open a new one. */
  public readonly pane: TerminalPane;

  private readonly callbacks: FindBarCallbacks;
  private readonly root: HTMLElement;
  private readonly input: HTMLInputElement;
  private readonly count: HTMLElement;
  private readonly caseBtn: HTMLElement;
  private readonly regexBtn: HTMLElement;
  private readonly resultsSub: IDisposable;
  private readonly writeSub: IDisposable;
  private rateTimer: ReturnType<typeof setInterval> | null = null;

  private caseSensitive = false;
  private useRegex = false;
  private disposed = false;
  // Colours are read at construction — the bar is rebuilt on every open — and
  // again when the theme changes underneath an open one; see refreshTheme().
  private matchColor: string;
  private activeColor: string;
  // Whether the last search ran with highlight-all on. Tracked because the addon
  // leaves the previous term's decorations in place when a search runs without
  // them, so the step down to a one-character term has to clear them by hand —
  // and only then: clearDecorations() also drops the addon's cached term, which
  // is what makes a repeated Enter step to the *next* match rather than finding
  // the same one again.
  private decorationsOn = false;
  // The output-rate watch. Highlight-all costs a scan of the whole buffer, and
  // the addon re-arms that scan on every parsed write, so it is suspended while
  // a program is streaming — see sampleOutputRate().
  private writesInWindow = 0;
  private busyWindows = 0;
  private streamPaused = false;

  constructor(host: HTMLElement, pane: TerminalPane, callbacks: FindBarCallbacks) {
    this.pane = pane;
    this.callbacks = callbacks;
    this.matchColor = themeColor('--selection-bg', '#2a1a4e');
    this.activeColor = themeColor('--accent', '#5e17eb');

    const root = document.createElement('div');
    root.className = 'find-bar';
    root.innerHTML = `
      <input class="find-input" type="text" placeholder="Find" spellcheck="false" autocomplete="off" />
      <span class="find-count"></span>
      <button class="find-btn find-toggle" data-act="case" title="Match case" tabindex="-1">Aa</button>
      <button class="find-btn find-toggle" data-act="regex" title="Regular expression" tabindex="-1">.*</button>
      <button class="find-btn" data-act="prev" title="Previous match (CMD+SHIFT+G)" tabindex="-1">&#8593;</button>
      <button class="find-btn" data-act="next" title="Next match (CMD+G)" tabindex="-1">&#8595;</button>
      <button class="find-btn find-close" data-act="close" title="Close (ESC)" tabindex="-1">&times;</button>
    `;
    this.root = root;
    this.input = root.querySelector('.find-input') as HTMLInputElement;
    this.count = root.querySelector('.find-count') as HTMLElement;
    this.caseBtn = root.querySelector('[data-act="case"]') as HTMLElement;
    this.regexBtn = root.querySelector('[data-act="regex"]') as HTMLElement;

    // The pane's own mousedown listener moves the focus into the terminal, and
    // a button that steals it back would leave the bar unable to take a
    // keystroke. Both halves are needed: the host skips events that land in
    // here, and the buttons refuse the focus themselves.
    root.addEventListener('mousedown', (e) => {
      if (e.target !== this.input) e.preventDefault();
      e.stopPropagation();
    });
    root.addEventListener('click', (e) => {
      const act = (e.target as HTMLElement).closest('[data-act]')?.getAttribute('data-act');
      if (!act) return;
      e.preventDefault();
      e.stopPropagation();
      switch (act) {
        case 'case': this.caseSensitive = !this.caseSensitive; this.syncToggles(); this.research(); break;
        case 'regex': this.useRegex = !this.useRegex; this.syncToggles(); this.research(); break;
        case 'prev': this.findPrevious(); break;
        case 'next': this.findNext(false); break;
        case 'close': this.requestClose(); break;
      }
    });

    this.input.addEventListener('input', () => this.findNext(true));
    // Recommended by the addon: the active match is drawn *over* the selection,
    // so dropping it while the bar is not the thing with the keyboard lets the
    // ordinary selection show through.
    this.input.addEventListener('blur', () => {
      if (!this.disposed) this.pane.search.clearActiveDecoration();
    });

    this.resultsSub = this.pane.search.onDidChangeResults(({ resultIndex, resultCount }) => {
      this.renderCount(resultIndex, resultCount);
    });

    // How fast this pane is being written to. Counted here, judged on a timer:
    // the event itself must stay as close to free as it can be, because on a
    // flood it is the thing firing hundreds of times a second.
    this.writeSub = this.pane.terminal.onWriteParsed(() => { this.writesInWindow++; });
    this.rateTimer = setInterval(() => this.sampleOutputRate(), FIND_STREAM_SAMPLE_MS);

    this.syncToggles();
    host.appendChild(root);
    // The pane is on screen already, so this is a plain focus; select() is what
    // makes a second Cmd+F replace the previous term by typing over it.
    this.input.focus();
  }

  /** Put the keyboard back in the search field — Cmd+F while the bar is open. */
  focusInput(): void {
    if (this.disposed) return;
    this.input.focus();
    this.input.select();
  }

  /** Does this bar want the key? Returns true when it has dealt with it, so the
   *  host's own shortcut table never sees it.
   *
   *  Anything typed into the field is claimed but *not* cancelled: the input
   *  needs the character. Modifier combos are deliberately let through, so
   *  Cmd+T and Cmd+W still work with the cursor in the search field. */
  handleKeydown(e: KeyboardEvent): boolean {
    if (this.disposed) return false;

    // Cmd+G / Cmd+Shift+G step through matches from anywhere — the terminal has
    // the focus as often as the field does while reading results.
    if (e.metaKey && !e.ctrlKey && !e.altKey && e.key.toLowerCase() === 'g') {
      e.preventDefault();
      if (e.shiftKey) this.findPrevious(); else this.findNext(false);
      return true;
    }

    if (document.activeElement !== this.input) return false;

    if (e.key === 'Escape') {
      e.preventDefault();
      this.requestClose();
      return true;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      if (e.shiftKey) this.findPrevious(); else this.findNext(false);
      return true;
    }
    // Cmd combos go past: they are the app's own shortcuts, and the ones this
    // bar does not want (Cmd+T, Cmd+W) should still work with the cursor in
    // the field. Cmd+A / Cmd+C / Cmd+V are unhandled there and fall through to
    // the browser, which does the right thing in a text input.
    if (e.metaKey) return false;
    // Ctrl and Alt combos are claimed rather than passed on. They are how
    // custom commands are bound, and a search term is not the place to
    // discover that Ctrl+K also runs something in the shell — and claiming
    // them without cancelling leaves Ctrl+A / Ctrl+E doing what they do in
    // every other macOS text field.
    return true;
  }

  /** Close on the user's behalf and hand the keyboard back to the terminal. */
  requestClose(): void {
    if (this.disposed) return;
    this.dispose();
    this.callbacks.onClose();
  }

  /** Take the bar down. Silent — the host uses this when the bar's pane is
   *  going away or losing the focus for reasons of its own. */
  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.resultsSub.dispose();
    this.writeSub.dispose();
    if (this.rateTimer) {
      clearInterval(this.rateTimer);
      this.rateTimer = null;
    }
    // Leave no highlights behind on a pane nobody is searching any more.
    try {
      this.pane.search.clearDecorations();
    } catch (e) {
      // A pane disposed out from under us — nothing left to clear.
      logWarn('Failed to clear search highlights', e);
    }
    this.root.remove();
  }

  /** The theme changed while this bar was open. The colours were read from the
   *  page's CSS variables when it was built, and the addon copies them into the
   *  decorations it draws, so the highlights on screen are still the old
   *  theme's — and it will not rebuild them for a term and options that have not
   *  changed. Clearing them first is what makes the re-run repaint. */
  refreshTheme(): void {
    if (this.disposed) return;
    this.matchColor = themeColor('--selection-bg', '#2a1a4e');
    this.activeColor = themeColor('--accent', '#5e17eb');
    // Nothing on screen to repaint: an empty field never drew any highlights,
    // and a suspended one has already dropped the ones it had. Either way the
    // next search builds them from the colours just read.
    if (!this.input.value || this.streamPaused) return;
    try {
      this.pane.search.clearDecorations();
    } catch (e) {
      logWarn('Failed to clear search highlights for the new theme', e);
      return;
    }
    this.decorationsOn = false;
    // Incremental: this is not a step to the next match, only the same one
    // painted in the new colours.
    this.findNext(true);
  }

  /** One window of the output-rate watch.
   *
   *  The search addon re-runs its entire highlight pass — every cell of the
   *  scrollback, for every match — on a 200 ms debounce that is *re-armed* by
   *  each parsed write. A pane under `yes | head -200000` therefore pays for a
   *  full rescan every time the stream draws breath, on the main thread, while
   *  the user is not looking at the highlights anyway. So highlighting steps
   *  aside for the duration: two busy windows in a row is more than a second of
   *  continuous output, which is what tells a build's output apart from a prompt
   *  redraw. Any keystroke or Enter in the field brings it straight back. */
  private sampleOutputRate(): void {
    const writes = this.writesInWindow;
    this.writesInWindow = 0;
    if (this.disposed || !this.decorationsOn) return;
    if ((writes * 1000) / FIND_STREAM_SAMPLE_MS <= FIND_STREAM_WRITES_PER_SEC) {
      this.busyWindows = 0;
      return;
    }
    if (++this.busyWindows < 2) return;
    this.pauseHighlighting();
  }

  private pauseHighlighting(): void {
    if (this.streamPaused) return;
    this.streamPaused = true;
    this.decorationsOn = false;
    try {
      // Not just cosmetic: this also drops the addon's cached term, which is the
      // only thing that stops its own re-search timer re-running the pass.
      this.pane.search.clearDecorations();
    } catch (e) {
      logWarn('Failed to clear search highlights', e);
    }
    this.setState(PAUSED_NOTE, false);
  }

  private syncToggles(): void {
    this.caseBtn.classList.toggle('is-on', this.caseSensitive);
    this.regexBtn.classList.toggle('is-on', this.useRegex);
  }

  /** The options every search runs with. `decorations` is what makes *all*
   *  matches visible rather than just the one the cursor is on, and is also the
   *  only thing that makes the addon report a match count at all — so a search
   *  that goes without it navigates normally and shows no count.
   *
   *  It is left out below the two-character mark. Building those highlights
   *  means scanning the whole scrollback for every match, and a single character
   *  matches a large part of a 10k-line buffer: the most expensive pass there
   *  is, for the least useful picture. Two characters is where it starts being
   *  worth the scan. */
  private searchOptions(incremental: boolean): ISearchOptions {
    const options: ISearchOptions = {
      regex: this.useRegex,
      caseSensitive: this.caseSensitive,
      incremental,
    };
    if (!this.shouldHighlightAll()) return options;
    return {
      ...options,
      decorations: {
        matchBackground: this.matchColor,
        matchOverviewRuler: this.matchColor,
        activeMatchBackground: this.activeColor,
        activeMatchBorder: this.activeColor,
        activeMatchColorOverviewRuler: this.activeColor,
      },
    };
  }

  /** Whether this search is worth lighting every match of. */
  private shouldHighlightAll(): boolean {
    return !this.streamPaused && this.input.value.length >= FIND_MIN_HIGHLIGHT_TERM;
  }

  /** Re-run the current term after a toggle changed what it means. */
  private research(): void {
    this.findNext(true);
  }

  private findNext(incremental: boolean): void {
    this.run(() => this.pane.search.findNext(this.input.value, this.searchOptions(incremental)));
  }

  private findPrevious(): void {
    // `incremental` is documented as affecting findNext only, and a backwards
    // step is never incremental anyway: it is a deliberate jump.
    this.run(() => this.pane.search.findPrevious(this.input.value, this.searchOptions(false)));
  }

  /** Run one search, absorbing the one thing that can go wrong: with the regex
   *  toggle on, the term is compiled with `new RegExp` on every keystroke, so
   *  every half-typed pattern — `(`, `[a-`, `*` — throws on its way past. That
   *  is the user mid-thought, not a failure, so it is shown in the bar and
   *  nowhere else.
   *
   *  A pattern that compiles and then backtracks catastrophically — `(a+)+$`
   *  against a long line — is a different matter, and not one that is caught
   *  here: the addon matches on the main thread, so the window stops until the
   *  engine is done. That is xterm's search and the user's own pattern; there is
   *  no timeout to hand it and no worker to run it in. */
  private run(search: () => boolean): void {
    if (this.disposed) return;
    // Any deliberate search is the answer to a paused highlight: the user typed
    // something or pressed Enter, so it is worth paying for again. If the pane
    // is still streaming the watch will suspend it once more a second later.
    this.streamPaused = false;
    this.busyWindows = 0;

    const term = this.input.value;
    if (!term) {
      // The addon clears its own decorations for an empty term but reports no
      // result, so the count is ours to reset.
      this.pane.search.clearDecorations();
      this.decorationsOn = false;
      this.setState('', false);
      return;
    }
    // Stepping down to a term too short to highlight has to take the previous
    // term's decorations with it: a search that runs without `decorations` never
    // touches the ones already on screen.
    const highlightAll = this.shouldHighlightAll();
    if (this.decorationsOn && !highlightAll) this.pane.search.clearDecorations();
    this.decorationsOn = highlightAll;

    try {
      search();
      this.root.classList.remove('is-invalid');
      // Without decorations the addon reports nothing, so there is no count to
      // wait for and nothing to leave the previous term's on screen.
      if (!highlightAll) this.setState('', false);
    } catch {
      this.pane.search.clearDecorations();
      this.decorationsOn = false;
      this.setState(this.useRegex ? 'Bad pattern' : 'Error', true);
    }
  }

  private renderCount(resultIndex: number, resultCount: number): void {
    if (this.disposed) return;
    // A result event can still arrive just after highlighting was suspended —
    // the addon's own re-search timer was already armed — and "No results" is
    // not what happened.
    if (this.streamPaused) return;
    if (resultCount === 0) {
      this.setState(this.input.value ? 'No results' : '', false);
      return;
    }
    // The addon reports -1 for the index once its highlight limit is passed:
    // it stopped counting, so neither may we.
    this.setState(resultIndex >= 0 ? `${resultIndex + 1}/${resultCount}` : `${resultCount}+`, false);
  }

  private setState(text: string, invalid: boolean): void {
    this.count.textContent = text;
    // The slot is a few characters wide and anything longer than a count is
    // clipped in a narrow pane; the tooltip is where the rest of it lives.
    this.count.title = text;
    this.root.classList.toggle('is-invalid', invalid);
  }
}
