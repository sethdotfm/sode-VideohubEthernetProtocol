// Pure helpers for the page, kept apart from the DOM so Node can test them.

// The hub's lock letters as words: U unlocked, O locked by this client (the
// bridge), L locked by another client.
export function lockLabel(l) {
  if (l === 'O') return 'Locked (this client)';
  if (l === 'L') return 'Locked (other)';
  return 'Unlocked';
}

// SW-P-08 names are at most 16 characters (Probel); Sode asks for that length.
export const NAME_MAX = 16;

// A name in two parts: what survives the 16-character limit and what is cut.
// Counts characters, not UTF-16 units, so an emoji is one.
export function splitName(name, max = NAME_MAX) {
  const chars = Array.from(name ?? '');
  return { head: chars.slice(0, max).join(''), tail: chars.slice(max).join('') };
}

// How many of these names run past the limit.
export const countLong = (names, max = NAME_MAX) => names.filter((n) => Array.from(n ?? '').length > max).length;

// One row per output from a /api/state snapshot. Numbers count from 1.
export function buildRows(st) {
  const rows = [];
  for (let i = 0; i < (st.outputs || []).length; i++) {
    const src = st.route?.[i] ?? -1;
    rows.push({
      out: i + 1,
      name: st.outputs[i],
      inNum: src >= 0 ? src + 1 : null,
      inName: src >= 0 ? (st.inputs?.[src] ?? '') : '',
      lock: lockLabel(st.lock?.[i]),
    });
  }
  return rows;
}

// Case-insensitive substring over names, numbers and the lock label. Several
// words must all match (in any field).
export function filterRows(rows, query) {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (!words.length) return rows;
  return rows.filter((r) => {
    const hay = [r.out, r.name, r.inNum ?? '', r.inName, r.lock].join('\n').toLowerCase();
    return words.every((w) => hay.includes(w));
  });
}

// "3d 4h", "5m 07s": the two largest units of a duration in seconds.
export function duration(sec) {
  sec = Math.max(0, Math.floor(sec));
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60), s = sec % 60;
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${String(m).padStart(2, '0')}m`;
  if (m) return `${m}m ${String(s).padStart(2, '0')}s`;
  return `${s}s`;
}

// A zero Go time ("0001-01-01T…") or a missing one is "never".
export const isZeroTime = (t) => !t || String(t).startsWith('0001-');

// The config the form holds as the PUT body, or {error} if it isn't valid.
export function readConfig(f) {
  const int = (s) => (/^\s*-?\d+\s*$/.test(s) ? Number(s) : NaN);
  const protectDevice = int(f.protect), resyncSeconds = int(f.resync);
  if (!(protectDevice >= 0 && protectDevice <= 1023)) return { error: 'Protect device number must be a whole number from 0 to 1023.' };
  if (!(resyncSeconds >= 5 && resyncSeconds <= 3600)) return { error: 'Resync seconds must be a whole number from 5 to 3600.' };
  return { cfg: { hub: f.hub.trim(), protectDevice, resyncSeconds } };
}
