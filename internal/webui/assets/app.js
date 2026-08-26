// The interface is a thin view over /api. It holds no state beyond what is on
// screen, so a reload always shows the truth rather than a stale copy of it.
'use strict';

const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};

let currentTab = 'log';
let running = false;
let listTimer = null;
let latest = null;
let editingKey = null;

// ---------------------------------------------------------------- theme

function applyTheme(theme) {
  document.documentElement.dataset.theme = theme;
  try { localStorage.setItem('webvacuum-theme', theme); } catch { /* private mode */ }
}
applyTheme((() => {
  try { return localStorage.getItem('webvacuum-theme') || 'dark'; } catch { return 'dark'; }
})());
$('btn-theme').addEventListener('click', () => {
  applyTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
});

// ---------------------------------------------------------------- formatting

function bytes(n) {
  if (!n) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(1)) + ' ' + units[i];
}

function duration(ns) {
  if (!ns || ns < 0) return '—';
  const ms = ns / 1e6;
  if (ms < 1000) return Math.round(ms) + 'ms';
  let s = Math.round(ms / 1000);
  const h = Math.floor(s / 3600); s -= h * 3600;
  const m = Math.floor(s / 60); s -= m * 60;
  if (h) return `${h}h ${String(m).padStart(2, '0')}m`;
  if (m) return `${m}m ${String(s).padStart(2, '0')}s`;
  return `${s}s`;
}

function clock(t) {
  try { return new Date(t).toLocaleTimeString(); } catch { return ''; }
}

function statusClass(code) {
  if (!code) return 'dim';
  if (code >= 200 && code < 300) return 'good';
  if (code === 304) return 'dim';
  if (code < 400) return 'warn';
  return 'bad';
}

// shortType trims a media type down to the part that identifies it.
function shortType(mt) {
  if (!mt) return '';
  const i = mt.indexOf('/');
  if (i < 0) return mt;
  const head = mt.slice(0, i), tail = mt.slice(i + 1);
  if (['image', 'video', 'audio', 'font'].includes(head)) return mt;
  return tail.replace(/^x-/, '').replace(/^vnd\./, '');
}

// ---------------------------------------------------------------- api

async function api(path, opts) {
  const res = await fetch(path, opts);
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
  const type = res.headers.get('content-type') || '';
  return type.includes('json') ? res.json() : res.text();
}

const post = (path, body) =>
  api(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });

const control = (action, value) =>
  post('/api/control', { action, value: value || 0 })
    .catch((e) => { addLine('bad', e.message); return null; });

// ---------------------------------------------------------------- setup form

function fillSelect(select, values, selected) {
  select.innerHTML = '';
  for (const v of values || []) {
    const o = el('option', null, v);
    o.value = v;
    if (v === selected) o.selected = true;
    select.appendChild(o);
  }
}

const scopeLabels = {
  host: 'This host only (safest)',
  subdomains: 'This domain and its subdomains',
  'host+1': 'This host, plus one hop off-site',
  directory: 'This folder and below',
  rules: 'Only what the rules below allow',
  none: 'Anywhere (can be enormous)',
};

function initSetup(data) {
  const t = data.template || {};
  const sel = $('f-scope');
  sel.innerHTML = '';
  for (const v of data.presets.constraints) {
    const o = el('option', null, scopeLabels[v] || v);
    o.value = v;
    if (v === (t.scope?.constraint || 'host')) o.selected = true;
    sel.appendChild(o);
  }
  fillSelect($('f-ua'), data.presets.user_agents, t.request?.user_agent || 'chrome');
  fillSelect($('f-supporting'), data.presets.supporting, t.general?.supporting_files || 'any');
  fillSelect($('f-render'), data.presets.render_modes, t.render?.mode || 'never');

  if (t.start_urls?.length) $('f-url').value = t.start_urls[0];
  if (t.destination) $('f-dest').value = t.destination;
  if (t.general?.connections) $('f-conns').value = t.general.connections;
}

const splitList = (v) => v.split(',').map((s) => s.trim()).filter(Boolean);

function buildJob() {
  const url = $('f-url').value.trim();
  if (!url) throw new Error('Enter a website address first.');
  const job = {
    start_urls: [url],
    scope: {
      constraint: $('f-scope').value,
      hosts: splitList($('f-domains').value),
      exclude: splitList($('f-exclude').value),
      drop_query_params: splitList($('f-dropparams').value),
    },
    general: {
      connections: Number($('f-conns').value) || 8,
      ignore_robots: $('f-robots').checked,
      file_modification: $('f-localize').checked ? 'localize' : 'none',
      supporting_files: $('f-supporting').value,
      file_replacement: 'newer',
      login: 'auto',
    },
    limits: {
      max_levels: Number($('f-levels').value) || 0,
      max_files: Number($('f-maxfiles').value) || 0,
      max_bytes: $('f-maxsize').value.trim() || '0',
      max_file_size: $('f-maxfilesize').value.trim() || '0',
      max_rate: $('f-rate').value.trim() || '0',
    },
    request: {
      user_agent: $('f-ua').value,
      delay: $('f-delay').value.trim() || '0s',
    },
    webpage: { use_sitemap: $('f-sitemap').checked },
    render: { mode: $('f-render').value, scroll: $('f-scroll').checked },
  };
  const dest = $('f-dest').value.trim();
  if (dest) job.destination = dest;
  return job;
}

$('btn-start').addEventListener('click', async () => {
  const err = $('setup-error');
  err.textContent = '';
  let job;
  try { job = buildJob(); } catch (e) { err.textContent = e.message; return; }

  $('btn-start').disabled = true;
  try {
    await post('/api/start', job);
    location.reload();
  } catch (e) {
    err.textContent = e.message;
    $('btn-start').disabled = false;
  }
});

// ---------------------------------------------------------------- dashboard

function renderSnapshot(data) {
  latest = data;
  running = data.running;
  $('setup').classList.toggle('hidden', running);
  $('dash').classList.toggle('hidden', !running);
  $('controls').classList.toggle('hidden', !running);
  if (!running) {
    $('phase').textContent = 'ready';
    $('phase').className = 'phase';
    return;
  }

  const s = data.snapshot;
  const st = s.stats;

  const phase = $('phase');
  phase.textContent = s.phase;
  phase.className = 'phase ' +
    (s.phase === 'waiting for network' ? 'offline'
      : s.phase === 'paused' ? 'paused'
      : s.phase === 'failed' ? 'failed' : 'running');

  $('s-done').textContent = st.Done ?? 0;
  $('s-bytes').textContent = bytes(st.Bytes ?? 0);
  $('s-queued').textContent = (st.Pending ?? 0) + (st.Active ?? 0);
  $('s-eta').textContent = s.eta_ns ? 'about ' + duration(s.eta_ns) + ' left' : '';
  $('s-failed').textContent = st.Failed ?? 0;
  $('s-failed').className = st.Failed ? 'bad' : '';
  $('s-skipped').textContent = (st.Skipped ?? 0) + ' skipped';
  $('s-speed').textContent = bytes(s.bytes_per_sec || 0) + '/s';
  $('s-elapsed').textContent =
    (s.rate_limit ? 'capped at ' + bytes(s.rate_limit) + '/s · ' : '') + duration(s.elapsed_ns) + ' elapsed';

  const net = s.network || {};
  const offline = net.State === 1;
  $('s-net').textContent = offline ? 'offline' : 'online';
  $('s-net').className = offline ? 'warn' : 'good';
  $('s-outage').textContent = net.TotalOutages
    ? `${net.TotalOutages} outage(s), ${duration(net.TotalDowntime)} waiting` : '';

  const banner = $('offline-banner');
  banner.classList.toggle('hidden', !offline);
  if (offline) {
    banner.textContent =
      'The network is unreachable. The download is paused and will continue on its own as soon as the connection returns — nothing has been lost.';
  }

  const total = st.Total || 1;
  $('bar').style.width = Math.min(100, ((st.Done || 0) / total) * 100) + '%';
  $('dest').textContent = s.destination;

  $('btn-pause').textContent = data.paused ? 'Resume' : 'Pause';
  $('conn-count').textContent = data.config?.general?.connections ?? '–';
  renderSlots(s.slots || []);

  const done = ['done', 'stopped', 'failed'].includes(s.phase);
  $('finished').classList.toggle('hidden', !done);
  if (done) {
    $('finished-text').textContent =
      `${st.Done} files saved (${bytes(st.Bytes)}), ${st.Failed} failed, ${st.Pending} still queued.`;
    $('open-link').href = '/files/';
    $('open-link').classList.toggle('hidden', !data.entry);
  }

  if (currentTab === 'settings' && !editingKey) renderSettings(data);
}

function renderSlots(slots) {
  const tbody = $('slots');
  tbody.innerHTML = '';
  for (const s of slots) {
    const tr = el('tr');
    tr.appendChild(el('td', 'dim', String(s.id)));

    const barCell = el('td');
    const bar = el('div', 'minibar');
    const fill = el('i');
    if (s.total > 0) {
      fill.style.width = Math.min(100, (s.bytes / s.total) * 100) + '%';
    } else if (s.busy) {
      // Without a Content-Length there is no proportion to show, so the bar
      // reads as activity rather than progress.
      bar.classList.add('indet');
      fill.style.width = s.bytes > 0 ? '100%' : '0';
    } else {
      fill.style.width = '0';
    }
    bar.appendChild(fill);
    barCell.appendChild(bar);
    tr.appendChild(barCell);

    if (!s.busy) {
      const idle = el('td', 'dim', 'idle');
      idle.colSpan = 6;
      tr.appendChild(idle);
      tbody.appendChild(tr);
      continue;
    }

    tr.appendChild(el('td', 'dim', s.activity || ''));
    tr.appendChild(el('td', statusClass(s.status), s.status ? String(s.status) : '–'));
    tr.appendChild(el('td', 'num dim',
      s.total > 0 ? `${bytes(s.bytes)} / ${bytes(s.total)}` : bytes(s.bytes)));
    tr.appendChild(el('td', 'num ' + (s.speed > 0 && s.speed < 8192 ? 'warn' : 'dim'),
      s.speed > 0 ? bytes(s.speed) + '/s' : '–'));
    tr.appendChild(el('td', 'num dim', duration(s.elapsed_ns)));

    const url = el('td', 'url mono', s.url || '');
    url.title = s.url || '';
    tr.appendChild(url);
    tbody.appendChild(tr);
  }
}

// ---------------------------------------------------------------- settings

// SETTINGS mirrors what the terminal interface offers. Each entry knows where
// its value lives in the config so a change can be posted as the same shape the
// YAML file uses, which keeps one representation everywhere.
const SETTINGS = [
  { key: 'general.connections', label: 'connections', type: 'number', help: 'how many downloads run at once' },
  { key: 'request.connections_per_host', label: 'per host', type: 'number', help: 'how many at once against a single server' },
  { key: 'request.delay', label: 'delay', type: 'text', help: 'minimum wait between requests to one server, e.g. 500ms' },
  { key: 'limits.max_rate', label: 'speed limit', type: 'text', help: 'cap on total speed per second, e.g. 2MB' },
  { key: 'scope.constraint', label: 'scope', type: 'enum', preset: 'constraints' },
  { key: 'scope.hosts', label: 'extra domains', type: 'list', help: 'other domains to crawl fully' },
  { key: 'scope.asset_hosts', label: 'asset domains', type: 'list', help: 'domains allowed for files only' },
  { key: 'scope.block_hosts', label: 'blocked domains', type: 'list' },
  { key: 'scope.exclude', label: 'exclude', type: 'list', help: 'regular expressions; these win over everything' },
  { key: 'scope.include', label: 'include', type: 'list' },
  { key: 'scope.drop_query_params', label: 'drop parameters', type: 'list', help: 'e.g. session ids' },
  { key: 'general.supporting_files', label: 'supporting files', type: 'enum', preset: 'supporting' },
  { key: 'limits.max_levels', label: 'max levels', type: 'number' },
  { key: 'limits.max_files', label: 'max files', type: 'number' },
  { key: 'limits.max_bytes', label: 'max total size', type: 'text' },
  { key: 'limits.max_file_size', label: 'max file size', type: 'text' },
  { key: 'limits.min_file_size', label: 'min file size', type: 'text' },
  { key: 'request.user_agent', label: 'user agent', type: 'enum', preset: 'user_agents' },
  { key: 'request.attempts', label: 'attempts', type: 'number' },
  { key: 'request.timeout', label: 'timeout', type: 'text' },
  { key: 'general.ignore_robots', label: 'ignore robots.txt', type: 'bool' },
  { key: 'general.ignore_nofollow', label: 'ignore nofollow', type: 'bool' },
  { key: 'general.file_replacement', label: 'replace files', type: 'enum', preset: 'replacement' },
  { key: 'render.mode', label: 'render javascript', type: 'enum', preset: 'render_modes' },
  { key: 'render.scroll', label: 'render scrolling', type: 'bool' },
  { key: 'log.level', label: 'log level', type: 'enum', values: ['info', 'debug', 'warn', 'error'] },
];

const READONLY = [
  { key: 'start_urls', label: 'start urls' },
  { key: 'destination', label: 'destination' },
];

const dig = (obj, path) => path.split('.').reduce((o, k) => (o == null ? o : o[k]), obj);

// nest turns "scope.hosts" into {scope:{hosts:value}} so a single field can be
// posted without sending — and risking clobbering — everything else.
function nest(path, value) {
  const parts = path.split('.');
  const root = {};
  let cur = root;
  parts.forEach((p, i) => {
    if (i === parts.length - 1) cur[p] = value;
    else cur = (cur[p] = {});
  });
  return root;
}

function renderSettings(data) {
  const cfg = data.config;
  if (!cfg) return;
  const box = $('settings');
  box.innerHTML = '';

  for (const f of SETTINGS) {
    box.appendChild(el('div', 'skey', f.label));
    const val = el('div', 'sval');
    const raw = dig(cfg, f.key);

    if (f.type === 'bool') {
      const cb = el('input');
      cb.type = 'checkbox';
      cb.checked = !!raw;
      cb.addEventListener('change', () => applySetting(f, cb.checked));
      val.appendChild(cb);
    } else if (f.type === 'enum') {
      const sel = el('select');
      const values = f.values || data.presets[f.preset] || [];
      fillSelect(sel, values, String(raw ?? ''));
      sel.addEventListener('change', () => applySetting(f, sel.value));
      val.appendChild(sel);
    } else {
      const input = el('input');
      input.type = 'text';
      input.value = Array.isArray(raw) ? raw.join(', ') : (raw ?? '');
      input.addEventListener('focus', () => { editingKey = f.key; });
      input.addEventListener('blur', () => { editingKey = null; });
      input.addEventListener('keydown', (ev) => {
        if (ev.key === 'Enter') { ev.preventDefault(); input.blur(); applySetting(f, input.value); }
        if (ev.key === 'Escape') { input.blur(); renderSettings(latest); }
      });
      val.appendChild(input);
    }

    if (f.help) val.appendChild(el('span', 'hint', f.help));
    box.appendChild(val);
  }

  for (const f of READONLY) {
    box.appendChild(el('div', 'skey', f.label));
    const raw = dig(cfg, f.key);
    const v = el('div', 'sval');
    v.appendChild(el('span', 'text locked', Array.isArray(raw) ? raw.join(', ') : String(raw ?? '')));
    v.appendChild(el('span', 'hint', 'fixed for this job'));
    box.appendChild(v);
  }

  box.appendChild(el('div', 'skey', 'user agent sent'));
  const ua = el('div', 'sval');
  ua.appendChild(el('span', 'text locked mono', data.user_agent_resolved || ''));
  box.appendChild(ua);
}

async function applySetting(f, value) {
  if (f.type === 'list') value = splitList(String(value));
  if (f.type === 'number') value = Number(value) || 0;
  const status = $('settings-status');
  try {
    const res = await post('/api/settings', nest(f.key, value));
    status.className = 'ok';
    status.textContent = res.status;
    if (res.ignored?.length) {
      status.className = 'sub';
      status.textContent = res.status;
    }
    latest.config = res.config;
    renderSettings(latest);
  } catch (e) {
    status.className = 'error';
    status.textContent = e.message;
  }
}

// ---------------------------------------------------------------- log + lists

const logPane = $('pane-log');
const MAX_LINES = 600;

function addLine(kind, text, time) {
  if (!text) return;
  const row = el('div', 'logrow plain');
  row.appendChild(el('span', 'dim', time ? clock(time) : ''));
  row.appendChild(el('span', kind + ' msg', text));
  pushRow(row);
}

function pushRow(row) {
  const atBottom = logPane.scrollHeight - logPane.scrollTop - logPane.clientHeight < 40;
  logPane.appendChild(row);
  while (logPane.childElementCount > MAX_LINES) logPane.removeChild(logPane.firstChild);
  if (atBottom) logPane.scrollTop = logPane.scrollHeight;
}

function onEvent(ev) {
  switch (ev.kind) {
    case 'fetched': {
      // Columns rather than a sentence: a download is a set of numbers, and
      // scanning them for the slow or the large is the point of having a log.
      const row = el('div', 'logrow');
      row.appendChild(el('span', 'dim', clock(ev.time)));
      row.appendChild(el('span', statusClass(ev.status), ev.status ? String(ev.status) : '–'));
      row.appendChild(el('span', 'dim num', duration(ev.duration_ns)));
      row.appendChild(el('span', 'dim num', bytes(ev.size)));
      row.appendChild(el('span', 'dim', shortType(ev.media_type)));
      const path = el('span', 'msg', ev.path || ev.url);
      if (ev.rendered) path.prepend(el('span', 'good', 'js '));
      if (ev.attempts > 1) path.prepend(el('span', 'warn', `×${ev.attempts} `));
      row.appendChild(path);
      pushRow(row);
      break;
    }
    case 'failed': {
      const row = el('div', 'logrow');
      row.appendChild(el('span', 'dim', clock(ev.time)));
      row.appendChild(el('span', 'bad', ev.status ? String(ev.status) : '–'));
      row.appendChild(el('span', 'dim', ''));
      row.appendChild(el('span', 'dim', ''));
      row.appendChild(el('span', 'dim', ''));
      row.appendChild(el('span', 'msg bad', `${ev.url} — ${ev.message || ''}`));
      pushRow(row);
      break;
    }
    case 'network':
      addLine('warn', ev.message, ev.time);
      break;
    case 'log':
    case 'phase':
      if (ev.message) addLine(ev.level === 'error' ? 'bad' : ev.level === 'warn' ? 'warn' : '', ev.message, ev.time);
      break;
    case 'finished':
      addLine('warn', 'job ' + ev.phase, ev.time);
      break;
  }
}

async function refreshList() {
  if (currentTab === 'log' || currentTab === 'settings' || !running) return;
  const q = encodeURIComponent($('filter').value.trim());
  try {
    const rows = await api(`/api/list?status=${currentTab}&limit=500&q=${q}`);
    const tbody = $('list');
    tbody.innerHTML = '';
    if (!rows.length) {
      const tr = el('tr');
      tr.appendChild(el('td', 'sub', 'nothing here'));
      tbody.appendChild(tr);
      return;
    }
    for (const r of rows) {
      const tr = el('tr');
      tr.appendChild(el('td', 'sub mono ' + statusClass(r.c), String(r.c || r.ek || r.s || '')));
      tr.appendChild(el('td', 'num sub', r.sz ? bytes(r.sz) : ''));
      const u = el('td', 'mono', r.u);
      u.style.overflowWrap = 'anywhere';
      tr.appendChild(u);
      if (r.e) tr.appendChild(el('td', 'sub', r.e));
      tbody.appendChild(tr);
    }
  } catch { /* a transient failure must not disturb the live view */ }
}

for (const tab of document.querySelectorAll('.tab')) {
  tab.addEventListener('click', () => {
    document.querySelectorAll('.tab').forEach((t) => t.classList.remove('active'));
    tab.classList.add('active');
    currentTab = tab.dataset.tab;
    $('pane-log').classList.toggle('hidden', currentTab !== 'log');
    $('pane-settings').classList.toggle('hidden', currentTab !== 'settings');
    $('pane-list').classList.toggle('hidden', currentTab === 'log' || currentTab === 'settings');
    if (currentTab === 'settings' && latest) renderSettings(latest);
    refreshList();
  });
}
$('filter').addEventListener('input', () => {
  clearTimeout(listTimer);
  listTimer = setTimeout(refreshList, 250);
});

// ---------------------------------------------------------------- wiring

$('btn-pause').addEventListener('click', () => control('toggle'));
$('btn-localize').addEventListener('click', async () => {
  const res = await control('localize');
  if (res?.status) addLine('', res.status);
});
$('btn-faster').addEventListener('click', () => control('faster'));
$('btn-slower').addEventListener('click', () => control('slower'));
$('conn-up').addEventListener('click', () =>
  control('connections', (latest?.config?.general?.connections || 1) + 1));
$('conn-down').addEventListener('click', () =>
  control('connections', Math.max(1, (latest?.config?.general?.connections || 2) - 1)));
$('btn-stop').addEventListener('click', () => {
  if (confirm('Stop the download? Progress is saved and you can resume later.')) control('stop');
});

async function poll() {
  try {
    const data = await api('/api/snapshot');
    if (!window.__init) { window.__init = true; initSetup(data); }
    renderSnapshot(data);
    if (data.error) addLine('bad', data.error);
  } catch {
    $('phase').textContent = 'disconnected';
    $('phase').className = 'phase failed';
  }
}

const stream = new EventSource('/api/events');
stream.addEventListener('event', (e) => {
  try { onEvent(JSON.parse(e.data)); } catch { /* ignore malformed frames */ }
});

poll();
setInterval(poll, 1000);
setInterval(refreshList, 3000);
