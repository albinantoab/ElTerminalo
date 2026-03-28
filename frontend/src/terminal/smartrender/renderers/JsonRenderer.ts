export function renderJson(parsed: unknown, container: HTMLElement): void {
  container.innerHTML = '';
  const wrapper = document.createElement('div');
  wrapper.className = 'smart-json';
  wrapper.appendChild(renderValue(parsed, 0, 2));
  container.appendChild(wrapper);
}

function renderValue(value: unknown, depth: number, maxExpand: number): HTMLElement {
  if (value === null) return span('null', 'smart-json-null');
  if (typeof value === 'string') return span(`"${value}"`, 'smart-json-string');
  if (typeof value === 'number') return span(String(value), 'smart-json-number');
  if (typeof value === 'boolean') return span(String(value), 'smart-json-boolean');
  if (Array.isArray(value)) return renderArray(value, depth, maxExpand);
  if (typeof value === 'object') return renderObject(value as Record<string, unknown>, depth, maxExpand);
  return span(String(value), 'smart-json-string');
}

function renderObject(obj: Record<string, unknown>, depth: number, maxExpand: number): HTMLElement {
  const keys = Object.keys(obj);
  if (keys.length === 0) return span('{}', 'smart-json-null');

  const el = document.createElement('div');
  el.className = 'smart-json-node';
  const collapsed = depth >= maxExpand;

  const toggle = document.createElement('span');
  toggle.className = 'smart-json-toggle smart-overlay-interactive';
  toggle.textContent = collapsed ? '\u25B6' : '\u25BC';

  const summary = span(`{${keys.length} key${keys.length !== 1 ? 's' : ''}}`, 'smart-json-summary');
  summary.style.display = collapsed ? 'inline' : 'none';

  const body = document.createElement('div');
  body.className = 'smart-json-body';
  body.style.display = collapsed ? 'none' : 'block';

  for (const key of keys) {
    const row = document.createElement('div');
    row.className = 'smart-json-entry';
    row.appendChild(span(`"${key}"`, 'smart-json-key'));
    row.appendChild(document.createTextNode(': '));
    row.appendChild(renderValue(obj[key], depth + 1, maxExpand));
    body.appendChild(row);
  }

  toggle.addEventListener('click', (e) => {
    e.stopPropagation();
    const show = body.style.display === 'none';
    body.style.display = show ? 'block' : 'none';
    summary.style.display = show ? 'none' : 'inline';
    toggle.textContent = show ? '\u25BC' : '\u25B6';
  });

  el.appendChild(toggle);
  el.appendChild(summary);
  el.appendChild(body);
  return el;
}

function renderArray(arr: unknown[], depth: number, maxExpand: number): HTMLElement {
  if (arr.length === 0) return span('[]', 'smart-json-null');

  const el = document.createElement('div');
  el.className = 'smart-json-node';
  const collapsed = depth >= maxExpand;

  const toggle = document.createElement('span');
  toggle.className = 'smart-json-toggle smart-overlay-interactive';
  toggle.textContent = collapsed ? '\u25B6' : '\u25BC';

  const summary = span(`[${arr.length} item${arr.length !== 1 ? 's' : ''}]`, 'smart-json-summary');
  summary.style.display = collapsed ? 'inline' : 'none';

  const body = document.createElement('div');
  body.className = 'smart-json-body';
  body.style.display = collapsed ? 'none' : 'block';

  for (let i = 0; i < arr.length; i++) {
    const row = document.createElement('div');
    row.className = 'smart-json-entry';
    row.appendChild(span(`${i}`, 'smart-json-index'));
    row.appendChild(document.createTextNode(': '));
    row.appendChild(renderValue(arr[i], depth + 1, maxExpand));
    body.appendChild(row);
  }

  toggle.addEventListener('click', (e) => {
    e.stopPropagation();
    const show = body.style.display === 'none';
    body.style.display = show ? 'block' : 'none';
    summary.style.display = show ? 'none' : 'inline';
    toggle.textContent = show ? '\u25BC' : '\u25B6';
  });

  el.appendChild(toggle);
  el.appendChild(summary);
  el.appendChild(body);
  return el;
}

function span(text: string, className: string): HTMLElement {
  const el = document.createElement('span');
  el.className = className;
  el.textContent = text;
  return el;
}
