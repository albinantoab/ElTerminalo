import { TerminalPane } from './terminal/TerminalPane';
import { AppTheme, themeFromDTO, applyThemeToCSS } from './theme/themes';
import { PaneInfo, SplitNode, Tab, SavedSplitNode, SavedState, CustomCommand, PaletteCommand } from './types';
import { CommandPalette } from './palette/CommandPalette';
import { CommandWizard } from './wizard/CommandWizard';
import { ThemeWizard } from './wizard/ThemeWizard';
import { StateManager } from './state/StateManager';
import { escHtml, generateId, waitForLayout, utf8ToBase64, bytesToBase64 } from './utils';
import {
  MAX_TABS, DOUBLE_CLICK_DELAY_MS, MIN_SPLIT_RATIO, MAX_SPLIT_RATIO,
  DEFAULT_SPLIT_RATIO, SPATIAL_NAV_THRESHOLD, STATE_SAVE_INTERVAL_MS,
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

    this.stateManager = new StateManager({
      getTabs: () => this.tabs,
      getActiveTabIndex: () => this.activeTabIndex,
      getCurrentThemeName: () => this.currentTheme.name,
    });

    const themeDTOs = await window.go.main.App.GetThemes();
    this.themes = themeDTOs.map(themeFromDTO);
    this.currentTheme = this.themes[0];

    await this.refreshCustomCommands();

    const restored = await this.restoreState();
    if (!restored) {
      applyThemeToCSS(this.currentTheme);
      await this.createTab('Terminalo 1');
    }

    this.switchToTab(this.activeTabIndex);
    window.addEventListener('keydown', (e: KeyboardEvent) => this.handleKeydown(e), true);
    this.renderStatusBar();
    setInterval(() => this.stateManager.save(), STATE_SAVE_INTERVAL_MS);

    // Dismiss splash screen
    this.dismissSplash();

    // Check for updates in background (non-blocking), then every 6 hours
    this.checkForUpdate();
    setInterval(() => this.checkForUpdate(), 6 * 60 * 60 * 1000);

    // Handle file drops — read via HTML5 API, save to temp via Go
    document.addEventListener('dragover', (e) => e.preventDefault(), true);
    document.addEventListener('drop', async (e) => {
      e.preventDefault();
      const ap = this.panes[this.activeIndex];
      if (!ap?.pane.sessionId || !e.dataTransfer?.files?.length) return;
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
      <div class="tab-new" title="New Tab (Cmd+T)">+</div>
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
      ? `<a class="status-update" id="status-update-link">Update v${this.updateInfo.latestVersion} available</a><span class="status-sep">·</span>`
      : '';

    this.statusbar.innerHTML = `
      <div class="status-left">${updateBadge}</div>
      <div class="status-right">
        <span class="status-key">Cmd + P</span><span class="status-label">commands</span>
        <span class="status-sep">·</span>
        <span class="status-key">Cmd + T</span><span class="status-label">new tab</span>
        <span class="status-sep">·</span>
        <span class="status-key">Cmd + W</span><span class="status-label">close tab</span>
        <span class="status-sep">·</span>
        <span class="status-key">Cmd + B</span><span class="status-label">vsplit</span>
        <span class="status-sep">·</span>
        <span class="status-key">Cmd + G</span><span class="status-label">hsplit</span>
        <span class="status-sep">·</span>
        <span class="status-key">Cmd + X</span><span class="status-label">close pane</span>
        <span class="status-sep">·</span>
        <span class="status-key">Cmd + L</span><span class="status-label">clear</span>
      </div>
    `;

    if (this.updateInfo?.available) {
      document.getElementById('status-update-link')?.addEventListener('click', () => {
        this.promptUpdate();
      });
    }
  }

  private async promptUpdate(): Promise<void> {
    if (!this.updateInfo?.available) return;

    // Show confirmation overlay
    const overlay = document.createElement('div');
    overlay.className = 'update-overlay';
    overlay.innerHTML = `<div class="update-dialog">
      <div class="update-dialog-title">Update Available</div>
      <div class="update-dialog-body">
        v${this.updateInfo.latestVersion} is ready to install.<br>
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
      { name: 'New Tab', desc: 'Open a new terminal tab', category: 'Tabs', shortcutDisplay: 'Cmd+T', action: () => this.createTab() },
      { name: 'Close Tab', desc: 'Close current tab', category: 'Tabs', shortcutDisplay: 'Cmd+W', action: () => this.closeTab(this.activeTabIndex) },
      { name: 'Rename Tab', desc: 'Rename current tab', category: 'Tabs', action: () => { this.palette.hide(); this.renamingTabIndex = this.activeTabIndex; this.renderTabBar(); } },
      { name: 'Split Vertical', desc: 'Split pane side by side', category: 'Panes', shortcutDisplay: 'Cmd+B', action: () => this.splitPane('vertical') },
      { name: 'Split Horizontal', desc: 'Split pane top/bottom', category: 'Panes', shortcutDisplay: 'Cmd+G', action: () => this.splitPane('horizontal') },
      { name: 'Close Pane', desc: 'Close the active pane', category: 'Panes', shortcutDisplay: 'Cmd+X', action: () => this.closeActivePane() },
      { name: 'Next Pane', desc: 'Focus the next pane', category: 'Panes', shortcutDisplay: 'Cmd+→', action: () => this.navigateSpatial('right') },
      { name: 'Previous Pane', desc: 'Focus the previous pane', category: 'Panes', shortcutDisplay: 'Cmd+←', action: () => this.navigateSpatial('left') },
      { name: 'Command Palette', desc: 'Open command palette', category: 'General', shortcutDisplay: 'Cmd+P', action: () => this.palette.show() },
      { name: 'Clear Terminal', desc: 'Clear the active terminal', category: 'General', shortcutDisplay: 'Cmd+L', action: () => this.clearActiveTerminal() },
      { name: 'Create Command', desc: 'Create a custom command', category: 'Commands', shortcutDisplay: 'Cmd+Shift+C', action: () => { this.palette.hide(); this.wizard.show(); } },
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
    if (ap) window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64('clear\n'));
  }

  // --- Keyboard ---

  private handleKeydown(e: KeyboardEvent): void {
    const isMeta = e.metaKey || e.ctrlKey;

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
            if (ap) window.go.main.App.WriteToSession(ap.pane.sessionId, utf8ToBase64(c.command + '\n'));
            return;
          }
        }
      }
    }

    // Built-in shortcuts (Cmd-based)
    if (isMeta) {
      if (e.shiftKey && e.key.toLowerCase() === 'c') { e.preventDefault(); this.wizard.show(); return; }

      switch (e.key.toLowerCase()) {
        case 'p': e.preventDefault(); this.palette.show(); return;
        case 't': e.preventDefault(); this.createTab(); return;
        case 'w': e.preventDefault(); this.closeTab(this.activeTabIndex); return;
        case 'b': e.preventDefault(); this.splitPane('vertical'); return;
        case 'g': e.preventDefault(); this.splitPane('horizontal'); return;
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
