/* FORGE front end.
 *
 * No framework and no build step, for the same reason the agent has no
 * dependencies: this ships inside a single Go binary and has to render with
 * no network. Everything below is plain DOM.
 */

'use strict';

/* ---------- icons ---------- */

const ICON = {
  forge: 'M9 21V6h11M9 13.5h8',
  sessions: 'M4 5h16v11H8l-4 4z',
  plus: 'M12 5v14M5 12h14',
  search: 'M11 19a8 8 0 1 1 0-16 8 8 0 0 1 0 16zM21 21l-4.3-4.3',
  repo: 'M4 7c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3zM4 7v10c0 1.7 3.6 3 8 3s8-1.3 8-3V7',
  cloud: 'M6.5 19a4.5 4.5 0 0 1 0-9 6 6 0 0 1 11.6 1.6A3.7 3.7 0 0 1 17.5 19z',
  chart: 'M5 20V10M12 20V4M19 20v-7',
  check: 'M4 12.5 9 17.5 20 6.5',
  x: 'M6 6l12 12M18 6L6 18',
  file: 'M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8zM14 3v5h5',
  gear: 'M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7zM19.4 15a1.6 1.6 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.6 1.6 0 0 0-2.7 1.1V21a2 2 0 1 1-4 0v-.1A1.6 1.6 0 0 0 8 19.4a1.6 1.6 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.6 1.6 0 0 0-1.1-2.7H2a2 2 0 1 1 0-4h.1A1.6 1.6 0 0 0 4.6 8a1.6 1.6 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.6 1.6 0 0 0 2.7-1.1V2a2 2 0 1 1 4 0v.1A1.6 1.6 0 0 0 16 4.6a1.6 1.6 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.6 1.6 0 0 0 1.1 2.7H22a2 2 0 1 1 0 4h-.1a1.6 1.6 0 0 0-1.5 1z',
  stop: 'M7 7h10v10H7z',
  send: 'M4 12l16-8-6 8 6 8z',
  clock: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 7v5l3 2',
  bolt: 'M13 3 5 14h6l-1 7 8-11h-6z',
  refresh: 'M20 11a8 8 0 1 0-1.2 5.2M20 5v6h-6',
  folder: 'M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z',
  shield: 'M12 21s7-3.2 7-9V6l-7-3-7 3v6c0 5.8 7 9 7 9z',
};

function icon(name, size = 18) {
  const d = ICON[name];
  if (!d) return '';
  return `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"
    aria-hidden="true">${
      d.split('M').filter(Boolean).map(p => `<path d="M${p}"/>`).join('')
    }</svg>`;
}

/* ---------- small helpers ---------- */

const $ = (sel, root = document) => root.querySelector(sel);
const esc = s => String(s ?? '').replace(/[&<>"']/g, c =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

function el(html) {
  const t = document.createElement('template');
  t.innerHTML = html.trim();
  return t.content.firstElementChild;
}

function nfmt(n) {
  n = Number(n) || 0;
  if (n >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, '') + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'K';
  return String(n);
}

function dur(ms) {
  const s = Math.floor((Number(ms) || 0) / 1000);
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60), r = s % 60;
  if (m < 60) return `${m}m ${String(r).padStart(2, '0')}s`;
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, '0')}m`;
}

function ago(ts) {
  const d = Date.now() - Number(ts);
  if (d < 60e3) return 'just now';
  if (d < 3600e3) return Math.floor(d / 60e3) + 'm ago';
  if (d < 86400e3) return Math.floor(d / 3600e3) + 'h ago';
  return Math.floor(d / 86400e3) + 'd ago';
}

/* Minimal inline markdown: backtick code only. Anything richer would need a
 * parser, and the agent's prose is plain text with identifiers in it. */
function prose(text) {
  return esc(text).replace(/`([^`\n]+)`/g, (_, c) => `<code>${c}</code>`);
}

function toast(msg, isErr) {
  const t = el(`<div class="toast${isErr ? ' err' : ''}">${esc(msg)}</div>`);
  $('#toasts').appendChild(t);
  setTimeout(() => {
    t.style.transition = 'opacity 300ms';
    t.style.opacity = '0';
    setTimeout(() => t.remove(), 320);
  }, isErr ? 6000 : 3200);
}

/* ---------- api ---------- */

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: opts.body ? { 'Content-Type': 'application/json' } : {},
    ...opts,
  });
  const text = await res.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { data = { error: text }; }
  }
  if (!res.ok) throw new Error((data && data.error) || `HTTP ${res.status}`);
  return data;
}

/* ---------- state ---------- */

const State = {
  boot: null,
  sessions: [],
  route: { name: 'sessions', params: {} },
  live: null,        // { id, es, events, byId, pending }
  workspace: '',
};

/* ---------- router ---------- */

const ROUTES = [
  { id: 'sessions', label: 'Sessions', ic: 'sessions' },
  { id: 'new', label: 'New Session', ic: 'plus' },
  { id: 'search', label: 'Search', ic: 'search' },
  { id: 'repo', label: 'Repository', ic: 'repo' },
  { id: 'providers', label: 'Providers', ic: 'cloud', secondary: true },
  { id: 'verify', label: 'Verification', ic: 'shield', secondary: true },
  { id: 'usage', label: 'Usage', ic: 'chart', secondary: true },
  { id: 'settings', label: 'Settings', ic: 'gear', secondary: true },
];

function parseHash() {
  const raw = (location.hash || '#/sessions').slice(2);
  const [name, ...rest] = raw.split('/');
  return { name: name || 'sessions', params: { id: rest[0] || '' } };
}

function go(path) { location.hash = '#/' + path; }

async function render() {
  State.route = parseHash();
  renderNav();
  renderCrumbs();
  renderWorkspaceChip();

  const view = $('#view');
  const { name, params } = State.route;

  // Leaving a session tears the stream down; nothing else should hold one.
  if (!(name === 'session' && State.live && State.live.id === params.id)) {
    closeStream();
  }

  try {
    switch (name) {
      case 'sessions': view.replaceChildren(await viewSessions()); break;
      case 'new':      view.replaceChildren(viewNew()); break;
      case 'session':  view.replaceChildren(await viewSession(params.id)); break;
      case 'search':   view.replaceChildren(viewSearch()); break;
      case 'repo':     view.replaceChildren(await viewRepo()); break;
      case 'providers':view.replaceChildren(await viewProviders()); break;
      case 'verify':   view.replaceChildren(viewVerify()); break;
      case 'usage':    view.replaceChildren(await viewUsage()); break;
      case 'settings': view.replaceChildren(viewSettings()); break;
      default:         go('sessions'); return;
    }
  } catch (e) {
    // "Failed to fetch" means the page loaded from the service worker's cache
    // but the server behind it is gone — the shell renders and every call
    // fails. Saying so is the difference between a fixable problem and a
    // browser error message nobody can act on.
    if (isOffline(e)) {
      view.replaceChildren(serverGoneScreen());
      return;
    }
    view.replaceChildren(el(`<div class="page"><div class="card">
      <div class="t-meta bad">Error</div>
      <p class="t-body">${esc(e.message)}</p></div></div>`));
  }
}

function isOffline(e) {
  return e instanceof TypeError || /failed to fetch|networkerror|load failed/i.test(e.message || '');
}

// showError renders a failure into a panel a view owns. Views that load their
// own data asynchronously cannot throw to render()'s handler, so without this
// they each reinvent an error box — and the one the user actually hit read
// "Failed to fetch", which says nothing about what to do.
function showError(container, e) {
  if (isOffline(e)) {
    container.replaceChildren(serverGoneScreen());
    return;
  }
  container.replaceChildren(el(`<div class="card">
    <div class="t-meta bad">Error</div>
    <p class="t-body" style="margin:8px 0 0">${esc(e.message)}</p></div>`));
}

function serverGoneScreen() {
  const page = el(`<div class="page" style="max-width:640px">
    <div class="page-head">
      <h1 class="t-display">FORGE is not running</h1>
      <p class="t-body">This window is showing a cached copy of the interface.
        The program behind it is not answering on <code>${esc(location.host)}</code>.</p>
    </div>
    <div class="card">
      <div class="t-meta" style="margin-bottom:12px">Start it again</div>
      <div class="stack" style="gap:14px">
        <div>
          <div class="t-sm dim" style="margin-bottom:6px">From the Start Menu or Desktop</div>
          <div class="t-mono">FORGE</div>
        </div>
        <div>
          <div class="t-sm dim" style="margin-bottom:6px">Or in a terminal</div>
          <div class="t-mono">forge app</div>
        </div>
      </div>
    </div>
    <div class="card" style="margin-top:14px">
      <div class="t-meta" style="margin-bottom:10px">If it still will not connect</div>
      <p class="t-sm dim" style="margin:0">
        The local model needs Ollama running — <span class="t-mono">ollama serve</span>.
        Check the rest with <span class="t-mono">forge doctor</span>.
      </p>
    </div>
    <div class="row" style="gap:10px;margin-top:18px">
      <button class="btn btn-primary" id="retry">${icon('refresh', 16)} Try again</button>
    </div>
  </div>`);

  $('#retry', page).onclick = async () => {
    try {
      State.boot = await api('/api/bootstrap');
      State.workspace = State.boot.workspace || '';
      toast('Reconnected');
      render();
    } catch {
      toast('Still no answer from ' + location.host, true);
    }
  };
  return page;
}

function renderNav() {
  const nav = $('#nav');
  const active = State.route.name;
  const running = State.sessions.filter(s => s.status === 'running').length;

  nav.replaceChildren(...ROUTES.flatMap(r => {
    const items = [];
    if (r.id === 'providers') {
      items.push(el(`<div class="nav-group t-meta">System</div>`));
    }
    const isActive = active === r.id || (active === 'session' && r.id === 'sessions');
    const badge = r.id === 'sessions' && running
      ? `<span class="count">${running}</span>` : '';
    const b = el(`<button class="nav-item${isActive ? ' active' : ''}${r.secondary ? ' secondary' : ''}">
      ${icon(r.ic, 19)}<span>${esc(r.label)}</span>${badge}</button>`);
    b.onclick = () => go(r.id);
    items.push(b);
    return items;
  }));
}

// The workspace bounds everything the agent may touch, so it belongs in the
// chrome rather than buried in settings — and a desktop launcher cannot pass
// -dir, which makes changing it from here the only way.
function openWorkspacePicker() {
  const cur = State.workspace || '';
  const panel = el(`<div class="overlay"><div class="overlay-panel" style="max-width:620px">
    <div class="overlay-head">
      <div class="t-meta">Workspace</div>
      <div class="t-section" style="margin-top:6px">Where may the agent act?</div>
    </div>
    <div class="overlay-body">
      <div class="field">
        <label for="ws-in">Absolute path to a project folder</label>
        <input class="input t-mono" id="ws-in" spellcheck="false" value="${esc(cur)}">
      </div>
      <p class="t-sm dimmer" style="margin:12px 0 0">
        The agent cannot read or write outside this folder. Point it at one
        repository — a whole home directory makes the repository map and the
        search index enormous and slow.
      </p>
    </div>
    <div class="overlay-foot">
      <button class="btn btn-ghost" data-x="cancel">Cancel</button>
      <button class="btn btn-primary" data-x="save">Use this folder</button>
    </div>
  </div></div>`);

  const input = $('#ws-in', panel);
  const save = async () => {
    const dir = input.value.trim();
    if (!dir) return;
    const btn = panel.querySelector('[data-x="save"]');
    btn.disabled = true;
    try {
      State.boot = await api('/api/workspace', { method: 'POST', body: JSON.stringify({ dir }) });
      State.workspace = State.boot.workspace;
      closeOverlay();
      toast('Workspace set to ' + State.workspace);
      render();
    } catch (e) {
      toast(e.message, true);
      btn.disabled = false;
    }
  };

  panel.querySelector('[data-x="save"]').onclick = save;
  panel.querySelector('[data-x="cancel"]').onclick = closeOverlay;
  panel._keys = e => {
    if (e.key === 'Escape') { e.preventDefault(); closeOverlay(); }
    else if (e.key === 'Enter' && document.activeElement === input) { e.preventDefault(); save(); }
  };
  document.addEventListener('keydown', panel._keys);

  openOverlay(panel);
  input.focus();
  input.select();
}

function renderWorkspaceChip() {
  const host = document.getElementById('ws-chip');
  if (!host) return;
  const w = State.workspace || '';
  const short = w.split(/[\\/]/).filter(Boolean).slice(-2).join('/') || 'choose a folder';
  host.innerHTML = `${icon('folder', 14)}<span class="truncate">${esc(short)}</span>`;
  host.title = w || 'No workspace selected';
  host.onclick = openWorkspacePicker;
}

function renderCrumbs() {
  const { name, params } = State.route;
  const route = ROUTES.find(r => r.id === name);
  let label = route ? route.label : name;
  if (name === 'session') {
    const s = State.sessions.find(x => x.id === params.id);
    label = s ? (s.task.length > 60 ? s.task.slice(0, 60) + '…' : s.task) : 'Session';
  }
  $('#crumbs').innerHTML =
    `<span class="dimmer">Forge</span><span class="dimmer">/</span><span>${esc(label)}</span>`;
}

function setSignal(on) {
  const s = $('#signal');
  if (on) {
    s.classList.remove('idle');
    if (!s.firstChild) s.appendChild(el('<div class="bar"></div>'));
  } else {
    s.classList.add('idle');
    s.replaceChildren();
  }
}

/* ---------- view: sessions ---------- */

async function viewSessions() {
  State.sessions = await api('/api/sessions');
  renderNav();

  const page = el(`<div class="page"></div>`);
  page.appendChild(el(`<div class="page-head">
    <h1 class="t-display">Workspace</h1>
    <p class="t-body">${esc(State.workspace || '')}</p>
  </div>`));

  const running = State.sessions.filter(s => s.status === 'running').length;
  const totTok = State.sessions.reduce((a, s) => a + s.promptTokens + s.completionTokens, 0);
  const changed = new Set(State.sessions.flatMap(s => s.changed || [])).size;

  page.appendChild(el(`<div class="grid grid-4" style="margin-bottom:28px">
    ${statCard('Sessions', State.sessions.length)}
    ${statCard('Running', running, running ? 'accent' : '')}
    ${statCard('Tokens', nfmt(totTok))}
    ${statCard('Files touched', changed)}
  </div>`));

  const start = el(`<button class="btn btn-primary" style="margin-bottom:22px">
    ${icon('plus', 16)} New session</button>`);
  start.onclick = () => go('new');
  page.appendChild(start);

  if (!State.sessions.length) {
    page.appendChild(el(`<div class="card empty">
      ${icon('sessions', 34)}
      <div class="t-body">No sessions yet.</div>
      <div class="t-sm dimmer">Describe a task and forge will work in your repository.</div>
    </div>`));
    return page;
  }

  const list = el(`<div class="card" style="padding:0;overflow:hidden"></div>`);
  for (const s of State.sessions) {
    const row = el(`<button class="list-row">
      <span class="dot ${statusDot(s.status)}${s.status === 'running' ? ' live' : ''}"></span>
      <span class="col grow" style="gap:3px;min-width:0">
        <span class="truncate">${esc(s.task)}</span>
        <span class="t-mono dimmer truncate">${esc(s.class)} · step ${s.step}/${s.maxSteps} · ${nfmt(s.promptTokens + s.completionTokens)} tok</span>
      </span>
      ${s.pendingApprovals ? `<span class="chip warn">needs approval</span>` : ''}
      <span class="chip ${statusChip(s.status)}">${esc(s.status)}</span>
      <span class="t-mono dimmer" style="flex:none">${ago(s.created)}</span>
    </button>`);
    row.onclick = () => go('session/' + s.id);
    list.appendChild(row);
  }
  page.appendChild(list);
  return page;
}

function statCard(label, value, cls = '') {
  return `<div class="card stat">
    <div class="t-meta">${esc(label)}</div>
    <div class="n ${cls}">${esc(String(value))}</div>
  </div>`;
}

const statusDot = s => s === 'running' ? 'ok' : s === 'error' ? 'err' : s === 'cancelled' ? 'warn' : '';
const statusChip = s => s === 'running' ? 'ok' : s === 'error' ? 'err' : s === 'cancelled' ? 'warn' : 'info';

/* ---------- view: new session ---------- */

function viewNew() {
  const b = State.boot || {};
  const classes = (b.classNames || []);
  const page = el(`<div class="page" style="max-width:760px"></div>`);
  page.appendChild(el(`<div class="page-head">
    <h1 class="t-display">New session</h1>
    <p class="t-body">Describe the outcome, not the steps.</p>
  </div>`));

  const form = el(`<div class="stack-lg">
    <div class="field">
      <label for="f-task">Task</label>
      <textarea class="textarea" id="f-task" rows="5"
        placeholder="Add a health check endpoint that reports provider cooldowns as JSON."></textarea>
    </div>
    <div class="field">
      <label for="f-dir">Workspace</label>
      <input class="input" id="f-dir" value="${esc(b.workspace || '.')}">
    </div>
    <div class="grid grid-2">
      <div class="field">
        <label for="f-class">Model class</label>
        <select class="select" id="f-class">
          ${classes.map(c => `<option value="${esc(c)}"${c === b.defaultClass ? ' selected' : ''}>${esc(c)}</option>`).join('')}
        </select>
      </div>
      <div class="field">
        <label for="f-approval">Approval</label>
        <select class="select" id="f-approval">
          <option value="ask">ask — confirm every change</option>
          <option value="auto-edit" selected>auto-edit — edits pass, commands ask</option>
          <option value="yolo">yolo — everything but destructive commands</option>
          <option value="readonly">readonly — explain only</option>
        </select>
      </div>
      <div class="field">
        <label for="f-proto">Edit protocol</label>
        <select class="select" id="f-proto">
          <option value="auto" selected>auto</option>
          <option value="blocks">blocks — best for small local models</option>
          <option value="tool">tool</option>
        </select>
      </div>
      <div class="field">
        <label for="f-steps">Max steps</label>
        <input class="input" id="f-steps" type="number" min="1" max="200" value="30">
      </div>
    </div>
    <div class="row" style="gap:10px">
      <button class="btn btn-primary" id="f-go">${icon('bolt', 16)} Start</button>
      <button class="btn btn-ghost" id="f-cancel">Cancel</button>
      <span class="t-sm dimmer">${esc(classes.length ? '' : 'No model classes configured — check Providers.')}</span>
    </div>
  </div>`);
  page.appendChild(form);

  const task = $('#f-task', form);
  setTimeout(() => task.focus(), 30);

  const submit = async () => {
    const body = {
      task: task.value.trim(),
      dir: $('#f-dir', form).value.trim() || '.',
      class: $('#f-class', form).value,
      approval: $('#f-approval', form).value,
      protocol: $('#f-proto', form).value,
      maxSteps: Number($('#f-steps', form).value) || 30,
    };
    if (!body.task) { toast('Describe a task first', true); task.focus(); return; }
    const btn = $('#f-go', form);
    btn.disabled = true;
    btn.innerHTML = icon('clock', 16) + ' Starting…';
    try {
      const s = await api('/api/sessions', { method: 'POST', body: JSON.stringify(body) });
      go('session/' + s.id);
    } catch (e) {
      toast(e.message, true);
      btn.disabled = false;
      btn.innerHTML = icon('bolt', 16) + ' Start';
    }
  };

  $('#f-go', form).onclick = submit;
  $('#f-cancel', form).onclick = () => go('sessions');
  task.addEventListener('keydown', e => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') { e.preventDefault(); submit(); }
  });
  return page;
}

/* ---------- view: session ---------- */

async function viewSession(id) {
  if (!id) { go('sessions'); return el('<div></div>'); }
  const s = await api('/api/sessions/' + encodeURIComponent(id));

  const root = el(`<div id="session-layout">
    <div id="session-main">
      <div class="session-head">
        <div class="col grow" style="gap:8px;min-width:0">
          <div class="row" style="gap:10px">
            <span class="chip ${statusChip(s.status)}" id="s-status">
              <span class="dot ${statusDot(s.status)}${s.status === 'running' ? ' live' : ''}"></span>
              ${esc(s.status)}
            </span>
            <span class="t-mono dimmer" id="s-step">step ${s.step}/${s.maxSteps}</span>
          </div>
          <div class="t-section" style="font-size:17px;line-height:23px">${esc(s.task)}</div>
        </div>
        <div class="session-metrics">
          <div class="m"><span class="t-meta">Class</span><span class="t-mono">${esc(s.class)}</span></div>
          <div class="m"><span class="t-meta">Tokens</span><span class="t-mono accent" id="s-tok">${nfmt(s.promptTokens + s.completionTokens)}</span></div>
          <div class="m"><span class="t-meta">Elapsed</span><span class="t-mono" id="s-elapsed">${dur(s.elapsedMs)}</span></div>
        </div>
      </div>
      <div id="timeline"><div class="tl" id="tl"></div></div>
      <div class="composer">
        <div class="composer-inner">
          <textarea id="s-input" rows="1" placeholder="Session is read-only once it ends — start a new one to continue."></textarea>
          <button class="btn btn-danger btn-sm" id="s-cancel">${icon('stop', 14)} Stop</button>
        </div>
      </div>
    </div>
    <aside id="session-side"></aside>
  </div>`);

  $('#s-cancel', root).onclick = async () => {
    try { await api(`/api/sessions/${encodeURIComponent(id)}/cancel`, { method: 'POST' }); toast('Stopping…'); }
    catch (e) { toast(e.message, true); }
  };
  $('#s-input', root).disabled = true;

  renderSide(root, s);

  State.live = { id, es: null, byId: new Map(), lastSeq: 0, session: s, tl: null };
  State.live.tl = $('#tl', root);

  setSignal(s.status === 'running');
  openStream(id, root);
  return root;
}

function renderSide(root, s) {
  const side = $('#session-side', root);
  if (!side) return;
  const files = s.changed || [];
  const o = s.outcome;

  side.replaceChildren(el(`<div class="stack-lg">
    <div>
      <div class="t-meta" style="margin-bottom:10px">Session</div>
      <div class="stack" style="gap:8px">
        ${sideRow('Workspace', s.dir)}
        ${sideRow('Approval', s.approval)}
        ${sideRow('Protocol', s.protocol || 'auto')}
        ${sideRow('Prompt tok', nfmt(s.promptTokens))}
        ${sideRow('Output tok', nfmt(s.completionTokens))}
      </div>
    </div>
    <div>
      <div class="t-meta" style="margin-bottom:10px">Files changed (${files.length})</div>
      ${files.length ? files.map(f =>
        `<div class="row t-mono" style="gap:8px;padding:5px 0">
           <span class="accent">${icon('file', 14)}</span>
           <span class="truncate" title="${esc(f)}">${esc(f)}</span></div>`).join('')
        : '<div class="t-sm dimmer">Nothing written yet.</div>'}
    </div>
    ${o ? `<div>
      <div class="t-meta" style="margin-bottom:10px">Result</div>
      <div class="stack" style="gap:8px">
        ${sideRow('Stop reason', o.stopReason)}
        ${sideRow('Steps', o.steps)}
        ${sideRow('Verification', o.verifyRan ? (o.verified ? 'passed' : 'failed') : 'not run')}
        ${o.repairs ? sideRow('Repairs', o.repairs) : ''}
        ${o.subAgents ? sideRow('Delegations', o.subAgents) : ''}
        ${o.compactions ? sideRow('Compactions', o.compactions) : ''}
      </div>
    </div>` : ''}
  </div>`));
}

const sideRow = (k, v) => `<div class="spread" style="gap:10px">
  <span class="t-sm dimmer" style="flex:none">${esc(k)}</span>
  <span class="t-mono truncate" style="text-align:right" title="${esc(v)}">${esc(v)}</span></div>`;

/* ---------- live stream ---------- */

function closeStream() {
  if (State.live && State.live.es) {
    State.live.es.close();
    State.live.es = null;
  }
  State.live = null;
  setSignal(false);
}

function openStream(id, root) {
  const live = State.live;
  const url = `/api/sessions/${encodeURIComponent(id)}/stream?from=0`;
  const es = new EventSource(url);
  live.es = es;

  es.onmessage = ev => {
    try { applyEvent(JSON.parse(ev.data), root); } catch { /* malformed frame */ }
  };
  // Named events arrive on their own listeners; route them all through one path.
  for (const k of ['text', 'step', 'activity', 'tool.call', 'tool.result', 'file',
                   'usage', 'approval', 'approval.resolved', 'end', 'error', 'cancelled']) {
    es.addEventListener(k, ev => {
      try { applyEvent(JSON.parse(ev.data), root); } catch { /* malformed frame */ }
    });
  }
  es.addEventListener('close', () => { es.close(); live.es = null; setSignal(false); refreshSessionHead(id, root); });
  es.onerror = () => {
    // EventSource reconnects on its own; only stop the spinner if it is done.
    if (es.readyState === EventSource.CLOSED) { setSignal(false); }
  };
}

async function refreshSessionHead(id, root) {
  try {
    const s = await api('/api/sessions/' + encodeURIComponent(id));
    if (!State.live || State.live.id !== id) return;
    State.live.session = s;
    const chip = $('#s-status', root);
    if (chip) {
      chip.className = 'chip ' + statusChip(s.status);
      chip.innerHTML = `<span class="dot ${statusDot(s.status)}"></span>${esc(s.status)}`;
    }
    renderSide(root, s);
    const idx = State.sessions.findIndex(x => x.id === id);
    if (idx >= 0) State.sessions[idx] = s; else State.sessions.unshift(s);
    renderNav();
  } catch { /* the session list will catch up on the next navigation */ }
}

function applyEvent(ev, root) {
  const live = State.live;
  if (!live) return;
  live.lastSeq = Math.max(live.lastSeq, ev.seq || 0);
  const tl = live.tl;

  switch (ev.kind) {
    case 'text': appendText(tl, ev.text); break;

    case 'step': {
      live.textBlock = null;
      addItem(tl, 'is-step', `<div class="tl-label">
        <span class="t-meta">Step ${ev.step}</span></div>`);
      const stepEl = $('#s-step', root);
      if (stepEl) stepEl.textContent = `step ${ev.step}/${live.session.maxSteps}`;
      break;
    }

    case 'tool.call': {
      live.textBlock = null;
      const line = el(`<div class="tl-item is-tool">
        <div class="toolline" data-tool="${esc(ev.id || '')}">
          <span class="name">${esc(ev.name)}</span>
          <span class="args">${esc(summarizeArgs(ev.args))}</span>
          <span class="res dimmer">running…</span>
        </div></div>`);
      tl.appendChild(line);
      if (ev.id) live.byId.set(ev.id, line);
      scroll(tl);
      break;
    }

    case 'tool.result': {
      const line = ev.id && live.byId.get(ev.id);
      if (line) {
        const box = $('.toolline', line);
        const res = $('.res', box);
        res.textContent = firstLine(ev.summary);
        res.title = ev.summary || '';
        if (ev.ok === false) { box.classList.add('err'); line.classList.add('is-err'); }
        res.classList.remove('dimmer');
      }
      break;
    }

    case 'file': {
      live.textBlock = null;
      addItem(tl, 'is-tool', `<div class="toolline">
        <span class="accent" style="flex:none">${icon('file', 13)}</span>
        <span class="name">${esc(ev.path)}</span>
        <span class="args">written</span></div>`);
      break;
    }

    case 'usage': {
      const s = live.session;
      s.promptTokens += ev.prompt || 0;
      s.completionTokens += ev.completion || 0;
      const t = $('#s-tok', root);
      if (t) t.textContent = nfmt(s.promptTokens + s.completionTokens);
      const e = $('#s-elapsed', root);
      if (e) e.textContent = dur(Date.now() - s.created);
      break;
    }

    case 'approval': showApproval(ev, live.id); break;

    case 'approval.resolved': closeOverlay(); break;

    case 'end':
    case 'error':
    case 'cancelled': {
      live.textBlock = null;
      setSignal(false);
      renderEnd(tl, ev);
      refreshSessionHead(live.id, root);
      break;
    }
  }
}

function addItem(tl, cls, html) {
  const item = el(`<div class="tl-item ${cls}">${html}</div>`);
  tl.appendChild(item);
  scroll(tl);
  return item;
}

function appendText(tl, text) {
  const live = State.live;
  if (!live.textBlock) {
    live.textBuf = '';
    live.textBlock = addItem(tl, '', `<div class="bubble"><div class="prose"></div></div>`);
  }
  live.textBuf += text;
  $('.prose', live.textBlock).innerHTML = prose(live.textBuf);
  scroll(tl);
}

function renderEnd(tl, ev) {
  const o = ev.outcome || {};
  const bad = ev.kind !== 'end';
  const chip = bad ? 'err' : (o.verified ? 'ok' : 'info');
  addItem(tl, bad ? 'is-err' : 'is-end', `
    <div class="tl-label">
      <span class="chip ${chip}">${esc(ev.kind === 'end' ? (o.stopReason || 'done') : ev.kind)}</span>
      ${o.steps ? `<span class="t-mono dimmer">${o.steps} steps · ${nfmt((o.promptTokens || 0) + (o.completionTokens || 0))} tok · ${dur(o.elapsedMs)}</span>` : ''}
    </div>
    ${ev.text && bad ? `<div class="bubble" style="border-color:rgba(255,180,171,.35)"><div class="prose bad">${prose(ev.text)}</div></div>` : ''}
    ${o.finalText ? `<div class="bubble"><div class="prose">${prose(o.finalText)}</div></div>` : ''}
    ${o.verifyRan ? `<div class="toolline ${o.verified ? '' : 'err'}" style="margin-top:10px">
        <span class="name">verification</span>
        <span class="args">${esc(o.verifySummary || '')}</span>
        <span class="res">${o.verified ? 'passed' : 'failed'}</span></div>` : ''}
  `);
}

function scroll(tl) {
  const box = tl.parentElement;
  if (!box) return;
  // Only follow if the user is already near the bottom; yanking the view away
  // while they are reading earlier output is worse than not following.
  const near = box.scrollHeight - box.scrollTop - box.clientHeight < 220;
  if (near) box.scrollTop = box.scrollHeight;
}

function firstLine(s) {
  const t = String(s || '').split('\n')[0].trim();
  return t.length > 90 ? t.slice(0, 90) + '…' : t;
}

function summarizeArgs(raw) {
  if (!raw) return '';
  try {
    const o = JSON.parse(raw);
    return Object.entries(o).map(([k, v]) => {
      let s = typeof v === 'string' ? v : JSON.stringify(v);
      s = String(s).replace(/\s+/g, ' ');
      if (s.length > 48) s = s.slice(0, 48) + '…';
      return `${k}=${s}`;
    }).join('  ');
  } catch {
    return String(raw).slice(0, 90);
  }
}

/* ---------- approval overlay ---------- */

function showApproval(ev, sessionId) {
  // A created file has no @@ hunk header, so keying on that would render the
  // most common approval as untinted plain text. The --- / +++ pair is what
  // actually marks a unified diff.
  const d = ev.detail || '';
  const isDiff = /^--- /m.test(d) || /^\+\+\+ /m.test(d) || d.includes('@@');
  const body = d
    ? (isDiff ? renderDiff(d) : `<div class="code"><pre>${esc(d)}</pre></div>`)
    : '';

  const panel = el(`<div class="overlay"><div class="overlay-panel">
    <div class="overlay-head">
      <div class="t-meta">${ev.risky ? '<span class="bad">Destructive · approve</span>' : 'Approve'}</div>
      <div class="t-section" style="margin-top:6px">${esc(ev.summary || ev.name)}</div>
      ${ev.path ? `<div class="t-mono dimmer" style="margin-top:4px">${esc(ev.path)}</div>` : ''}
    </div>
    <div class="overlay-body">${body}</div>
    <div class="overlay-foot">
      <button class="btn btn-danger" data-d="abort">Abort run</button>
      <button class="btn btn-ghost" data-d="deny">Skip <span class="kbd">N</span></button>
      <button class="btn btn-ghost" data-d="always">Always allow ${esc(ev.name)}</button>
      <button class="btn btn-primary" data-d="approve">Approve <span class="kbd">Y</span></button>
    </div>
  </div></div>`);

  const send = async decision => {
    closeOverlay();
    try {
      await api(`/api/sessions/${encodeURIComponent(sessionId)}/approve`, {
        method: 'POST', body: JSON.stringify({ id: ev.id, decision }),
      });
    } catch (e) { toast(e.message, true); }
  };

  panel.querySelectorAll('[data-d]').forEach(b => { b.onclick = () => send(b.dataset.d); });

  panel._keys = e => {
    const k = e.key.toLowerCase();
    if (k === 'y') { e.preventDefault(); send('approve'); }
    else if (k === 'n' || k === 'escape') { e.preventDefault(); send('deny'); }
  };
  document.addEventListener('keydown', panel._keys);

  openOverlay(panel);
}

function renderDiff(text) {
  const lines = String(text).split('\n').map(l => {
    let cls = '';
    if (l.startsWith('@@')) cls = 'hunk';
    else if (l.startsWith('---') || l.startsWith('+++')) cls = 'head';
    else if (l.startsWith('+')) cls = 'add';
    else if (l.startsWith('-')) cls = 'del';

    // The tint carries add/remove, so the marker column is dropped. forge's
    // unified output already puts a padded line number after the marker;
    // split it out so it lands in the gutter instead of the code.
    let body = (cls === 'add' || cls === 'del') ? l.slice(1) : l;
    let no = '';
    if (cls === 'add' || cls === 'del') {
      const m = body.match(/^(\s*)(\d+)\s{2}(.*)$/);
      if (m) { no = m[2]; body = m[3]; }
    }
    return `<div class="ln ${cls}"><span class="no">${no}</span>${esc(body) || '&nbsp;'}</div>`;
  });
  return `<div class="code"><pre>${lines.join('')}</pre></div>`;
}

function openOverlay(node) {
  closeOverlay();
  $('#overlay-root').appendChild(node);
}

function closeOverlay() {
  const root = $('#overlay-root');
  [...root.children].forEach(c => {
    if (c._keys) document.removeEventListener('keydown', c._keys);
    c.remove();
  });
}

/* ---------- view: search ---------- */

function viewSearch() {
  const page = el(`<div class="page">
    <div class="page-head">
      <h1 class="t-display">Search</h1>
      <p class="t-body">Hybrid ranking fuses BM25 with vector similarity.</p>
    </div>
    <div class="row" style="gap:10px;margin-bottom:14px">
      <input class="input grow" id="q" placeholder="where is rate limiting handled?" autocomplete="off">
      <select class="select" id="mode" style="width:150px">
        <option value="hybrid">hybrid</option>
        <option value="keyword">keyword</option>
      </select>
      <button class="btn btn-primary" id="run">${icon('search', 16)} Search</button>
    </div>
    <div id="hits"></div>
  </div>`);

  const q = $('#q', page), hits = $('#hits', page);
  setTimeout(() => q.focus(), 30);

  const run = async () => {
    const query = q.value.trim();
    if (!query) return;
    hits.replaceChildren(el(`<div class="empty">${icon('clock', 26)}<div>Searching…</div></div>`));
    try {
      const res = await api(`/api/search?q=${encodeURIComponent(query)}&mode=${$('#mode', page).value}`);
      if (!res.length) {
        hits.replaceChildren(el(`<div class="empty">${icon('search', 30)}<div>No matches.</div></div>`));
        return;
      }
      const max = Math.max(...res.map(r => r.score)) || 1;
      hits.replaceChildren(el(`<div class="card">${res.map(r => `
        <div class="searchhit">
          <div class="spread" style="margin-bottom:8px">
            <span class="t-meta">${esc(r.path)} · ${r.start}-${r.end}${r.symbol ? ' · ' + esc(r.symbol) : ''}</span>
            <span class="row" style="gap:8px;flex:none;width:90px">
              <span class="bar-track"><span class="bar-fill" style="width:${Math.round(r.score / max * 100)}%"></span></span>
            </span>
          </div>
          <div class="code" style="max-height:180px"><pre>${esc(r.text)}</pre></div>
        </div>`).join('')}</div>`));
    } catch (e) {
      showError(hits, e);
    }
  };

  $('#run', page).onclick = run;
  q.addEventListener('keydown', e => { if (e.key === 'Enter') run(); });
  return page;
}

/* ---------- view: repository ---------- */

async function viewRepo() {
  const page = el(`<div class="page">
    <div class="page-head">
      <h1 class="t-display">Repository</h1>
      <p class="t-body">The ranked map the agent sees — PageRank over the reference graph.</p>
    </div>
    <div id="map-body"><div class="empty">${icon('clock', 26)}<div>Building map…</div></div></div>
  </div>`);

  (async () => {
    const body = $('#map-body', page);
    try {
      const m = await api('/api/map?tokens=4000');
      body.replaceChildren(el(`<div class="stack-lg">
        <div class="grid grid-3">
          ${statCard('Files scanned', m.files)}
          ${statCard('Build time', m.ms + 'ms')}
          ${statCard('Map size', nfmt(m.text.length) + ' ch')}
        </div>
        <div class="code" style="max-height:none">
          <div class="code-head"><span class="t-meta">Repository map</span></div>
          <pre>${esc(m.text)}</pre>
        </div>
      </div>`));
    } catch (e) {
      showError(body, e);
    }
  })();

  return page;
}

/* ---------- view: providers ---------- */

async function viewProviders() {
  const page = el(`<div class="page">
    <div class="page-head spread">
      <div>
        <h1 class="t-display">Providers</h1>
        <p class="t-body">Every target the router can fall back to, and why it might not.</p>
      </div>
      <div class="row" style="gap:8px;flex:none">
        <button class="btn btn-ghost" id="probe">${icon('refresh', 16)} Probe</button>
        <button class="btn btn-ghost" id="reset">Clear cooldowns</button>
      </div>
    </div>
    <div id="p-body"></div>
  </div>`);

  const body = $('#p-body', page);

  const load = async probe => {
    body.replaceChildren(el(`<div class="empty">${icon('clock', 26)}<div>${probe ? 'Probing providers…' : 'Loading…'}</div></div>`));
    try {
      const list = await api('/api/providers' + (probe ? '?probe=1' : ''));
      const b = State.boot || {};
      body.replaceChildren(el(`<div class="stack-lg">
        <div class="card" style="padding:0;overflow:hidden">
          ${list.map(p => `<div class="list-row">
            <span class="dot ${p.cooldownSec ? 'warn' : (!p.configured ? '' : (p.ok ? 'ok' : (p.detail ? 'err' : '')))}"></span>
            <span class="col grow" style="gap:3px;min-width:0">
              <span class="row" style="gap:8px"><b>${esc(p.name)}</b>
                ${!p.enabled ? '<span class="chip">disabled</span>' : ''}
                ${!p.configured ? '<span class="chip warn">no key</span>' : ''}
                ${p.models ? `<span class="chip ok">${p.models} models</span>` : ''}
                ${p.cooldownSec ? `<span class="chip warn">cooling ${p.cooldownSec}s</span>` : ''}
              </span>
              <span class="t-mono dimmer truncate">${esc(p.detail || p.note || p.baseUrl)}</span>
            </span>
            ${p.ms ? `<span class="t-mono dimmer" style="flex:none">${p.ms}ms</span>` : ''}
          </div>`).join('')}
        </div>
        ${renderClasses(b)}
      </div>`));
    } catch (e) {
      showError(body, e);
    }
  };

  $('#probe', page).onclick = () => load(true);
  $('#reset', page).onclick = async () => {
    try { await api('/api/providers/reset', { method: 'POST' }); toast('Cooldowns cleared'); load(false); }
    catch (e) { toast(e.message, true); }
  };

  load(false);
  return page;
}

function renderClasses(b) {
  const classes = b.classes || {};
  const names = b.classNames || [];
  if (!names.length) return '';
  return `<div>
    <div class="t-meta" style="margin-bottom:12px">Class chains — tried top to bottom</div>
    <div class="stack">
      ${names.map(n => `<div class="card tight">
        <div class="spread" style="margin-bottom:10px">
          <b>${esc(n)}</b>
          ${n === b.defaultClass ? '<span class="chip info">default</span>' : ''}
        </div>
        ${(classes[n] || []).map((t, i) => `<div class="row t-mono" style="gap:10px;padding:4px 0">
          <span class="dimmer" style="width:16px">${i + 1}.</span>
          <span class="dot ${t.cooldownSec ? 'warn' : 'ok'}"></span>
          <span style="width:96px" class="truncate">${esc(t.provider)}</span>
          <span class="grow truncate">${esc(t.model)}</span>
          <span class="dimmer" style="flex:none">${nfmt(t.maxContext)} ctx</span>
          ${t.cooldownSec ? `<span class="chip warn">${t.cooldownSec}s</span>` : ''}
        </div>`).join('')}
      </div>`).join('')}
    </div>
  </div>`;
}

/* ---------- view: verification ---------- */

function viewVerify() {
  const b = State.boot || {};
  const page = el(`<div class="page">
    <div class="page-head spread">
      <div>
        <h1 class="t-display">Verification</h1>
        <p class="t-body">The project's own checks. A model's confidence is not evidence.</p>
      </div>
      <button class="btn btn-primary" id="run" style="flex:none">${icon('check', 16)} Run checks</button>
    </div>
    <div class="card tight" style="margin-bottom:18px">
      <div class="t-meta" style="margin-bottom:8px">Detected checks</div>
      ${(b.verifyChecks || []).length
        ? (b.verifyChecks).map(c => `<div class="t-mono" style="padding:3px 0">${esc(c)}</div>`).join('')
        : '<div class="t-sm dimmer">None detected for this workspace.</div>'}
    </div>
    <div id="v-body"></div>
  </div>`);

  $('#run', page).onclick = async () => {
    const body = $('#v-body', page);
    body.replaceChildren(el(`<div class="empty">${icon('clock', 26)}<div>Running…</div></div>`));
    try {
      const v = await api('/api/verify', { method: 'POST' });
      body.replaceChildren(el(`<div class="stack">
        <div class="card tight spread">
          <span class="row" style="gap:10px"><span class="dot ${v.passed ? 'ok' : 'err'}"></span>
          <b>${v.passed ? 'All checks passed' : 'Checks failed'}</b></span>
          <span class="t-mono dimmer">${esc(v.summary || '')}</span>
        </div>
        ${(v.checks || []).map(c => `<div class="card tight">
          <div class="spread" style="margin-bottom:${c.output ? '10px' : '0'}">
            <span class="row" style="gap:10px"><span class="dot ${c.passed ? 'ok' : 'err'}"></span>
            <span class="t-mono">${esc(c.command)}</span></span>
            <span class="t-mono dimmer">${c.ms}ms</span>
          </div>
          ${c.output ? `<div class="code" style="max-height:260px"><pre>${esc(c.output)}</pre></div>` : ''}
        </div>`).join('')}
      </div>`));
    } catch (e) {
      showError(body, e);
    }
  };
  return page;
}

/* ---------- view: usage ---------- */

async function viewUsage() {
  const u = await api('/api/usage');
  const total = u.totalPrompt + u.totalCompletion;
  const maxDay = Math.max(1, ...(u.byDay || []).map(d => d.tokens));

  return el(`<div class="page">
    <div class="page-head">
      <h1 class="t-display">Usage</h1>
      <p class="t-body">Every call the router has made, from the local ledger.</p>
    </div>
    <div class="grid grid-3" style="margin-bottom:28px">
      ${statCard('Total tokens', nfmt(total))}
      ${statCard('Prompt', nfmt(u.totalPrompt))}
      ${statCard('Completion', nfmt(u.totalCompletion))}
    </div>

    ${(u.byDay || []).length ? `<div class="card" style="margin-bottom:22px">
      <div class="t-meta" style="margin-bottom:16px">Tokens per day</div>
      <div class="row" style="gap:6px;align-items:flex-end;height:120px">
        ${u.byDay.map(d => `<div class="col grow" style="gap:6px;align-items:center;justify-content:flex-end;height:100%">
          <div style="width:100%;background:var(--primary);border-radius:3px;height:${Math.max(2, d.tokens / maxDay * 100)}%"
               title="${esc(d.day)}: ${nfmt(d.tokens)}"></div>
          <span class="t-mono dimmer" style="font-size:10px">${esc(d.day.slice(5))}</span>
        </div>`).join('')}
      </div>
    </div>` : ''}

    ${(u.byModel || []).length ? `<div class="card" style="padding:0;overflow:hidden;margin-bottom:22px">
      ${u.byModel.map(m => `<div class="list-row">
        <span class="col grow" style="gap:3px;min-width:0">
          <span class="t-mono truncate">${esc(m.provider)} / ${esc(m.model)}</span>
          <span class="t-sm dimmer">${m.calls} calls</span>
        </span>
        <span class="t-mono dimmer" style="flex:none">${nfmt(m.prompt)} in</span>
        <span class="t-mono accent" style="flex:none">${nfmt(m.completion)} out</span>
      </div>`).join('')}
    </div>` : `<div class="card empty">${icon('chart', 30)}<div>No usage recorded yet.</div></div>`}

    ${(u.recent || []).length ? `<div>
      <div class="t-meta" style="margin-bottom:12px">Recent calls</div>
      <div class="card" style="padding:0;overflow:hidden">
        ${u.recent.map(c => `<div class="list-row">
          <span class="dot ${c.error ? 'err' : 'ok'}"></span>
          <span class="t-mono truncate grow">${esc(c.model)}</span>
          <span class="t-mono dimmer" style="flex:none">${nfmt(c.prompt)}+${nfmt(c.completion)}</span>
          <span class="t-mono dimmer" style="flex:none">${c.ms}ms</span>
          <span class="t-mono dimmer" style="flex:none">${ago(c.t)}</span>
        </div>`).join('')}
      </div>
    </div>` : ''}
  </div>`);
}

/* ---------- view: settings ---------- */

function viewSettings() {
  const b = State.boot || {};
  const standalone = window.matchMedia('(display-mode: standalone)').matches;
  const page = el(`<div class="page">
    <div class="page-head">
      <h1 class="t-display">Settings</h1>
      <p class="t-body">Read from disk. Edit the config file and restart to change them.</p>
    </div>
    <div class="card tight spread" style="margin-bottom:18px">
      <div class="col" style="gap:3px">
        <b>${standalone ? 'Installed as an app' : 'Install as a desktop app'}</b>
        <span class="t-sm dimmer">${standalone
          ? 'Running in its own window.'
          : 'Adds FORGE to your Start Menu, Dock, or home screen with its own window.'}</span>
      </div>
      <button class="btn btn-primary" id="install-btn" style="display:${installPrompt ? '' : 'none'}">Install</button>
    </div>
    <div class="stack">
      <div class="card tight">${sideRow('Version', b.version || 'dev')}</div>
      <div class="card tight">${sideRow('Config', b.configPath || '')}</div>
      <div class="card tight">${sideRow('State dir', b.stateDir || '')}</div>
      <div class="card tight">${sideRow('Workspace', b.workspace || '')}</div>
      <div class="card tight">${sideRow('Default class', b.defaultClass || '')}</div>
      <div class="card tight">${sideRow('Embedder', b.embedder || 'none')}</div>
    </div>
    <div class="card" style="margin-top:22px">
      <div class="t-meta" style="margin-bottom:10px">Keyboard</div>
      <div class="stack" style="gap:8px">
        ${sideRow('Command palette', '⌘K / Ctrl+K')}
        ${sideRow('New session', 'N')}
        ${sideRow('Approve / skip', 'Y / N')}
        ${sideRow('Submit task', '⌘⏎')}
      </div>
    </div>
  </div>`);

  const btn = $('#install-btn', page);
  btn.onclick = async () => {
    if (!installPrompt) return;
    installPrompt.prompt();
    const { outcome } = await installPrompt.userChoice;
    // The prompt is single use; a declined one cannot be replayed.
    installPrompt = null;
    btn.style.display = 'none';
    toast(outcome === 'accepted' ? 'FORGE installed' : 'Install dismissed');
  };
  return page;
}

/* ---------- command palette ---------- */

function openPalette() {
  const actions = [
    ...ROUTES.map(r => ({ label: r.label, ctx: 'Navigate', run: () => go(r.id) })),
    ...State.sessions.slice(0, 8).map(s => ({
      label: s.task, ctx: 'Session · ' + s.status, run: () => go('session/' + s.id),
    })),
  ];

  const panel = el(`<div class="overlay" style="align-items:flex-start">
    <div class="overlay-panel palette">
      <input class="palette-input" placeholder="Jump to…" autocomplete="off" spellcheck="false">
      <div class="hairline"></div>
      <div class="palette-list"></div>
      <div class="overlay-foot" style="justify-content:flex-start;gap:14px">
        <span class="t-sm dimmer"><span class="kbd">↑↓</span> navigate</span>
        <span class="t-sm dimmer"><span class="kbd">⏎</span> select</span>
        <span class="t-sm dimmer"><span class="kbd">esc</span> close</span>
      </div>
    </div></div>`);

  const input = $('.palette-input', panel), list = $('.palette-list', panel);
  let filtered = actions, sel = 0;

  const draw = () => {
    list.replaceChildren(...filtered.map((a, i) => {
      const row = el(`<button class="palette-row${i === sel ? ' sel' : ''}">
        <span class="truncate">${esc(a.label)}</span>
        <span class="ctx">${esc(a.ctx)}</span></button>`);
      row.onclick = () => { closeOverlay(); a.run(); };
      return row;
    }));
    const cur = list.children[sel];
    if (cur) cur.scrollIntoView({ block: 'nearest' });
  };

  input.oninput = () => {
    const q = input.value.toLowerCase();
    filtered = actions.filter(a => (a.label + ' ' + a.ctx).toLowerCase().includes(q));
    sel = 0;
    draw();
  };

  panel._keys = e => {
    if (e.key === 'Escape') { e.preventDefault(); closeOverlay(); }
    else if (e.key === 'ArrowDown') { e.preventDefault(); sel = Math.min(sel + 1, filtered.length - 1); draw(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); sel = Math.max(sel - 1, 0); draw(); }
    else if (e.key === 'Enter') {
      e.preventDefault();
      const a = filtered[sel];
      if (a) { closeOverlay(); a.run(); }
    }
  };
  document.addEventListener('keydown', panel._keys);

  openOverlay(panel);
  draw();
  input.focus();
}

/* ---------- boot ---------- */

function isTyping() {
  const t = document.activeElement;
  return t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable);
}

document.addEventListener('keydown', e => {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault();
    openPalette();
    return;
  }
  if (isTyping() || $('#overlay-root').children.length) return;
  if (e.key.toLowerCase() === 'n') { e.preventDefault(); go('new'); }
});

$('#palette-btn').onclick = openPalette;
window.addEventListener('hashchange', render);

// The elapsed clock ticks locally so a running session does not look frozen
// between model calls, which on a local model can be minutes apart.
setInterval(() => {
  const live = State.live;
  if (!live || !live.session || live.session.status !== 'running') return;
  const e = document.getElementById('s-elapsed');
  if (e) e.textContent = dur(Date.now() - live.session.created);
}, 1000);

// Register the shell worker so the app is installable and paints while the Go
// process is still binding its port. Failure is not worth surfacing — it only
// costs the offline shell, and the app itself works either way.
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch(() => {});
  });
}

// Chromium fires this instead of showing its own install affordance. Holding
// the event lets Settings offer a real button rather than telling the user to
// hunt through a browser menu.
let installPrompt = null;
window.addEventListener('beforeinstallprompt', e => {
  e.preventDefault();
  installPrompt = e;
  const btn = document.getElementById('install-btn');
  if (btn) btn.style.display = '';
});

(async function main() {
  try {
    State.boot = await api('/api/bootstrap');
    State.workspace = State.boot.workspace || '';
    $('#foot-model').textContent = State.boot.defaultClass || 'no class';
    $('#foot-dot').className = 'dot ' + ((State.boot.providers || []).some(p => p.configured) ? 'ok' : 'warn');
  } catch (e) {
    if (isOffline(e)) {
      // Nothing else will work either; show the one screen that helps rather
      // than a chrome full of controls wired to a server that is not there.
      $('#foot-model').textContent = 'not running';
      $('#foot-dot').className = 'dot err';
      $('#view').replaceChildren(serverGoneScreen());
      return;
    }
    toast('Could not reach the forge server: ' + e.message, true);
  }
  try { State.sessions = await api('/api/sessions'); } catch { /* first paint can proceed */ }
  if (!location.hash) location.hash = '#/sessions';
  render();
})();
