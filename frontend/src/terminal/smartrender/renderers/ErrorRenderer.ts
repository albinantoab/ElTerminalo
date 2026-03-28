export function renderError(raw: string, errorLines: number[], exitCode: number | null, container: HTMLElement): void {
  container.innerHTML = '';
  const wrapper = document.createElement('div');
  wrapper.className = 'smart-error';

  // Header
  const header = document.createElement('div');
  header.className = 'smart-error-header';
  const dot = document.createElement('span');
  dot.className = 'smart-error-dot';
  header.appendChild(dot);
  header.appendChild(document.createTextNode(` Error${exitCode ? ` (exit ${exitCode})` : ''}`));
  wrapper.appendChild(header);

  // Output with highlighted error lines
  const lines = raw.split('\n');
  const errorSet = new Set(errorLines);
  const pre = document.createElement('pre');
  pre.className = 'smart-error-output';

  for (let i = 0; i < lines.length; i++) {
    const lineEl = document.createElement('div');
    lineEl.className = errorSet.has(i) ? 'smart-error-line' : 'smart-error-normal';
    lineEl.textContent = lines[i];
    pre.appendChild(lineEl);
  }

  wrapper.appendChild(pre);
  container.appendChild(wrapper);
}
