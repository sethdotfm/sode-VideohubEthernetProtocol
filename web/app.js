// The Sode · Videohub page: status, settings and a live table of outputs.
// Builds DOM with el(), never innerHTML: labels come from the hub.

import { buildRows, filterRows, duration, isZeroTime, readConfig } from './helpers.js';

const $ = (id) => document.getElementById(id);

// el('div', 'cls', child, 'text', …), as Sode's core.js.
function el(tag, cls, ...kids) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  for (const k of kids) if (k != null) e.append(k);
  return e;
}

// A status is always a dot and text, never colour alone.
function setStatus(id, kind, text) {
  $(`${id}-dot`).className = `pb-dot pb-dot--${kind}`;
  $(`${id}-text`).textContent = text;
}

// ---- theme: this browser's own choice, else the system's; Alt/Option+Shift+T

const THEMES = { system: 'System', light: 'Light', dark: 'Dark' };
const store = {
  get(k) { try { return localStorage.getItem(k); } catch (e) { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch (e) {} },
};
const root = document.documentElement;
function setTheme(t) {
  if (t === 'system') delete root.dataset.theme;
  else root.dataset.theme = t;
  store.set('sode-theme', t);
  $('cfg-theme').value = t;
}
const osDark = matchMedia('(prefers-color-scheme: dark)');
const isDark = () => (root.dataset.theme ? root.dataset.theme === 'dark' : osDark.matches);
for (const [v, label] of Object.entries(THEMES)) {
  const o = el('option', null, label);
  o.value = v;
  $('cfg-theme').append(o);
}
$('cfg-theme').value = root.dataset.theme || 'system';
$('cfg-theme').addEventListener('change', () => setTheme($('cfg-theme').value));
if (/Mac|iPhone|iPad/.test(navigator.platform)) $('cfg-theme-hint').replaceChildren('Saved in this browser only. ', el('code', null, '⌥⇧t'), ' toggles.');
// Matched by e.code because Option changes e.key on macOS.
document.addEventListener('keydown', (e) => {
  if (e.code !== 'KeyT' || !e.shiftKey || !e.altKey || e.ctrlKey || e.metaKey || e.repeat) return;
  e.preventDefault();
  setTheme(isDark() ? 'light' : 'dark');
}, true);

// ---- live state

let st = null; // last /api/state
let rows = [];

function render() {
  if (!st) return;
  if (st.up) {
    setStatus('st-hub', 'ok', 'Connected');
    setStatus('conn-hub', 'ok', 'Videohub up');
  } else {
    setStatus('st-hub', 'error', 'Down');
    setStatus('conn-hub', 'error', 'Videohub down');
  }
  $('st-hub-note').textContent = [st.hub, st.model].filter(Boolean).join(' · ') || 'No address set';
  const n = st.clients;
  $('st-swp').replaceChildren(el('span', 'vep__num', st.listen || '–'), ` · ${n} Sode client${n === 1 ? '' : 's'} connected`);
  $('st-swp-note').textContent = '';
  $('st-resync').textContent = isZeroTime(st.lastResync) ? 'Not yet' : `${new Date(st.lastResync).toLocaleTimeString()} (${duration((Date.now() - new Date(st.lastResync)) / 1000)} ago)`;
  $('st-uptime').textContent = isZeroTime(st.started) ? '–' : duration((Date.now() - new Date(st.started)) / 1000);
  $('st-version').textContent = st.version ? `Sode · Videohub ${st.version}` : '';
}

function renderRows() {
  const shown = filterRows(rows, $('filter').value);
  const body = $('rows');
  body.replaceChildren(...shown.map((r) => el('tr', null,
    el('td', 'vep__num', String(r.out)),
    el('td', 'vep__name', r.name),
    el('td', 'vep__name', r.inNum == null ? el('span', 'vep__dim', 'Nothing routed') : el('span', null, el('span', 'vep__num', String(r.inNum)), ` ${r.inName}`)),
    el('td', null, lockCell(r.lock)),
  )));
  $('count').textContent = rows.length ? `${shown.length} of ${rows.length}` : '';
  const empty = $('empty');
  empty.hidden = shown.length > 0;
  empty.textContent = rows.length ? 'No outputs match.' : (st && st.up ? 'The Videohub has no outputs.' : 'Videohub is down: no outputs to show.');
}

// Locked states get a badge, but the words carry the meaning.
function lockCell(label) {
  if (label === 'Unlocked') return label;
  return el('span', `pb-badge pb-badge--${label.includes('other') ? 'warn' : 'info'}`, label);
}

function apply(next) {
  st = next;
  rows = buildRows(st);
  render();
  renderRows();
}
$('filter').addEventListener('input', renderRows);
setInterval(render, 1000); // uptime and "ago" tick between events

function connect() {
  const es = new EventSource('events');
  es.addEventListener('state', (e) => {
    setStatus('conn-page', 'ok', 'Bridge up');
    try { apply(JSON.parse(e.data)); } catch (err) { /* ignore a bad frame */ }
  });
  es.onerror = () => setStatus('conn-page', 'warn', 'Bridge reconnecting'); // EventSource retries by itself
}
connect();

// ---- settings

const form = $('settings');
const formValues = () => ({ hub: $('cfg-hub').value, protect: $('cfg-protect').value, resync: $('cfg-resync').value, debug: $('cfg-debug').checked });
let clean = null; // formValues() as last loaded or saved
const dirty = () => clean !== null && JSON.stringify(formValues()) !== JSON.stringify(clean);

function fill(c) {
  $('cfg-hub').value = c.hub;
  $('cfg-protect').value = c.protectDevice;
  $('cfg-resync').value = c.resyncSeconds;
  $('cfg-debug').checked = !!c.debug;
  clean = formValues();
}

function note(text) { $('settings-note').textContent = text; }
function showError(text) { $('settings-err').textContent = text; $('settings-err').hidden = !text; }

function touched() {
  if (!dirty()) return;
  note('Unsaved changes');
  showError('');
}
for (const id of ['cfg-hub', 'cfg-protect', 'cfg-resync', 'cfg-debug']) {
  $(id).addEventListener('input', touched);
  $(id).addEventListener('change', touched);
}
window.addEventListener('beforeunload', (e) => { if (dirty()) e.preventDefault(); });

async function loadConfig() {
  try {
    const r = await fetch('api/config');
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    fill(await r.json());
  } catch (err) {
    showError(`Could not load the settings: ${err.message}`);
  }
}

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  const read = readConfig(formValues());
  if (read.error) return showError(read.error);
  showError('');
  $('settings-save').disabled = true;
  note('Saving…');
  try {
    const r = await fetch('api/config', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(read.cfg) });
    const body = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(body.error || `HTTP ${r.status}`);
    fill(body);
    note(`Saved ${new Date().toLocaleTimeString()}`);
  } catch (err) {
    note(dirty() ? 'Unsaved changes' : '');
    showError(`Not saved: ${err.message}`);
  } finally {
    $('settings-save').disabled = false;
  }
});

loadConfig();
