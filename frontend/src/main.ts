import { TerminalPane } from './terminal/TerminalPane';
import { AppTheme, themeFromDTO, applyThemeToCSS } from './theme/themes';

declare const window: any;

interface PaneInfo {
  id: string;
  pane: TerminalPane;
  element: HTMLElement;
}

interface SplitNode {
  type: 'leaf' | 'split';
  direction?: 'vertical' | 'horizontal';
  ratio?: number;
  paneInfo?: PaneInfo;
  children?: [SplitNode, SplitNode];
}

interface Tab {
  id: string;
  name: string;
  panes: PaneInfo[];
  activeIndex: number;
  layoutRoot: SplitNode | null;
}

interface SavedSplitNode {
  type: 'leaf' | 'split';
  direction?: 'vertical' | 'horizontal';
  ratio?: number;
  cwd?: string;
  children?: [SavedSplitNode, SavedSplitNode];
}

interface SavedTab {
  name: string;
  layout: SavedSplitNode;
}

interface SavedState {
  version: number;
  themeName: string;
  activeTabIndex: number;
  tabs: SavedTab[];
}

class ElTerminalo {
  private tabs: Tab[] = [];
  private activeTabIndex: number = 0;
  private themes: AppTheme[] = [];
  private currentTheme!: AppTheme;
  private paletteOpen = false;
  private paletteQuery = '';
  private paletteCursor = 0;
  private customCommands: { name: string; command: string; description: string; scope: string; shortcut: string }[] = [];

  private wizardOpen = false;
  private wizardStep = 0;
  private wizardScopeCursor = 0;
  private wizardData = { scope: '', command: '', name: '', description: '', shortcut: '' };
  private wizardShortcutConflict = '';
  private wizardCapturingShortcut = false;
  private renamingTabIndex = -1;

  private container!: HTMLElement;
  private tabBar!: HTMLElement;
  private statusbar!: HTMLElement;
  private paletteOverlay!: HTMLElement;
  private wizardOverlay!: HTMLElement;

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
    this.paletteOverlay = document.getElementById('palette-overlay')!;
    this.wizardOverlay = document.getElementById('wizard-overlay')!;

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
    setInterval(() => this.saveState(), 30000);
  }

  // --- Tab Management ---

  private async createTab(name?: string): Promise<void> {
    if (this.tabs.length >= 9) return;
    const tabName = name || `Terminalo ${this.tabs.length + 1}`;
    const tab: Tab = {
      id: `tab-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
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
    await this.waitForLayout();
    pane.pane.fit();
    await pane.pane.connect();
    this.setActive(0);
    this.saveState();
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
    this.saveState();
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
      this.saveState();
    }
  }

  private renderTabBar(): void {
    const tabs = this.tabs.map((t, i) => {
      const isActive = i === this.activeTabIndex;
      if (this.renamingTabIndex === i) {
        return `<div class="tab-item active">
          <input class="tab-rename-input" type="text" value="${this.escHtml(t.name)}" data-index="${i}" />
        </div>`;
      }
      return `<div class="tab-item ${isActive ? 'active' : ''}" data-index="${i}">
        <span class="tab-shortcut">${i + 1}</span>
        <span class="tab-name">${this.escHtml(t.name)}</span>
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
        }, 250);
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

  private async saveState(): Promise<void> {
    if (this.tabs.length === 0) return;
    try {
      const savedTabs: SavedTab[] = [];
      for (const tab of this.tabs) {
        const layout = tab.layoutRoot ? await this.serializeLayout(tab.layoutRoot) : { type: 'leaf' as const };
        savedTabs.push({ name: tab.name, layout });
      }
      const state: SavedState = {
        version: 2,
        themeName: this.currentTheme.name,
        activeTabIndex: this.activeTabIndex,
        tabs: savedTabs,
      };
      await window.go.main.App.SaveAppState(JSON.stringify(state));
    } catch (e) {
      console.error('Failed to save state:', e);
    }
  }

  private async serializeLayout(node: SplitNode): Promise<SavedSplitNode> {
    if (node.type === 'leaf' && node.paneInfo) {
      const cwd = await node.paneInfo.pane.getCWD();
      return { type: 'leaf', cwd };
    }
    if (node.type === 'split' && node.children) {
      const [a, b] = await Promise.all([
        this.serializeLayout(node.children[0]),
        this.serializeLayout(node.children[1]),
      ]);
      return { type: 'split', direction: node.direction, ratio: node.ratio, children: [a, b] };
    }
    return { type: 'leaf' };
  }

  private async restoreState(): Promise<boolean> {
    try {
      const json = await window.go.main.App.LoadAppState();
      if (!json) return false;

      const state: SavedState = JSON.parse(json);
      if (!state.tabs && (state as any).layout) {
        // v1 migration: single layout → single tab
        applyThemeToCSS(this.currentTheme);
        const tab: Tab = { id: 'tab-migrated', name: 'Terminal', panes: [], activeIndex: 0, layoutRoot: null };
        this.tabs.push(tab);
        this.activeTabIndex = 0;
        tab.layoutRoot = await this.restoreLayoutNode((state as any).layout, tab);
        this.renderTabBar();
        this.renderLayout();
        await this.waitForLayout();
        for (const p of tab.panes) p.pane.fit();
        return true;
      }

      if (!state.tabs || state.tabs.length === 0) return false;

      const theme = this.themes.find(t => t.name === state.themeName);
      if (theme) this.currentTheme = theme;
      applyThemeToCSS(this.currentTheme);

      for (const savedTab of state.tabs) {
        const tab: Tab = {
          id: `tab-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
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
      await this.waitForLayout();
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
      return { type: 'split', direction: saved.direction, ratio: saved.ratio ?? 0.5, children: [first, second] };
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
    const id = `pane-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
    const info: PaneInfo = { id, pane, element: el };

    el.addEventListener('mousedown', () => {
      const idx = tab.panes.indexOf(info);
      if (idx >= 0 && tab === this.tab) this.setActive(idx);
    });

    tab.panes.push(info);
    return info;
  }

  // Alias for current tab
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
        return { type: 'split', direction, ratio: 0.5, children: [{ type: 'leaf', paneInfo: activePane }, { type: 'leaf', paneInfo: newPane }] };
      }
      if (node.type === 'split' && node.children) {
        return { ...node, children: [replaceInTree(node.children[0]), replaceInTree(node.children[1])] };
      }
      return node;
    };

    this.layoutRoot = replaceInTree(this.layoutRoot);
    this.renderLayout();
    await this.waitForLayout();
    this.fitAll();
    await newPane.pane.connect();
    this.setActive(this.panes.indexOf(newPane));
    this.saveState();
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
    this.saveState();
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
      const ratio = node.ratio ?? 0.5;
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
          const newRatio = Math.max(0.1, Math.min(0.9, startRatio + delta));
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
          this.saveState();
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

  private async waitForLayout(): Promise<void> {
    await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
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

  private setTheme(name: string): void {
    const theme = this.themes.find(t => t.name.toLowerCase() === name.toLowerCase());
    if (!theme) return;
    this.currentTheme = theme;
    applyThemeToCSS(theme);
    for (const tab of this.tabs) {
      for (const p of tab.panes) p.pane.setTheme(theme.xterm);
    }
    this.renderStatusBar();
    this.saveState();
  }

  private renderStatusBar(): void {
    this.statusbar.innerHTML = `
      <div class="status-left"></div>
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
  }

  // --- Custom Commands ---

  private async refreshCustomCommands(): Promise<void> {
    try {
      const globalCmds = await window.go.main.App.GetGlobalCommands() || [];
      let localCmds: any[] = [];
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

  // --- Command Palette ---

  private getCommands(): { name: string; desc: string; category: string; isCustom?: boolean; scope?: string; command?: string; shortcutDisplay?: string; shortcutKey?: string; action: (metaKey?: boolean) => void }[] {
    const builtIn = [
      { name: 'New Tab', desc: 'Open a new terminal tab', category: 'Tabs', action: () => this.createTab() },
      { name: 'Close Tab', desc: 'Close current tab', category: 'Tabs', action: () => this.closeTab(this.activeTabIndex) },
      { name: 'Rename Tab', desc: 'Rename current tab', category: 'Tabs', action: () => { this.closePalette(); this.renamingTabIndex = this.activeTabIndex; this.renderTabBar(); } },
      { name: 'Split Vertical', desc: 'Split pane side by side', category: 'Panes', action: () => this.splitPane('vertical') },
      { name: 'Split Horizontal', desc: 'Split pane top/bottom', category: 'Panes', action: () => this.splitPane('horizontal') },
      { name: 'Close Pane', desc: 'Close the active pane', category: 'Panes', action: () => this.closeActivePane() },
      { name: 'Next Pane', desc: 'Focus the next pane', category: 'Panes', action: () => this.navigateSpatial('right') },
      { name: 'Previous Pane', desc: 'Focus the previous pane', category: 'Panes', action: () => this.navigateSpatial('left') },
      { name: 'Create Command', desc: 'Cmd + Shift + C', category: 'Commands', action: () => { this.closePalette(); this.openWizard(); } },
      ...this.themes.map(t => ({ name: `Theme: ${t.name}`, desc: `Switch to ${t.name} theme`, category: 'Appearance', action: () => this.setTheme(t.name) })),
    ];

    const custom = this.customCommands.map(c => ({
      name: c.name, desc: c.description || c.command, category: c.scope === 'local' ? 'Project' : 'Global',
      shortcutDisplay: c.shortcut || '', isCustom: true, scope: c.scope, command: c.command, shortcutKey: c.shortcut || '',
      action: (metaKey?: boolean) => {
        const ap = this.panes[this.activeIndex];
        if (!ap) return;
        window.go.main.App.WriteToSession(ap.pane.sessionId, btoa(c.command + (metaKey ? '' : '\n')));
      },
    }));

    return [...custom, ...builtIn];
  }

  private getFilteredCommands() {
    const q = this.paletteQuery.toLowerCase();
    if (!q) return this.getCommands();
    return this.getCommands().filter(c => c.name.toLowerCase().includes(q) || c.desc.toLowerCase().includes(q) || c.category.toLowerCase().includes(q));
  }

  private async openPalette(): Promise<void> {
    this.paletteOpen = true;
    this.paletteQuery = '';
    this.paletteCursor = 0;
    await this.refreshCustomCommands();
    this.renderPalette();
    this.paletteOverlay.classList.remove('hidden');
    requestAnimationFrame(() => { (this.paletteOverlay.querySelector('.palette-input') as HTMLInputElement)?.focus(); });
  }

  private closePalette(): void {
    this.paletteOpen = false;
    this.paletteOverlay.classList.add('hidden');
    if (this.panes[this.activeIndex]) this.panes[this.activeIndex].pane.focus();
  }

  private renderPalette(): void {
    const commands = this.getFilteredCommands();
    let lastCategory = '';
    const items = commands.map((c, i) => {
      const shortcutBadge = c.shortcutDisplay ? `<kbd class="palette-item-shortcut">${c.shortcutDisplay}</kbd>` : '';
      let groupHeader = '';
      if (c.category !== lastCategory) { lastCategory = c.category; groupHeader = `<div class="palette-group-header">${c.category}</div>`; }
      return `${groupHeader}<div class="palette-item ${i === this.paletteCursor ? 'selected' : ''}" data-index="${i}"><div><span class="palette-item-name">${c.name}</span><span class="palette-item-desc">${c.desc}</span></div>${shortcutBadge}</div>`;
    }).join('');

    this.paletteOverlay.innerHTML = `<div class="palette-box"><input class="palette-input" type="text" placeholder="Type a command..." value="${this.paletteQuery}" /><div class="palette-list">${items || '<div class="palette-item"><span class="palette-item-desc">No matching commands</span></div>'}</div><div class="palette-hint"><kbd>Enter</kbd> execute · <kbd>Cmd + Enter</kbd> fill · <kbd>Cmd + E</kbd> edit · <kbd>Cmd + D</kbd> delete · <kbd>Esc</kbd> close</div></div>`;

    const input = this.paletteOverlay.querySelector('.palette-input') as HTMLInputElement;
    input.addEventListener('input', (e) => {
      this.paletteQuery = (e.target as HTMLInputElement).value;
      this.paletteCursor = 0;
      this.renderPalette();
      const ni = this.paletteOverlay.querySelector('.palette-input') as HTMLInputElement;
      ni?.focus();
      ni.selectionStart = ni.selectionEnd = ni.value.length;
    });
    this.paletteOverlay.querySelector('.palette-item.selected')?.scrollIntoView({ block: 'nearest' });
    this.paletteOverlay.querySelectorAll('.palette-item[data-index]').forEach(el => {
      el.addEventListener('click', () => { const cmd = commands[parseInt(el.getAttribute('data-index') || '0')]; this.closePalette(); cmd?.action(false); });
    });
  }

  // --- Edit/Delete Custom Commands ---

  private async deleteCustomCommand(cmd: { name: string; scope?: string }): Promise<void> {
    const cwd = this.panes[this.activeIndex] ? await this.panes[this.activeIndex].pane.getCWD() : '';
    try { await window.go.main.App.DeleteCommand(cmd.scope || 'global', cmd.name, cwd); await this.refreshCustomCommands(); this.renderPalette(); (this.paletteOverlay.querySelector('.palette-input') as HTMLInputElement)?.focus(); } catch (e) { console.error('Failed to delete command:', e); }
  }

  private async editCustomCommand(cmd: any): Promise<void> {
    this.closePalette();
    this.wizardOpen = true;
    this.wizardStep = 1;
    this.wizardData = { scope: cmd.scope || 'global', command: cmd.command || '', name: cmd.name, description: cmd.desc !== cmd.command ? cmd.desc : '', shortcut: cmd.shortcutKey || '' };
    this.wizardShortcutConflict = '';
    this.wizardCapturingShortcut = false;
    (this as any)._editingOriginalName = cmd.name;
    (this as any)._editingScope = cmd.scope;
    this.renderWizard();
    this.wizardOverlay.classList.remove('hidden');
  }

  // --- Create Command Wizard ---

  private openWizard(): void {
    this.wizardOpen = true;
    this.wizardStep = 0;
    this.wizardData = { scope: '', command: '', name: '', description: '', shortcut: '' };
    this.wizardShortcutConflict = '';
    this.wizardCapturingShortcut = false;
    this.renderWizard();
    this.wizardOverlay.classList.remove('hidden');
  }

  private closeWizard(): void {
    this.wizardOpen = false;
    this.wizardOverlay.classList.add('hidden');
    if (this.panes[this.activeIndex]) this.panes[this.activeIndex].pane.focus();
  }

  private renderWizard(): void {
    let content = '';
    if (this.wizardStep === 0) {
      const scopes = [{ key: 'global', label: 'Global', hint: '~/.config/elterminalo' }, { key: 'local', label: 'Project', hint: '.elterminalo/ in cwd' }];
      const btns = scopes.map((s, i) => `<div class="wizard-scope-btn ${i === this.wizardScopeCursor ? 'wizard-scope-active' : ''}" data-scope="${s.key}">${s.label}<br><span style="font-size:10px;color:var(--border)">${s.hint}</span></div>`).join('');
      content = `<div class="wizard-title">Create Command</div><div class="wizard-step-label">Where should this command be saved?</div><div class="wizard-scope-btns">${btns}</div><div class="wizard-hint">← → select · enter confirm · esc cancel</div>`;
    } else if (this.wizardStep === 1) {
      content = `<div class="wizard-title">Create Command — ${this.wizardData.scope}</div><div class="wizard-step-label">Command to execute</div><input class="wizard-input" id="wizard-field" type="text" placeholder="e.g. npm run build" value="${this.escHtml(this.wizardData.command)}" /><div class="wizard-hint">enter to continue · esc to cancel</div>`;
    } else if (this.wizardStep === 2) {
      content = `<div class="wizard-title">Create Command — ${this.wizardData.scope}</div><div class="wizard-step-label">Display name</div><input class="wizard-input" id="wizard-field" type="text" placeholder="e.g. Build Project" value="${this.escHtml(this.wizardData.name)}" /><div class="wizard-hint">enter to continue · esc to cancel</div>`;
    } else if (this.wizardStep === 3) {
      content = `<div class="wizard-title">Create Command — ${this.wizardData.scope}</div><div class="wizard-step-label">Description (optional)</div><input class="wizard-input" id="wizard-field" type="text" placeholder="e.g. Builds the project for production" value="${this.escHtml(this.wizardData.description)}" /><div class="wizard-hint">enter to continue · esc to cancel</div>`;
    } else if (this.wizardStep === 4) {
      const display = this.wizardData.shortcut || 'Press a key combo...';
      const conflict = this.wizardShortcutConflict ? `<div class="wizard-conflict">Conflicts with: ${this.escHtml(this.wizardShortcutConflict)}</div>` : '';
      content = `<div class="wizard-title">Create Command — ${this.wizardData.scope}</div><div class="wizard-step-label">Keyboard shortcut (optional)</div><div class="wizard-shortcut-capture" id="wizard-shortcut-box">${this.escHtml(display)}</div>${conflict}<div class="wizard-hint">press a key combo · backspace to clear · enter to save · esc to cancel</div>`;
    }
    this.wizardOverlay.innerHTML = `<div class="wizard-box">${content}</div>`;
    if (this.wizardStep === 0) {
      this.wizardOverlay.querySelectorAll('.wizard-scope-btn').forEach(btn => {
        btn.addEventListener('click', () => { this.wizardData.scope = btn.getAttribute('data-scope') || 'global'; this.wizardStep = 1; this.renderWizard(); });
      });
    }
    requestAnimationFrame(() => { const input = document.getElementById('wizard-field') as HTMLInputElement; if (input) { input.focus(); input.selectionStart = input.selectionEnd = input.value.length; } });
  }

  private handleWizardKey(e: KeyboardEvent): boolean {
    if (!this.wizardOpen) return false;
    if (e.key === 'Escape') { e.preventDefault(); this.closeWizard(); return true; }

    if (this.wizardStep === 0) {
      if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') { e.preventDefault(); this.wizardScopeCursor = 0; this.renderWizard(); return true; }
      if (e.key === 'ArrowRight' || e.key === 'ArrowDown') { e.preventDefault(); this.wizardScopeCursor = 1; this.renderWizard(); return true; }
      if (e.key === 'Enter') { e.preventDefault(); this.wizardData.scope = this.wizardScopeCursor === 0 ? 'global' : 'local'; this.wizardStep = 1; this.renderWizard(); return true; }
      return true;
    }

    if (this.wizardStep === 4) {
      if (e.key === 'Enter') { e.preventDefault(); if (!this.wizardShortcutConflict) this.finishWizard(); return true; }
      if (e.key === 'Backspace') { e.preventDefault(); this.wizardData.shortcut = ''; this.wizardShortcutConflict = ''; this.renderWizard(); return true; }
      if (e.key !== 'Meta' && e.key !== 'Control' && e.key !== 'Alt' && e.key !== 'Shift') {
        e.preventDefault();
        const parts: string[] = [];
        if (e.metaKey) parts.push('Cmd');
        if (e.ctrlKey) parts.push('Ctrl');
        if (e.shiftKey) parts.push('Shift');
        if (e.altKey) parts.push('Alt');
        parts.push(e.key.length === 1 ? e.key.toUpperCase() : e.key);
        const shortcut = parts.join('+');
        this.wizardData.shortcut = shortcut;
        this.wizardShortcutConflict = this.checkShortcutConflict(shortcut);
        this.renderWizard();
        return true;
      }
      return true;
    }

    if (e.key === 'Enter' && this.wizardStep >= 1) {
      e.preventDefault();
      const val = (document.getElementById('wizard-field') as HTMLInputElement)?.value.trim() || '';
      if (this.wizardStep === 1) { if (!val) return true; this.wizardData.command = val; this.wizardStep = 2; this.wizardData.name = val.split(' ').slice(0, 3).map(w => w.charAt(0).toUpperCase() + w.slice(1)).join(' '); this.renderWizard(); }
      else if (this.wizardStep === 2) { if (!val) return true; this.wizardData.name = val; this.wizardStep = 3; this.renderWizard(); }
      else if (this.wizardStep === 3) { this.wizardData.description = val; this.wizardStep = 4; this.wizardShortcutConflict = ''; this.renderWizard(); }
      return true;
    }
    return true;
  }

  private async finishWizard(): Promise<void> {
    const { scope, name, command, description, shortcut } = this.wizardData;
    const cwd = this.panes[this.activeIndex] ? await this.panes[this.activeIndex].pane.getCWD() : '';
    try {
      const editingName = (this as any)._editingOriginalName;
      if (editingName) {
        await window.go.main.App.UpdateCommand((this as any)._editingScope || scope, editingName, name, command, description, shortcut, cwd);
        (this as any)._editingOriginalName = null;
        (this as any)._editingScope = null;
      } else {
        await window.go.main.App.SaveCommand(scope, name, command, description, shortcut, cwd);
      }
    } catch (e) { console.error('Failed to save command:', e); }
    this.closeWizard();
    await this.refreshCustomCommands();
  }

  private checkShortcutConflict(shortcut: string): string {
    const s = shortcut.toLowerCase();
    const builtIns: Record<string, string> = {
      'cmd+p': 'Command Palette', 'cmd+b': 'Split Vertical', 'cmd+g': 'Split Horizontal', 'cmd+x': 'Close Pane',
      'cmd+l': 'Clear Terminal', 'cmd+shift+c': 'Create Command', 'cmd+e': 'Edit Command (in palette)', 'cmd+d': 'Delete Command (in palette)',
      'cmd+t': 'New Tab', 'cmd+w': 'Close Tab',
      'cmd+1': 'Switch to Tab 1', 'cmd+2': 'Switch to Tab 2', 'cmd+3': 'Switch to Tab 3',
      'cmd+4': 'Switch to Tab 4', 'cmd+5': 'Switch to Tab 5', 'cmd+6': 'Switch to Tab 6',
      'cmd+7': 'Switch to Tab 7', 'cmd+8': 'Switch to Tab 8', 'cmd+9': 'Switch to Tab 9',
      'cmd+arrowright': 'Next Pane', 'cmd+arrowleft': 'Previous Pane', 'cmd+arrowup': 'Pane Above', 'cmd+arrowdown': 'Pane Below',
    };
    const systemShortcuts: Record<string, string> = {
      'cmd+a': 'macOS: Select All', 'cmd+c': 'macOS: Copy', 'cmd+v': 'macOS: Paste', 'cmd+z': 'macOS: Undo', 'cmd+shift+z': 'macOS: Redo',
      'cmd+s': 'macOS: Save', 'cmd+o': 'macOS: Open', 'cmd+n': 'macOS: New Window', 'cmd+q': 'macOS: Quit',
      'cmd+m': 'macOS: Minimize', 'cmd+h': 'macOS: Hide', 'cmd+f': 'macOS: Find', 'cmd+r': 'macOS: Reload',
      'cmd+,': 'macOS: Preferences', 'cmd+tab': 'macOS: App Switcher', 'cmd+space': 'macOS: Spotlight',
    };
    if (systemShortcuts[s]) return systemShortcuts[s];
    if (builtIns[s]) return builtIns[s];
    for (const c of this.customCommands) { if (c.shortcut && c.shortcut.toLowerCase() === s) return c.name; }
    return '';
  }

  private escHtml(s: string): string { return s.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }

  private clearActiveTerminal(): void {
    const ap = this.panes[this.activeIndex];
    if (ap) window.go.main.App.WriteToSession(ap.pane.sessionId, btoa('clear\n'));
  }

  // --- Keyboard ---

  private handleKeydown(e: KeyboardEvent): void {
    const isMeta = e.metaKey || e.ctrlKey;

    if (this.wizardOpen) { e.stopPropagation(); this.handleWizardKey(e); return; }

    if (this.paletteOpen) {
      if (e.key === 'Escape') { e.preventDefault(); this.closePalette(); return; }
      if (e.key === 'ArrowUp') { e.preventDefault(); this.paletteCursor = Math.max(0, this.paletteCursor - 1); this.renderPalette(); (this.paletteOverlay.querySelector('.palette-input') as HTMLInputElement)?.focus(); return; }
      if (e.key === 'ArrowDown') { e.preventDefault(); this.paletteCursor = Math.min(this.getFilteredCommands().length - 1, this.paletteCursor + 1); this.renderPalette(); (this.paletteOverlay.querySelector('.palette-input') as HTMLInputElement)?.focus(); return; }
      if (e.key === 'Enter') { e.preventDefault(); const cmd = this.getFilteredCommands()[this.paletteCursor]; this.closePalette(); cmd?.action(isMeta); return; }
      if (isMeta && e.key.toLowerCase() === 'e') { e.preventDefault(); const cmd = this.getFilteredCommands()[this.paletteCursor]; if ((cmd as any)?.isCustom) this.editCustomCommand(cmd); return; }
      if (isMeta && e.key.toLowerCase() === 'd') { e.preventDefault(); const cmd = this.getFilteredCommands()[this.paletteCursor]; if ((cmd as any)?.isCustom) this.deleteCustomCommand(cmd); return; }
      return;
    }

    // Built-in shortcuts FIRST (always win over custom)
    if (isMeta) {
      if (e.shiftKey && e.key.toLowerCase() === 'c') { e.preventDefault(); this.openWizard(); return; }

      switch (e.key.toLowerCase()) {
        case 'p': e.preventDefault(); this.openPalette(); return;
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
          this.navigateSpatial(e.key.toLowerCase().replace('arrow', '') as any);
          return;
      }
    }

    // Custom command shortcuts AFTER built-in
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
            if (ap) window.go.main.App.WriteToSession(ap.pane.sessionId, btoa(c.command + '\n'));
            return;
          }
        }
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
      switch (direction) { case 'left': valid = px < cx - 10; break; case 'right': valid = px > cx + 10; break; case 'up': valid = py < cy - 10; break; case 'down': valid = py > cy + 10; break; }
      if (valid) { const dist = Math.abs(px - cx) + Math.abs(py - cy); if (dist < bestDist) { bestDist = dist; bestIndex = i; } }
    }
    if (bestIndex >= 0) this.setActive(bestIndex);
  }
}

document.addEventListener('DOMContentLoaded', async () => { const app = new ElTerminalo(); await app.init(); });
