export function renderTable(headers: string[], rows: string[][], container: HTMLElement): void {
  container.innerHTML = '';

  const table = document.createElement('table');
  table.className = 'smart-table-grid';

  const thead = document.createElement('thead');
  const headerRow = document.createElement('tr');
  let sortCol = -1;
  let sortAsc = true;
  let cachedSorted: string[][] | null = null;

  const doSort = (colIdx: number, asc: boolean) => {
    if (cachedSorted && sortCol === colIdx && sortAsc === asc) return;
    sortCol = colIdx;
    sortAsc = asc;
    cachedSorted = [...rows].sort((a, b) => {
      const va = a[colIdx] || '';
      const vb = b[colIdx] || '';
      const na = Number(va);
      const nb = Number(vb);
      if (!isNaN(na) && !isNaN(nb)) return asc ? na - nb : nb - na;
      return asc ? va.localeCompare(vb) : vb.localeCompare(va);
    });
    tbody.innerHTML = '';
    const display = cachedSorted.slice(0, 200);
    for (const row of display) tbody.appendChild(createRow(row, headers.length));
  };

  for (let i = 0; i < headers.length; i++) {
    const th = document.createElement('th');
    th.textContent = headers[i];
    th.className = 'smart-overlay-interactive';
    th.addEventListener('click', (e) => {
      e.stopPropagation();
      const newAsc = sortCol === i ? !sortAsc : true;
      doSort(i, newAsc);
    });
    headerRow.appendChild(th);
  }
  thead.appendChild(headerRow);
  table.appendChild(thead);

  const tbody = document.createElement('tbody');
  const displayRows = rows.slice(0, 200);
  for (const row of displayRows) {
    tbody.appendChild(createRow(row, headers.length));
  }
  table.appendChild(tbody);

  container.appendChild(table);

  if (rows.length > 200) {
    const more = document.createElement('div');
    more.className = 'smart-table-more smart-overlay-interactive';
    more.textContent = `Show all ${rows.length} rows`;
    more.addEventListener('click', (e) => {
      e.stopPropagation();
      tbody.innerHTML = '';
      for (const row of rows) tbody.appendChild(createRow(row, headers.length));
      more.remove();
    });
    container.appendChild(more);
  }
}

function createRow(row: string[], colCount: number): HTMLElement {
  const tr = document.createElement('tr');
  for (let i = 0; i < colCount; i++) {
    const td = document.createElement('td');
    td.textContent = i < row.length ? (row[i] || '') : '';
    tr.appendChild(td);
  }
  return tr;
}
