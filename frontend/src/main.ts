import { TerminalPane } from './terminal/TerminalPane';
import { AppTheme, themeFromDTO, applyThemeToCSS } from './theme/themes';
import { PaneInfo, SplitNode, Tab, SavedSplitNode, SavedState, CustomCommand, PaletteCommand } from './types';
import { CommandPalette } from './palette/CommandPalette';
import { CommandWizard } from './wizard/CommandWizard';
import { ThemeWizard } from './wizard/ThemeWizard';
import { StateManager } from './state/StateManager';
import { StatusModal } from './status/StatusModal';
import { AskAI } from './ai/AskAI';
import { HistoryModal } from './history/HistoryModal';
import { escHtml, generateId, waitForLayout, utf8ToBase64, bytesToBase64 } from './utils';
import {
  MAX_TABS, DOUBLE_CLICK_DELAY_MS, MIN_SPLIT_RATIO, MAX_SPLIT_RATIO,
  DEFAULT_SPLIT_RATIO, SPATIAL_NAV_THRESHOLD, STATE_SAVE_INTERVAL_MS, CMD,
} from './constants';
import './types/wails.d.ts';

class ElTerminalo {
  private tabs: Tab[] = [];
  private activeTabIndex = 0;
  private themes: AppTheme[] = [];
  private currentTheme!: AppTheme;
  private customCommands: CustomCommand[] = [];
  private renamingTabIndex = -1;

  private container!: HTMLElement;
  private tabBar!: HTMLElement;
  private statusbar!: HTMLElement;

  private palette!: CommandPalette;
  private wizard!: CommandWizard;
  private themeWizard!: ThemeWizard;
  private stateManager!: StateManager;
  private statusModal!: StatusModal;
  private askAI!: AskAI;
  private historyModal!: HistoryModal;
  private aiGenerating = false;
  private modelUpdateAvailable = false;

  // Current tab helpers
  private get tab(): Tab { return this.tabs[this.activeTabIndex]; }
  private get panes(): PaneInfo[] { return this.tab?.panes || []; }
  private get activeIndex(): number { return this.tab?.activeIndex || 0; }
  private set activeIndex(v: number) { if (this.tab) this.tab.activeIndex = v; }
  private get layoutRoot(): SplitNode | null { return this.tab?.layoutRoot || null; }
  private set layoutRoot(v: SplitNode | null) { if (this.tab) this.tab.layoutRoot = v; }

  async init(): Promise<void> {
    this.container = document.getElementById('pane-container')!;
    this.tabBar = document.getElementById('tab-bar')!;
    this.statusbar = document.getElementById('statusbar')!;
    const paletteOverlay = document.getElementById('palette-overlay')!;
    const wizardOverlay = document.getElementById('wizard-overlay')!;

    // Create composed modules with callbacks
    this.palette = new CommandPalette(paletteOverlay, {
      getBuiltInCommands: () => this.getBuiltInCommands(),
      getCustomCommands: () => this.customCommands,
      getActiveSessionId: () => this.panes[this.activeIndex]?.pane?.sessionId || '',
      getActivePaneCWD: () => this.getActivePaneCWD(),
      focusActivePane: () => this.focusActivePane(),
      refreshCustomCommands: () => this.refreshCustomCommands(),
      onEditCommand: (cmd) => this.editCustomCommand(cmd),
      onEditTheme: (cmd) => this.editTheme(cmd),
      onDeleteTheme: (cmd) => this.deleteTheme(cmd),
    });

    this.wizard = new CommandWizard(wizardOverlay, {
      getActivePaneCWD: () => this.getActivePaneCWD(),
      focusActivePane: () => this.focusActivePane(),
      refreshCustomCommands: () => this.refreshCustomCommands(),
      getCustomCommands: () => this.customCommands,
    });

    const themeWizardOverlay = document.getElementById('theme-wizard-overlay')!;
    this.themeWizard = new ThemeWizard(themeWizardOverlay, {
      onSave: async () => {
        const themeDTOs = await window.go.main.App.GetThemes();
        this.themes = themeDTOs.map(themeFromDTO);
        // Re-apply current theme in case it was edited
        const updated = this.themes.find(t => t.name === this.currentTheme.name);
        if (updated) {
          this.currentTheme = updated;
          applyThemeToCSS(updated);
          for (const tab of this.tabs) {
            for (const p of tab.panes) p.pane.setTheme(updated.xterm);
          }
        }
      },
      focusActivePane: () => {
        if (this.panes[this.activeIndex]) this.panes[this.activeIndex].pane.focus();
      },
    });

    const statusOverlay = document.getElementById('status-overlay')!;
    this.statusModal = new StatusModal(statusOverlay, {
      getTabs: () => this.tabs,
      getActiveTabIndex: () => this.activeTabIndex,
      focusActivePane: () => this.focusActivePane(),
      switchToPane: (tabIndex, paneIndex) => {
        this.switchToTab(tabIndex);
        requestAnimationFrame(() => this.setActive(paneIndex));
      },
    });

    const aiOverlay = document.getElementById('ai-overlay')!;
    this.askAI = new AskAI(aiOverlay, {
      getActiveSessionId: () => this.panes[this.activeIndex]?.pane?.sessionId || '',
      getActivePaneCWD: () => this.getActivePaneCWD(),
      focusActivePane: () => this.focusActivePane(),
      setAILoading: (loading) => this.setAILoading(loading),
    });

    const historyOverlay = document.getElementById('history-overlay')!;
    this.historyModal = new HistoryModal(historyOverlay, {
      getActiveSessionId: () => this.panes[this.activeIndex]?.pane?.sessionId || '',
      getActivePaneCWD: () => this.getActivePaneCWD(),
      focusActivePane: () => this.focusActivePane(),
    });

    this.stateManager = new StateManager({
      getTabs: () => this.tabs,
      getActiveTabIndex: () => this.activeTabIndex,
      getCurrentThemeName: () => this.currentTheme.name,
    });

    const themeDTOs = await window.go.main.App.GetThemes();
    this.themes = themeDTOs.map(themeFromDTO);
    this.currentTheme = this.themes[0];

    await this.refreshCustomCommands();

    // Download AI model during splash if needed
    await this.ensureModelReady();

    const restored = await this.restoreState();
    if (!restored) {
      applyThemeToCSS(this.currentTheme);
      await this.createTab('Terminalo 1');
    }

    this.switchToTab(this.activeTabIndex);
    window.addEventListener('keydown', (e: KeyboardEvent) => this.handleKeydown(e), true);

    // Re-focus active pane when app regains focus (after lock/unlock, screen switch, etc.)
    // Without this, shortcuts stop working until the user clicks the terminal.
    // Also hide the active pane border when the app loses focus.
    document.addEventListener('visibilitychange', () => {
      if (!document.hidden) this.focusActivePane();
    });
    window.addEventListener('focus', () => {
      document.body.classList.remove('app-blurred');
      this.focusActivePane();
    });
    window.addEventListener('blur', () => {
      document.body.classList.add('app-blurred');
    });

    this.renderStatusBar();
    setInterval(() => this.stateManager.save(), STATE_SAVE_INTERVAL_MS);

    // Dismiss splash screen
    this.dismissSplash();

    // AI model loads lazily on first Cmd+K use, unloads after idle.

    // Check for app + model updates in background (non-blocking), then every 6 hours
    this.checkForUpdate();
    this.checkModelUpdate();
    setInterval(() => { this.checkForUpdate(); this.checkModelUpdate(); }, 6 * 60 * 60 * 1000);

    // Listen for close confirmation request from the Go backend
    window.runtime.EventsOn('app:confirm-close', () => this.showCloseConfirmation());

    // Handle file drops — read via HTML5 API, save to temp via Go
    document.addEventListener('dragover', (e) => e.preventDefault(), true);
    document.addEventListener('drop', async (e) => {
      e.preventDefault();
      if (!e.dataTransfer?.files?.length) return;
      // Find which pane the drop landed on
      const target = e.target as HTMLElement;
      const ap = this.panes.find(p => p.element.contains(target)) || this.panes[this.activeIndex];
      if (!ap?.pane.sessionId) return;
      const paths: string[] = [];
      for (let i = 0; i < e.dataTransfer.files.length; i++) {
        const f = e.dataTransfer.files[i];
        if (!f) continue;
        try {
          const buf = await f.arrayBuffer();
          const b64 = bytesToBase64(new Uint8Array(buf));
          const path = await window.go.main.App.SaveDroppedFile(f.name, b64);
          if (path) paths.push(path);
        } catch { /* skip failed files */ }
      }
      if (paths.length > 0) {
        const escaped = paths.map(p => this.shellEscape(p)).join(' ');
        window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64(escaped + ' '));
      }
    }, true);
  }

  private shellEscape(path: string): string {
    if (/^[a-zA-Z0-9_.\/~-]+$/.test(path)) return path;
    return "'" + path.replace(/'/g, "'\\''") + "'";
  }


  private updateInfo: { available: boolean; latestVersion: string; url: string } | null = null;

  private async checkForUpdate(): Promise<void> {
    try {
      const info = await window.go.main.App.CheckForUpdate();
      if (info.available) {
        this.updateInfo = { available: true, latestVersion: info.latestVersion, url: info.url };
        this.renderStatusBar();
      }
    } catch (_) {
      // Silently ignore — update check is best-effort
    }
  }

  private async ensureModelReady(): Promise<void> {
    try {
      const downloaded = await window.go.main.App.IsModelDownloaded();
      if (downloaded) return;

      const status = document.getElementById('splash-status');
      const bar = document.getElementById('splash-bar');
      if (status) status.textContent = 'Downloading AI model... (Esc to skip)';
      if (bar) {
        bar.classList.add('downloading');
        bar.style.width = '0%';
      }

      // Allow Escape to cancel the download
      let skipped = false;
      const onKey = (e: KeyboardEvent) => {
        if (e.key === 'Escape') {
          skipped = true;
          window.go.main.App.SkipDownload();
          if (status) status.textContent = 'Skipping download...';
        }
      };
      document.addEventListener('keydown', onKey, true);

      // Listen for download progress events
      const unsub = window.runtime.EventsOn('model:download-progress', (data: { downloaded: number; total: number }) => {
        if (data.total > 0 && !skipped) {
          const pct = Math.round((data.downloaded / data.total) * 100);
          const mbDown = (data.downloaded / 1024 / 1024).toFixed(1);
          const mbTotal = (data.total / 1024 / 1024).toFixed(1);
          if (status) status.textContent = `Downloading AI model... ${mbDown} / ${mbTotal} MB (${pct}%) — Esc to skip`;
          if (bar) bar.style.width = `${pct}%`;
        }
      });

      try {
        await window.go.main.App.DownloadModel();
        if (!skipped && status) status.textContent = 'Model ready';
        if (!skipped && bar) bar.style.width = '100%';
      } catch {
        if (skipped) {
          if (status) status.textContent = 'Download skipped — use Cmd+K to retry later';
        } else {
          if (status) status.textContent = 'Download failed — use Cmd+K to retry later';
        }
      }

      unsub();
      document.removeEventListener('keydown', onKey, true);
    } catch (e) {
      console.error('Model check failed:', e);
    }
  }

  private dismissSplash(): void {
    const splash = document.getElementById('splash');
    if (!splash) return;
    // Let the progress bar finish, then fade out
    const minDisplayMs = 2000;
    const elapsed = performance.now();
    const remaining = Math.max(0, minDisplayMs - elapsed);
    setTimeout(() => {
      splash.classList.add('splash-exit');
      setTimeout(() => splash.remove(), 600);
    }, remaining);
  }

  // --- Tab Management ---

  private async createTab(name?: string): Promise<void> {
    if (this.tabs.length >= MAX_TABS) return;
    let tabName = name;
    if (!tabName) {
      const used = new Set(this.tabs.map(t => t.name));
      let n = 1;
      while (used.has(`Terminalo ${n}`)) n++;
      tabName = `Terminalo ${n}`;
    }
    const tab: Tab = {
      id: generateId('tab'),
      name: tabName,
      panes: [],
      activeIndex: 0,
      layoutRoot: null,
    };
    this.tabs.push(tab);
    this.activeTabIndex = this.tabs.length - 1;

    const pane = await this.createPaneForTab(tab);
    tab.layoutRoot = { type: 'leaf', paneInfo: pane };

    this.renderTabBar();
    this.renderLayout();
    await waitForLayout();
    pane.pane.fit();
    await pane.pane.connect();
    this.setActive(0);
    this.stateManager.save();
  }

  private closeTab(index: number): void {
    if (this.tabs.length <= 1) return;

    const tab = this.tabs[index];
    for (const p of tab.panes) {
      p.pane.dispose();
    }
    this.tabs.splice(index, 1);

    if (this.activeTabIndex >= this.tabs.length) {
      this.activeTabIndex = this.tabs.length - 1;
    }

    this.switchToTab(this.activeTabIndex);
    this.stateManager.save();
  }

  private switchToTab(index: number): void {
    if (index < 0 || index >= this.tabs.length) return;
    this.activeTabIndex = index;
    this.renderTabBar();
    this.renderLayout();
    requestAnimationFrame(() => {
      this.fitAll();
      if (this.panes.length > 0) {
        this.setActive(this.activeIndex);
      }
    });
  }

  private renameTab(index: number, newName: string): void {
    if (index >= 0 && index < this.tabs.length && newName.trim()) {
      this.tabs[index].name = newName.trim();
      this.renderTabBar();
      this.stateManager.save();
    }
  }

  private renderTabBar(): void {
    const tabs = this.tabs.map((t, i) => {
      const isActive = i === this.activeTabIndex;
      if (this.renamingTabIndex === i) {
        return `<div class="tab-item active">
          <input class="tab-rename-input" type="text" value="${escHtml(t.name)}" data-index="${i}" />
        </div>`;
      }
      return `<div class="tab-item ${isActive ? 'active' : ''}" data-index="${i}">
        <span class="tab-shortcut">${i + 1}</span>
        <span class="tab-name">${escHtml(t.name)}</span>
        ${this.tabs.length > 1 ? `<span class="tab-close" data-close="${i}">×</span>` : ''}
      </div>`;
    }).join('');

    this.tabBar.innerHTML = `
      <div class="tab-list">${tabs}</div>
      <div class="tab-new" title="${CMD.NEW_TAB.name} (${CMD.NEW_TAB.shortcut})">+</div>
    `;

    // Wire tab clicks — delay single click to detect double click
    this.tabBar.querySelectorAll('.tab-item[data-index]').forEach(el => {
      const idx = parseInt(el.getAttribute('data-index') || '0');
      let clickTimer: ReturnType<typeof setTimeout> | null = null;

      el.addEventListener('click', (e) => {
        if ((e.target as HTMLElement).classList.contains('tab-close')) return;
        if (clickTimer) { clearTimeout(clickTimer); clickTimer = null; return; }
        clickTimer = setTimeout(() => {
          clickTimer = null;
          this.switchToTab(idx);
        }, DOUBLE_CLICK_DELAY_MS);
      });

      el.addEventListener('dblclick', (e) => {
        e.preventDefault();
        if (clickTimer) { clearTimeout(clickTimer); clickTimer = null; }
        this.renamingTabIndex = idx;
        this.renderTabBar();
        requestAnimationFrame(() => {
          const input = this.tabBar.querySelector('.tab-rename-input') as HTMLInputElement;
          if (input) {
            input.focus();
            input.select();
            input.addEventListener('blur', () => {
              this.renameTab(idx, input.value);
              this.renamingTabIndex = -1;
              this.renderTabBar();
            });
            input.addEventListener('keydown', (ev: KeyboardEvent) => {
              ev.stopPropagation();
              if (ev.key === 'Enter') { ev.preventDefault(); input.blur(); }
              if (ev.key === 'Escape') { ev.preventDefault(); this.renamingTabIndex = -1; this.renderTabBar(); }
            });
          }
        });
      });
    });

    // Wire close buttons
    this.tabBar.querySelectorAll('.tab-close').forEach(el => {
      el.addEventListener('click', (e) => {
        e.stopPropagation();
        const idx = parseInt(el.getAttribute('data-close') || '0');
        this.closeTab(idx);
      });
    });

    // Wire new tab button
    this.tabBar.querySelector('.tab-new')?.addEventListener('click', () => {
      this.createTab();
    });
  }

  // --- State Persistence ---

  private async restoreState(): Promise<boolean> {
    const state = await this.stateManager.load();
    if (!state) return false;

    try {
      if (!state.tabs && state.layout) {
        // v1 migration: single layout -> single tab
        applyThemeToCSS(this.currentTheme);
        const tab: Tab = { id: 'tab-migrated', name: 'Terminal', panes: [], activeIndex: 0, layoutRoot: null };
        this.tabs.push(tab);
        this.activeTabIndex = 0;
        tab.layoutRoot = await this.restoreLayoutNode(state.layout, tab);
        this.renderTabBar();
        this.renderLayout();
        await waitForLayout();
        for (const p of tab.panes) p.pane.fit();
        return true;
      }

      if (!state.tabs || state.tabs.length === 0) return false;

      const theme = this.themes.find(t => t.name === state.themeName);
      if (theme) this.currentTheme = theme;
      applyThemeToCSS(this.currentTheme);

      for (const savedTab of state.tabs) {
        const tab: Tab = {
          id: generateId('tab'),
          name: savedTab.name,
          panes: [],
          activeIndex: 0,
          layoutRoot: null,
        };
        this.tabs.push(tab);
        tab.layoutRoot = await this.restoreLayoutNode(savedTab.layout, tab);
      }

      this.activeTabIndex = Math.min(state.activeTabIndex || 0, this.tabs.length - 1);
      this.renderTabBar();
      this.renderLayout();
      await waitForLayout();
      for (const p of this.tab.panes) p.pane.fit();

      return true;
    } catch (e) {
      console.error('Failed to restore state:', e);
      return false;
    }
  }

  private async restoreLayoutNode(saved: SavedSplitNode, tab: Tab): Promise<SplitNode> {
    if (saved.type === 'leaf') {
      const pane = await this.createPaneForTab(tab);
      await pane.pane.connect(saved.cwd || '');
      return { type: 'leaf', paneInfo: pane };
    }
    if (saved.type === 'split' && saved.children) {
      const [first, second] = await Promise.all([
        this.restoreLayoutNode(saved.children[0], tab),
        this.restoreLayoutNode(saved.children[1], tab),
      ]);
      return { type: 'split', direction: saved.direction, ratio: saved.ratio ?? DEFAULT_SPLIT_RATIO, children: [first, second] };
    }
    const pane = await this.createPaneForTab(tab);
    await pane.pane.connect();
    return { type: 'leaf', paneInfo: pane };
  }

  // --- Pane Management ---

  private async createPaneForTab(tab: Tab): Promise<PaneInfo> {
    const el = document.createElement('div');
    el.className = 'pane-leaf';
    el.style.flex = '1';

    const pane = new TerminalPane(el, this.currentTheme.xterm);
    const id = generateId('pane');
    const info: PaneInfo = { id, pane, element: el };

    pane.setContextActions({
      splitVertical: () => this.splitPane('vertical'),
      splitHorizontal: () => this.splitPane('horizontal'),
      closePane: () => this.closeActivePane(),
    });

    pane.smartRender.onBadgesChanged = () => this.renderStatusBar();

    // Record commands in history database
    pane.shellIntegration.onCommandFinishedAdd(async (block, exitCode) => {
      if (!block.commandText || !pane.sessionId) return;
      try {
        const cwd = await pane.getCWD();
        await window.go.main.App.RecordCommand(block.commandText, cwd, exitCode, pane.sessionId);
      } catch { /* best-effort */ }
    });

    el.addEventListener('mousedown', (e) => {
      if (e.button === 2) return; // don't steal focus on right-click
      const idx = tab.panes.indexOf(info);
      if (idx >= 0 && tab === this.tab) this.setActive(idx);
    });

    tab.panes.push(info);
    return info;
  }

  private async createPane(): Promise<PaneInfo> {
    return this.createPaneForTab(this.tab);
  }

  private findParent(node: SplitNode, paneId: string): { parent: SplitNode; childIndex: 0 | 1 } | null {
    if (node.type !== 'split' || !node.children) return null;
    for (let i = 0; i < 2; i++) {
      const child = node.children[i as 0 | 1];
      if (child.type === 'leaf' && child.paneInfo?.id === paneId) {
        return { parent: node, childIndex: i as 0 | 1 };
      }
      const found = this.findParent(child, paneId);
      if (found) return found;
    }
    return null;
  }

  private async splitPane(direction: 'vertical' | 'horizontal'): Promise<void> {
    if (!this.layoutRoot) return;
    const activePane = this.panes[this.activeIndex];
    if (!activePane) return;

    const newPane = await this.createPane();
    const replaceInTree = (node: SplitNode): SplitNode => {
      if (node.type === 'leaf' && node.paneInfo?.id === activePane.id) {
        return { type: 'split', direction, ratio: DEFAULT_SPLIT_RATIO, children: [{ type: 'leaf', paneInfo: activePane }, { type: 'leaf', paneInfo: newPane }] };
      }
      if (node.type === 'split' && node.children) {
        return { ...node, children: [replaceInTree(node.children[0]), replaceInTree(node.children[1])] };
      }
      return node;
    };

    this.layoutRoot = replaceInTree(this.layoutRoot);
    this.renderLayout();
    await waitForLayout();
    this.fitAll();
    await newPane.pane.connect();
    this.setActive(this.panes.indexOf(newPane));
    this.stateManager.save();
  }

  private closeActivePane(): void {
    if (this.panes.length <= 1 || !this.layoutRoot) return;
    const activePane = this.panes[this.activeIndex];
    if (!activePane) return;

    const result = this.findParent(this.layoutRoot, activePane.id);
    if (result) {
      const sibling = result.parent.children![result.childIndex === 0 ? 1 : 0];
      result.parent.type = sibling.type;
      result.parent.direction = sibling.direction;
      result.parent.ratio = sibling.ratio;
      result.parent.paneInfo = sibling.paneInfo;
      result.parent.children = sibling.children;
    }

    const removedIdx = this.panes.indexOf(activePane);
    this.panes.splice(removedIdx, 1);
    activePane.pane.dispose();

    if (this.activeIndex >= this.panes.length) this.activeIndex = this.panes.length - 1;

    this.renderLayout();
    requestAnimationFrame(() => this.fitAll());
    this.setActive(this.activeIndex);
    this.stateManager.save();
  }

  private renderLayout(): void {
    this.container.innerHTML = '';
    if (!this.layoutRoot) return;
    this.container.appendChild(this.renderNode(this.layoutRoot));
  }

  private renderNode(node: SplitNode): HTMLElement {
    if (node.type === 'leaf' && node.paneInfo) {
      node.paneInfo.element.style.flex = '1';
      return node.paneInfo.element;
    }
    if (node.type === 'split' && node.children) {
      const isVertical = node.direction === 'vertical';
      const ratio = node.ratio ?? DEFAULT_SPLIT_RATIO;
      const wrapper = document.createElement('div');
      wrapper.className = isVertical ? 'pane-split-vertical' : 'pane-split-horizontal';
      wrapper.style.flex = '1';
      const first = this.renderNode(node.children[0]);
      const second = this.renderNode(node.children[1]);
      first.style.flex = `${ratio}`;
      second.style.flex = `${1 - ratio}`;
      const divider = document.createElement('div');
      divider.className = isVertical ? 'pane-divider-v' : 'pane-divider-h';
      divider.addEventListener('mousedown', (e) => {
        e.preventDefault();
        const startPos = isVertical ? e.clientX : e.clientY;
        const wrapperRect = wrapper.getBoundingClientRect();
        const totalSize = isVertical ? wrapperRect.width : wrapperRect.height;
        const startRatio = ratio;
        const onMouseMove = (ev: MouseEvent) => {
          const delta = ((isVertical ? ev.clientX : ev.clientY) - startPos) / totalSize;
          const newRatio = Math.max(MIN_SPLIT_RATIO, Math.min(MAX_SPLIT_RATIO, startRatio + delta));
          node.ratio = newRatio;
          first.style.flex = `${newRatio}`;
          second.style.flex = `${1 - newRatio}`;
        };
        const onMouseUp = () => {
          document.removeEventListener('mousemove', onMouseMove);
          document.removeEventListener('mouseup', onMouseUp);
          document.body.style.cursor = '';
          document.body.style.userSelect = '';
          this.fitAll();
          this.stateManager.save();
        };
        document.body.style.cursor = isVertical ? 'col-resize' : 'row-resize';
        document.body.style.userSelect = 'none';
        document.addEventListener('mousemove', onMouseMove);
        document.addEventListener('mouseup', onMouseUp);
      });
      wrapper.appendChild(first);
      wrapper.appendChild(divider);
      wrapper.appendChild(second);
      return wrapper;
    }
    return document.createElement('div');
  }

  private fitAll(): void {
    for (const p of this.panes) p.pane.fit();
  }

  private setActive(index: number): void {
    if (index < 0 || index >= this.panes.length) return;
    for (const p of this.panes) {
      p.element.classList.remove('active');
      p.pane.terminal.options.cursorBlink = false;
    }
    this.activeIndex = index;
    this.panes[index].element.classList.add('active');
    this.panes[index].pane.terminal.options.cursorBlink = true;
    this.panes[index].pane.focus();
    this.renderStatusBar();
  }

  // --- Theme ---

  private setTheme(name: string): void {
    const theme = this.themes.find(t => t.name.toLowerCase() === name.toLowerCase());
    if (!theme) return;
    this.currentTheme = theme;
    applyThemeToCSS(theme);
    for (const tab of this.tabs) {
      for (const p of tab.panes) p.pane.setTheme(theme.xterm);
    }
    this.renderStatusBar();
    this.stateManager.save();
  }

  // --- Status Bar ---

  private renderStatusBar(): void {
    const updateBadge = this.updateInfo?.available
      ? `<a class="status-update" id="status-update-link">Update v${escHtml(this.updateInfo.latestVersion)} available</a><span class="status-sep">·</span>`
      : '';

    this.statusbar.innerHTML = `
      <div class="status-left">${updateBadge}</div>
      <div class="status-right">
        <span class="status-key">${CMD.AI_COMMAND.shortcut}</span><span class="status-label">ai</span>
        <span class="status-sep">·</span>
        <span class="status-key">${CMD.COMMAND_PALETTE.shortcut}</span><span class="status-label">commands</span>
        <span class="status-sep">·</span>
        <span class="status-key">${CMD.SESSION_STATUS.shortcut}</span><span class="status-label">status</span>
        <span class="status-sep">·</span>
        <span class="status-key">${CMD.SPLIT_VERTICAL.shortcut}</span><span class="status-label">vsplit</span>
        <span class="status-sep">·</span>
        <span class="status-key">${CMD.SPLIT_HORIZONTAL.shortcut}</span><span class="status-label">hsplit</span>
        <span class="status-sep">·</span>
        <span class="status-key">${CMD.CLOSE_PANE.shortcut}</span><span class="status-label">close pane</span>
        <span class="status-sep">·</span>
        <span class="status-key">${CMD.CLEAR_TERMINAL.shortcut}</span><span class="status-label">clear</span>
      </div>
    `;

    if (this.updateInfo?.available) {
      document.getElementById('status-update-link')?.addEventListener('click', () => {
        this.promptUpdate();
      });
    }

  }

  private showCloseConfirmation(): void {
    // Don't stack multiple dialogs
    if (document.querySelector('.close-overlay')) return;

    const activeSessions = this.tabs.reduce((n, t) => n + t.panes.length, 0);

    const overlay = document.createElement('div');
    overlay.className = 'update-overlay close-overlay';
    overlay.innerHTML = `<div class="update-dialog">
      <div class="update-dialog-title">Quit El Terminalo?</div>
      <div class="update-dialog-body">
        ${activeSessions} active session${activeSessions !== 1 ? 's' : ''} will be terminated.
      </div>
      <div class="update-dialog-actions">
        <button class="theme-btn theme-btn-cancel" id="close-cancel">Cancel</button>
        <button class="theme-btn theme-btn-save" id="close-confirm" style="background:#f85149;border-color:#f85149;">Quit</button>
      </div>
    </div>`;
    document.body.appendChild(overlay);

    const dismiss = () => overlay.remove();

    document.getElementById('close-cancel')?.addEventListener('click', dismiss);
    document.getElementById('close-confirm')?.addEventListener('click', () => {
      this.stateManager.save();
      window.go.main.App.ConfirmQuit();
    });

    // Allow Escape to cancel
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { dismiss(); document.removeEventListener('keydown', onKey, true); }
      if (e.key === 'Enter') { this.stateManager.save(); window.go.main.App.ConfirmQuit(); }
      e.stopPropagation();
    };
    document.addEventListener('keydown', onKey, true);
  }

  private async promptUpdate(): Promise<void> {
    if (!this.updateInfo?.available) return;

    // Show confirmation overlay
    const overlay = document.createElement('div');
    overlay.className = 'update-overlay';
    overlay.innerHTML = `<div class="update-dialog">
      <div class="update-dialog-title">Update Available</div>
      <div class="update-dialog-body">
        v${escHtml(this.updateInfo.latestVersion)} is ready to install.<br>
        The app will restart automatically.
      </div>
      <div class="update-dialog-actions">
        <button class="theme-btn theme-btn-cancel" id="update-cancel">Later</button>
        <button class="theme-btn theme-btn-save" id="update-now">Update Now</button>
      </div>
    </div>`;
    document.body.appendChild(overlay);

    document.getElementById('update-cancel')?.addEventListener('click', () => overlay.remove());
    document.getElementById('update-now')?.addEventListener('click', async () => {
      const btn = document.getElementById('update-now') as HTMLButtonElement;
      btn.textContent = 'Downloading...';
      btn.disabled = true;
      try {
        await window.go.main.App.ApplyUpdate();
      } catch (e) {
        btn.textContent = 'Failed — retry';
        btn.disabled = false;
        console.error('Update failed:', e);
      }
    });
  }

  // --- Custom Commands ---

  private async refreshCustomCommands(): Promise<void> {
    try {
      const globalCmds = await window.go.main.App.GetGlobalCommands() || [];
      let localCmds: CustomCommand[] = [];
      try {
        if (this.tab?.panes[this.activeIndex]?.pane?.sessionId) {
          const cwd = await this.tab.panes[this.activeIndex].pane.getCWD();
          if (cwd) localCmds = await window.go.main.App.GetLocalCommands(cwd) || [];
        }
      } catch (_) {}
      this.customCommands = [...localCmds, ...globalCmds];
    } catch (e) {
      this.customCommands = [];
    }
  }

  private getBuiltInCommands(): PaletteCommand[] {
    return [
      { name: CMD.NEW_TAB.name, desc: CMD.NEW_TAB.desc, category: CMD.NEW_TAB.category, shortcutDisplay: CMD.NEW_TAB.shortcut, action: () => this.createTab() },
      { name: CMD.CLOSE_TAB.name, desc: CMD.CLOSE_TAB.desc, category: CMD.CLOSE_TAB.category, shortcutDisplay: CMD.CLOSE_TAB.shortcut, action: () => this.closeTab(this.activeTabIndex) },
      { name: CMD.RENAME_TAB.name, desc: CMD.RENAME_TAB.desc, category: CMD.RENAME_TAB.category, action: () => { this.palette.hide(); this.renamingTabIndex = this.activeTabIndex; this.renderTabBar(); } },
      { name: CMD.SPLIT_VERTICAL.name, desc: CMD.SPLIT_VERTICAL.desc, category: CMD.SPLIT_VERTICAL.category, shortcutDisplay: CMD.SPLIT_VERTICAL.shortcut, action: () => this.splitPane('vertical') },
      { name: CMD.SPLIT_HORIZONTAL.name, desc: CMD.SPLIT_HORIZONTAL.desc, category: CMD.SPLIT_HORIZONTAL.category, shortcutDisplay: CMD.SPLIT_HORIZONTAL.shortcut, action: () => this.splitPane('horizontal') },
      { name: CMD.CLOSE_PANE.name, desc: CMD.CLOSE_PANE.desc, category: CMD.CLOSE_PANE.category, shortcutDisplay: CMD.CLOSE_PANE.shortcut, action: () => this.closeActivePane() },
      { name: CMD.NEXT_PANE.name, desc: CMD.NEXT_PANE.desc, category: CMD.NEXT_PANE.category, shortcutDisplay: CMD.NEXT_PANE.shortcut, action: () => this.navigateSpatial('right') },
      { name: CMD.PREV_PANE.name, desc: CMD.PREV_PANE.desc, category: CMD.PREV_PANE.category, shortcutDisplay: CMD.PREV_PANE.shortcut, action: () => this.navigateSpatial('left') },
      { name: CMD.NAV_PREV_COMMAND.name, desc: CMD.NAV_PREV_COMMAND.desc, category: CMD.NAV_PREV_COMMAND.category, shortcutDisplay: CMD.NAV_PREV_COMMAND.shortcut, action: () => { this.panes[this.activeIndex]?.pane.shellIntegration.navigateToBlock('prev'); } },
      { name: CMD.NAV_NEXT_COMMAND.name, desc: CMD.NAV_NEXT_COMMAND.desc, category: CMD.NAV_NEXT_COMMAND.category, shortcutDisplay: CMD.NAV_NEXT_COMMAND.shortcut, action: () => { this.panes[this.activeIndex]?.pane.shellIntegration.navigateToBlock('next'); } },
      { name: CMD.SEARCH_HISTORY.name, desc: CMD.SEARCH_HISTORY.desc, category: CMD.SEARCH_HISTORY.category, shortcutDisplay: CMD.SEARCH_HISTORY.shortcut, action: () => { this.closePaletteIfOpen(); this.historyModal.show(); } },
      { name: CMD.AI_COMMAND.name, desc: CMD.AI_COMMAND.desc, category: CMD.AI_COMMAND.category, shortcutDisplay: CMD.AI_COMMAND.shortcut, action: () => { this.closePaletteIfOpen(); this.askAI.show(); } },
      ...(this.modelUpdateAvailable ? [{ name: CMD.UPDATE_MODEL.name, desc: CMD.UPDATE_MODEL.desc, category: CMD.UPDATE_MODEL.category, action: () => { this.closePaletteIfOpen(); this.handleModelDownload(); } }] : []),
      { name: CMD.SESSION_STATUS.name, desc: CMD.SESSION_STATUS.desc, category: CMD.SESSION_STATUS.category, shortcutDisplay: CMD.SESSION_STATUS.shortcut, action: () => { this.closePaletteIfOpen(); this.statusModal.show(); } },
      { name: CMD.COMMAND_PALETTE.name, desc: CMD.COMMAND_PALETTE.desc, category: CMD.COMMAND_PALETTE.category, shortcutDisplay: CMD.COMMAND_PALETTE.shortcut, action: () => this.palette.show() },
      { name: CMD.CLEAR_TERMINAL.name, desc: CMD.CLEAR_TERMINAL.desc, category: CMD.CLEAR_TERMINAL.category, shortcutDisplay: CMD.CLEAR_TERMINAL.shortcut, action: () => this.clearActiveTerminal() },
      { name: CMD.COPY_LAST_OUTPUT.name, desc: CMD.COPY_LAST_OUTPUT.desc, category: CMD.COPY_LAST_OUTPUT.category, action: () => { const output = this.panes[this.activeIndex]?.pane.shellIntegration.getLastCommandOutput(); if (output) navigator.clipboard.writeText(output); this.closePaletteIfOpen(); } },
      { name: CMD.CREATE_COMMAND.name, desc: CMD.CREATE_COMMAND.desc, category: CMD.CREATE_COMMAND.category, shortcutDisplay: CMD.CREATE_COMMAND.shortcut, action: () => { this.palette.hide(); const input = this.panes[this.activeIndex]?.pane?.getCurrentInput() || ''; this.wizard.show(input); } },
      ...this.themes.map(t => ({
        name: `Theme: ${t.name}`, desc: `Switch to ${t.name} theme`, category: 'Appearance',
        isTheme: true,
        themeData: {
          name: t.name,
          background: t.background, foreground: t.foreground,
          accent: t.accent, accentDim: t.accentDim,
          border: t.border, borderActive: t.borderActive,
          statusBg: t.statusBg, statusFg: t.statusFg,
          cursorColor: t.cursorColor, selectionBg: t.selectionBg,
          black: t.xterm.black, red: t.xterm.red, green: t.xterm.green, yellow: t.xterm.yellow,
          blue: t.xterm.blue, magenta: t.xterm.magenta, cyan: t.xterm.cyan, white: t.xterm.white,
          brightBlack: t.xterm.brightBlack, brightRed: t.xterm.brightRed,
          brightGreen: t.xterm.brightGreen, brightYellow: t.xterm.brightYellow,
          brightBlue: t.xterm.brightBlue, brightMagenta: t.xterm.brightMagenta,
          brightCyan: t.xterm.brightCyan, brightWhite: t.xterm.brightWhite,
        },
        action: () => this.setTheme(t.name),
      })),
      { name: 'Create Theme', desc: 'Design a new color theme', category: 'Appearance', action: () => { this.closePaletteIfOpen(); this.themeWizard.show(); } },
    ];
  }

  private editTheme(cmd: PaletteCommand): void {
    this.palette.hide();
    if (cmd.themeData) {
      this.themeWizard.showForEdit(cmd.themeData.name, cmd.themeData);
    }
  }

  private async deleteTheme(cmd: PaletteCommand): Promise<void> {
    if (!cmd.themeData) return;
    const themeName = cmd.themeData.name;
    try {
      await window.go.main.App.DeleteTheme(themeName);
      const themeDTOs = await window.go.main.App.GetThemes();
      this.themes = themeDTOs.map(themeFromDTO);
      // If we deleted the active theme, switch to the first one
      if (this.currentTheme.name === themeName && this.themes.length > 0) {
        this.currentTheme = this.themes[0];
        applyThemeToCSS(this.currentTheme);
        for (const tab of this.tabs) {
          for (const p of tab.panes) p.pane.setTheme(this.currentTheme.xterm);
        }
      }
    } catch (e) {
      console.error('Failed to delete theme:', e);
    }
  }

  private editCustomCommand(cmd: PaletteCommand): void {
    this.palette.hide();
    this.wizard.showForEdit({
      name: cmd.name,
      command: cmd.command,
      desc: cmd.desc,
      scope: cmd.scope,
      shortcutKey: cmd.shortcutKey,
    });
  }

  // --- Helpers for callbacks ---

  private async getActivePaneCWD(): Promise<string> {
    const ap = this.panes[this.activeIndex];
    if (!ap) return '';
    return await ap.pane.getCWD();
  }

  private focusActivePane(): void {
    if (this.panes[this.activeIndex]) this.panes[this.activeIndex].pane.focus();
  }

  private closePaletteIfOpen(): void {
    if (this.palette.isOpen()) this.palette.hide();
  }

  private clearActiveTerminal(): void {
    const ap = this.panes[this.activeIndex];
    if (ap) window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64('\x0c'));
  }

  // --- AI Command (Cmd+K) ---

  private setAILoading(loading: boolean): void {
    this.aiGenerating = loading;
    this.renderStatusBar();
    const ap = this.panes[this.activeIndex];
    if (ap) {
      ap.element.classList.toggle('ai-loading', loading);
    }
  }

  private async handleAskAI(): Promise<void> {
    if (this.aiGenerating) return;
    const ap = this.panes[this.activeIndex];
    if (!ap?.pane.sessionId) return;

    // Read what the user typed on the current line
    const query = ap.pane.getCurrentInput();
    if (!query.trim()) return;

    // Check model, trigger download dialog if needed
    const ready = await window.go.main.App.IsModelReady();
    if (!ready) {
      await this.handleModelDownload();
      // Recheck after dialog closes
      if (!(await window.go.main.App.IsModelReady())) return;
    }

    // Show generating state — rotating border + status bar
    this.setAILoading(true);

    // Clear current line (Ctrl+U clears everything before cursor in most shells)
    window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64('\x15'));

    try {
      const cwd = await ap.pane.getCWD();
      const command = await window.go.main.App.AskAI(query, cwd);

      // Write the generated command WITHOUT executing (no newline)
      if (command) {
        window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64(command));
      }
    } catch (err) {
      // Restore the original query on failure
      window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64(query));
      console.error('AI generation failed:', err);
    }

    this.setAILoading(false);
  }

  private async checkModelUpdate(): Promise<void> {
    try {
      this.modelUpdateAvailable = await window.go.main.App.CheckModelUpdate();
    } catch { /* best-effort */ }
  }

  private async handleModelDownload(): Promise<void> {
    // Show dialog (same style as the app update dialog)
    const overlay = document.createElement('div');
    overlay.className = 'update-overlay';
    overlay.innerHTML = `<div class="update-dialog">
      <div class="update-dialog-title">${this.modelUpdateAvailable ? 'Update AI Model' : 'Download AI Model'}</div>
      <div class="update-dialog-body">
        <div id="model-dl-status">Connecting...</div>
        <div class="model-dl-bar-track"><div class="model-dl-bar-fill" id="model-dl-bar"></div></div>
      </div>
      <div class="update-dialog-actions">
        <button class="theme-btn theme-btn-cancel" id="model-dl-cancel">Cancel</button>
      </div>
    </div>`;
    document.body.appendChild(overlay);

    const statusEl = document.getElementById('model-dl-status')!;
    const barEl = document.getElementById('model-dl-bar')!;

    // Cancel button + Escape
    let cancelled = false;
    const cancel = () => {
      cancelled = true;
      window.go.main.App.SkipDownload();
    };
    document.getElementById('model-dl-cancel')?.addEventListener('click', cancel);
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') cancel();
      e.stopPropagation();
    };
    document.addEventListener('keydown', onKey, true);

    const unsub = window.runtime.EventsOn('model:download-progress', (data: { downloaded: number; total: number }) => {
      if (data.total > 0) {
        const pct = Math.round((data.downloaded / data.total) * 100);
        const mbDown = (data.downloaded / 1024 / 1024).toFixed(1);
        const mbTotal = (data.total / 1024 / 1024).toFixed(1);
        statusEl.textContent = `Downloading... ${mbDown} / ${mbTotal} MB (${pct}%)`;
        barEl.style.width = `${pct}%`;
      }
    });

    try {
      await window.go.main.App.DownloadModel();
      unsub();

      if (!cancelled) {
        statusEl.textContent = 'Loading model...';
        barEl.style.width = '100%';
        await window.go.main.App.InitLLM();
        this.modelUpdateAvailable = false;
        statusEl.textContent = 'AI model ready!';
        setTimeout(() => {
          overlay.remove();
          this.focusActivePane();
        }, 800);
      } else {
        overlay.remove();
        this.focusActivePane();
      }
    } catch {
      unsub();
      if (cancelled) {
        overlay.remove();
      } else {
        statusEl.textContent = 'Download failed. Check your network.';
        barEl.style.width = '0%';
        const cancelBtn = document.getElementById('model-dl-cancel');
        if (cancelBtn) cancelBtn.textContent = 'Close';
      }
      this.focusActivePane();
    }

    document.removeEventListener('keydown', onKey, true);
  }

  // --- Keyboard ---

  private handleKeydown(e: KeyboardEvent): void {
    const isMeta = e.metaKey;

    // Smart render panel (check all panes)
    for (const tab of this.tabs) {
      for (const p of tab.panes) {
        if (p.pane.smartRender.isPanelOpen()) {
          if (p.pane.smartRender.handleKeydown(e)) return;
        }
      }
    }

    // History modal
    if (this.historyModal.isOpen()) {
      e.stopPropagation();
      this.historyModal.handleKeydown(e);
      return;
    }

    // Status modal takes highest priority after wizards
    if (this.statusModal.isOpen()) {
      e.stopPropagation();
      this.statusModal.handleKeydown(e);
      return;
    }

    // Theme wizard takes highest priority
    if (this.themeWizard.isOpen()) {
      this.themeWizard.handleKeydown(e);
      return;
    }

    // Wizard takes priority
    if (this.wizard.isOpen()) {
      e.stopPropagation();
      this.wizard.handleKeydown(e);
      return;
    }

    // AI modal takes priority
    if (this.askAI.isOpen()) {
      e.stopPropagation();
      this.askAI.handleKeydown(e);
      return;
    }

    // Palette takes second priority
    if (this.palette.isOpen()) {
      this.palette.handleKeydown(e);
      return;
    }

    // Custom command shortcuts (checked first — they use Ctrl/Alt combos
    // that don't overlap with built-in Cmd shortcuts)
    if (this.customCommands.length > 0) {
      const parts: string[] = [];
      if (e.metaKey) parts.push('Cmd');
      if (e.ctrlKey) parts.push('Ctrl');
      if (e.shiftKey) parts.push('Shift');
      if (e.altKey) parts.push('Alt');
      const keyName = e.key.length === 1 ? e.key.toUpperCase() : e.key;
      if (!['Control', 'Meta', 'Shift', 'Alt'].includes(keyName)) {
        parts.push(keyName);
        const pressed = parts.join('+');
        for (const c of this.customCommands) {
          if (c.shortcut && c.shortcut.toLowerCase() === pressed.toLowerCase()) {
            e.preventDefault(); e.stopImmediatePropagation();
            const ap = this.panes[this.activeIndex];
            if (ap) {
              const data = c.command.includes('\n')
                ? '\x1b[200~' + c.command + '\x1b[201~\n'
                : c.command + '\n';
              window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64(data));
            }
            return;
          }
        }
      }
    }

    // Block Ctrl+L so it doesn't clear via the shell — only Cmd+L should clear
    if (e.ctrlKey && !e.metaKey && e.key.toLowerCase() === 'l') {
      e.preventDefault();
      return;
    }

    // Built-in shortcuts (Cmd-based)
    if (isMeta) {
      if (e.shiftKey && e.key.toLowerCase() === 'c') {
        e.preventDefault();
        const input = this.panes[this.activeIndex]?.pane?.getCurrentInput() || '';
        this.wizard.show(input);
        return;
      }
      if (e.shiftKey && e.key.toLowerCase() === 'r') {
        e.preventDefault();
        this.historyModal.show();
        return;
      }
      if (e.shiftKey && e.key === '\\') {
        e.preventDefault();
        this.splitPane('vertical');
        return;
      }
      if (e.shiftKey && e.key === 'ArrowUp') {
        e.preventDefault();
        this.panes[this.activeIndex]?.pane.shellIntegration.navigateToBlock('prev');
        return;
      }
      if (e.shiftKey && e.key === 'ArrowDown') {
        e.preventDefault();
        this.panes[this.activeIndex]?.pane.shellIntegration.navigateToBlock('next');
        return;
      }

      switch (e.key.toLowerCase()) {
        case 'k': e.preventDefault(); this.handleAskAI(); return;
        case 'i': e.preventDefault(); this.statusModal.show(); return;
        case 'p': e.preventDefault(); this.palette.show(); return;
        case 't': e.preventDefault(); this.createTab(); return;
        case 'w': e.preventDefault(); this.closeTab(this.activeTabIndex); return;
        case '|': e.preventDefault(); this.splitPane('vertical'); return;
        case '-': e.preventDefault(); this.splitPane('horizontal'); return;
        case 'x': e.preventDefault(); this.closeActivePane(); return;
        case 'l': e.preventDefault(); this.clearActiveTerminal(); return;
        case '1': case '2': case '3': case '4': case '5': case '6': case '7': case '8': case '9':
          e.preventDefault();
          const tabIdx = parseInt(e.key) - 1;
          if (tabIdx < this.tabs.length) this.switchToTab(tabIdx);
          return;
        case 'arrowright': case 'arrowleft': case 'arrowup': case 'arrowdown':
          e.preventDefault();
          this.navigateSpatial(e.key.toLowerCase().replace('arrow', '') as 'left' | 'right' | 'up' | 'down');
          return;
      }
    }
  }

  private navigateSpatial(direction: 'left' | 'right' | 'up' | 'down'): void {
    if (this.panes.length <= 1) return;
    const current = this.panes[this.activeIndex].element.getBoundingClientRect();
    const cx = current.left + current.width / 2, cy = current.top + current.height / 2;
    let bestIndex = -1, bestDist = Infinity;
    for (let i = 0; i < this.panes.length; i++) {
      if (i === this.activeIndex) continue;
      const rect = this.panes[i].element.getBoundingClientRect();
      const px = rect.left + rect.width / 2, py = rect.top + rect.height / 2;
      let valid = false;
      switch (direction) {
        case 'left': valid = px < cx - SPATIAL_NAV_THRESHOLD; break;
        case 'right': valid = px > cx + SPATIAL_NAV_THRESHOLD; break;
        case 'up': valid = py < cy - SPATIAL_NAV_THRESHOLD; break;
        case 'down': valid = py > cy + SPATIAL_NAV_THRESHOLD; break;
      }
      if (valid) {
        const dist = Math.abs(px - cx) + Math.abs(py - cy);
        if (dist < bestDist) { bestDist = dist; bestIndex = i; }
      }
    }
    if (bestIndex >= 0) this.setActive(bestIndex);
  }
}

document.addEventListener('DOMContentLoaded', async () => { const app = new ElTerminalo(); await app.init(); });
