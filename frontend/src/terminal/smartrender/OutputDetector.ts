export type DetectionResult =
  | { type: 'none' }
  | { type: 'json'; parsed: unknown; raw: string }
  | { type: 'table'; headers: string[]; rows: string[][]; raw: string }
  | { type: 'error'; raw: string; errorLines: number[]; exitCode: number | null };

const ERROR_PATTERNS = [
  /^error[:\[]/i,
  /\berror:/i,
  /^fatal:/i,
  /\bfatal:/i,
  /^panic:/i,
  /Traceback \(most recent call last\)/,
  /^\s+at\s+.+\(.+:\d+:\d+\)/,       // JS/Java stack frames
  /^\s+File ".+", line \d+/,          // Python stack frames
  /\bFAILED\b/,
  /\bFAIL\b/,
  /^error\[E\d+\]/,                   // Rust compiler
  /command not found/,
  /permission denied/i,
  /no such file or directory/i,
];

export function detect(output: string, exitCode: number | null): DetectionResult {
  if (!output || output.length < 5) return { type: 'none' };

  const lines = output.split('\n');
  if (lines.length > 2000) return { type: 'none' };

  // 1. JSON detection — try multiple strategies since terminal output
  // can have soft-wrapped lines and newlines inside string values
  const trimmed = output.trim();
  if (trimmed[0] === '{' || trimmed[0] === '[') {
    const jsonResult = tryParseJson(trimmed) || tryParseJson(lines.join(' '));
    if (jsonResult && isNonTrivialJson(jsonResult)) {
      return { type: 'json', parsed: jsonResult, raw: output };
    }
  }

  // 2. Error detection
  if (lines.length >= 2) {
    const errorLines: number[] = [];
    for (let i = 0; i < lines.length; i++) {
      if (ERROR_PATTERNS.some(p => p.test(lines[i]))) {
        errorLines.push(i);
      }
    }
    const isError = errorLines.length >= 2 || (errorLines.length >= 1 && exitCode !== null && exitCode !== 0);
    if (isError) {
      return { type: 'error', raw: output, errorLines, exitCode };
    }
  }

  // 3. Table detection
  if (lines.length >= 3) {
    const table = detectTable(lines);
    if (table) {
      return { type: 'table', headers: table.headers, rows: table.rows, raw: output };
    }
  }

  return { type: 'none' };
}

const MAX_JSON_LENGTH = 500_000;

function tryParseJson(text: string): unknown | null {
  const t = text.trim();
  if (t.length > MAX_JSON_LENGTH) return null;
  const first = t[0];
  if (first !== '{' && first !== '[') return null;
  try { return JSON.parse(t); } catch { /* continue */ }
  try {
    const fixed = t.replace(/"([^"\\]|\\.)*"/g, match => match.replace(/\n/g, ' '));
    return JSON.parse(fixed);
  } catch { /* continue */ }
  try {
    return JSON.parse(t.replace(/\s+/g, ' '));
  } catch { return null; }
}

function isNonTrivialJson(val: unknown): boolean {
  if (Array.isArray(val)) return val.length > 2;
  if (typeof val === 'object' && val !== null) {
    const keys = Object.keys(val);
    if (keys.length <= 1) return false;
    // Check for nested structure
    return keys.length > 2 || keys.some(k => typeof (val as Record<string, unknown>)[k] === 'object');
  }
  return false;
}

function detectTable(lines: string[]): { headers: string[]; rows: string[][] } | null {
  const pipeResult = detectPipeTable(lines);
  if (pipeResult) return pipeResult;

  const tabResult = detectTabTable(lines);
  if (tabResult) return tabResult;

  // Header-based detection: first line has 2+ uppercase words separated by 2+ spaces
  // (docker ps, kubectl get, ps aux, etc.)
  const headerResult = detectHeaderTable(lines);
  if (headerResult) return headerResult;

  return null;
}

function detectHeaderTable(lines: string[]): { headers: string[]; rows: string[][] } | null {
  const nonEmpty = lines.filter(l => l.trim().length > 0);
  if (nonEmpty.length < 2) return null;

  const firstLine = nonEmpty[0];

  // Split header on 2+ spaces — single-space groups stay merged
  const headerWords = firstLine.split(/\s{2,}/).map(s => s.trim()).filter(Boolean);
  if (headerWords.length < 2) return null;

  const uppercaseCount = headerWords.filter(w => /^[%]?[A-Z][A-Z0-9 _/%-]*$/.test(w)).length;
  if (uppercaseCount < 2 || uppercaseCount < headerWords.length * 0.5) return null;

  // Use header word positions as column boundaries
  const colStarts: number[] = [];
  for (const word of headerWords) {
    const idx = firstLine.indexOf(word, colStarts.length > 0 ? colStarts[colStarts.length - 1] + 1 : 0);
    if (idx >= 0) colStarts.push(idx);
  }

  if (colStarts.length < 2) return null;

  const splitLine = (line: string): string[] => {
    const cols: string[] = [];
    for (let i = 0; i < colStarts.length; i++) {
      const start = colStarts[i];
      const end = i + 1 < colStarts.length ? colStarts[i + 1] : line.length;
      cols.push(line.substring(start, end).trim());
    }
    return cols;
  };

  const headers = headerWords;
  const rows = nonEmpty.slice(1).map(splitLine);

  return { headers, rows };
}

function detectTabTable(lines: string[]): { headers: string[]; rows: string[][] } | null {
  const tabLines = lines.filter(l => l.includes('\t'));
  if (tabLines.length < 3 || tabLines.length < lines.length * 0.5) return null;

  const splitRows = tabLines.map(l => l.split('\t').map(s => s.trim()));
  const colCount = splitRows[0].length;
  if (colCount < 2) return null;

  // Check consistency
  const consistent = splitRows.filter(r => r.length === colCount);
  if (consistent.length < splitRows.length * 0.6) return null;

  const headers = consistent[0];
  const rows = consistent.slice(1);

  return { headers, rows };
}

function detectPipeTable(lines: string[]): { headers: string[]; rows: string[][] } | null {
  const pipeLines = lines.filter(l => l.includes('|'));
  if (pipeLines.length < 3 || pipeLines.length < lines.length * 0.5) return null;

  const headers = pipeLines[0].split('|').map(s => s.trim()).filter(Boolean);
  if (headers.length < 2) return null;

  // Skip separator line if present
  const startIdx = /^[\s|+-]+$/.test(pipeLines[1]) ? 2 : 1;
  const rows = pipeLines.slice(startIdx).map(l =>
    l.split('|').map(s => s.trim()).filter(Boolean)
  );

  return { headers, rows };
}

