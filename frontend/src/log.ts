// One place for everything the app has to say about itself going wrong.
//
// The console is where a developer with devtools open reads it; the backend's
// log file is the copy that survives the window, and the one a user can hand
// over with a bug report ("Reveal Logs" in the palette opens it in Finder).
// Every message goes to both, and neither path may ever throw: these calls sit
// inside catch blocks, where a second failure would replace the real problem.

type LogLevel = 'info' | 'warn' | 'error';

/** Flatten whatever was thrown into something worth writing down.
 *
 *  An Error's stack is taken in preference to its message. The bundle ships
 *  minified and without sourcemaps, so a message on its own — "Cannot read
 *  properties of undefined (reading 'panes')" — cannot be attributed to any of
 *  the call sites that report one, and the frames are the only thing that can.
 *  The stack already begins with "<name>: <message>", so nothing is lost by it;
 *  the backend's log sizes its per-message cap for exactly this, and folds the
 *  newlines away so one report stays one entry. */
function describe(err: unknown): string {
  if (err instanceof Error) return err.stack || err.message || err.name || String(err);
  if (typeof err === 'string') return err;
  // Wails rejects bindings with plain strings, but a DOM exception or a bare
  // object can land here too, and "[object Object]" says nothing.
  try {
    const json = JSON.stringify(err);
    if (json && json !== '{}') return json;
  } catch { /* circular or otherwise unprintable */ }
  return String(err);
}

function emit(level: LogLevel, message: string, err?: unknown): void {
  // The caller's label stays in front of the description, stack and all: a
  // stack names where the error was *thrown*, and only the label says which of
  // the app's operations was the one that failed.
  const text = err === undefined ? message : `${message}: ${describe(err)}`;
  if (level === 'error') console.error(text);
  else if (level === 'warn') console.warn(text);
  else console.info(text);

  // Best-effort mirror to the log file. The binding does not exist until the
  // Wails runtime has installed it — early startup, or a plain `vite dev` in a
  // browser — and the console line above is then the whole record.
  const app = window.go?.main?.App;
  if (!app?.LogMessage) return;
  try {
    app.LogMessage(level, text).catch(() => { /* logging must not need logging */ });
  } catch { /* nor may a binding that isn't really there */ }
}

export function logInfo(message: string, err?: unknown): void { emit('info', message, err); }
export function logWarn(message: string, err?: unknown): void { emit('warn', message, err); }
export function logError(message: string, err?: unknown): void { emit('error', message, err); }
