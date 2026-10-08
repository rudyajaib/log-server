'use strict';

/**
 * OSLog Live front end.
 *
 * Three ideas drive the design:
 *
 *  1. Filtering happens on the server against the whole buffer, so changing a
 *     filter must not throw the current view away. A new filter opens a new
 *     event stream and only replaces the rows once the new snapshot arrives, so
 *     the log never flashes empty.
 *
 *  2. The facet counts and the "N match" figure come from the same snapshot as
 *     the rows, so the pickers can never disagree with what is on screen.
 *
 *  3. A live stream is far deeper than one page of rows. "Load older" pages
 *     backwards through the server buffer and then into the on-disk history,
 *     which is what makes a subsystem that logs once every fifteen minutes
 *     findable at all.
 */

const RENDER_WINDOW = 800;  // rows kept in the DOM while following the tail
const RENDER_EXTEND = 500;  // older rows revealed per scroll-back step
const MAX_LIVE = 5000;      // rows kept in memory while following the live tail
const HARD_MAX = 20000;     // absolute ceiling on loaded rows when paging history
const PAGE_SIZE = 1000;     // entries fetched per "load older"
const FACET_REFRESH_MS = 5000;
const STATUS_REFRESH_MS = 2000;
const TOP_TRIGGER = 120;

const KNOWN_LEVELS = ['Error', 'Fault', 'Default', 'Info', 'Debug', 'Signpost', 'Activity', 'Loss', 'Timesync', 'State', 'Trace'];

const state = {
  targets: { simulators: [], devices: [], errors: {} },
  status: null,
  epoch: null,
  applying: false,

  rows: [],               // every loaded entry, oldest first
  windowStart: 0,         // index of the first row currently in the DOM
  inbox: [],              // entries queued for the next animation frame
  lastId: 0,
  selectedId: null,
  newCount: 0,            // entries that arrived while the user was scrolled back

  paused: false,
  pending: [],
  autoScroll: true,
  newestFirst: false,

  snapshot: { matched: 0, truncated: false },
  matched: 0,             // matches in the server buffer, kept live and corrected by polls
  hasMoreOlder: false,
  loadingOlder: false,
  clientDropped: 0,
  pendingDropped: 0,

  levelFacets: new Map(),
  filterDebounce: 0,

  filters: {
    subsystems: new Set(),
    categories: new Set(),
    processes: new Set(),
    levels: new Set(),
    query: '',
    regex: false,
    caseSensitive: false,
    requireSubsystem: false,
    predicate: '',
    targetKey: '',
    level: 'default',
  },
};

const dom = {};
for (const id of [
  'target-select', 'refresh-targets', 'boot-simulator', 'level-select', 'capture-toggle',
  'status-dot', 'status-text', 'stat-rate', 'stat-count', 'stat-buffer', 'stat-clients',
  'banner', 'banner-text', 'banner-close', 'advanced-toggle', 'advanced-row', 'predicate',
  'subsystem-filter', 'category-filter', 'process-filter', 'level-filter',
  'query', 'regex-toggle', 'case-toggle', 'require-subsystem',
  'pause-toggle', 'autoscroll-toggle', 'sort-toggle', 'clear-view',
  'log-scroll', 'log-head', 'log-rows', 'empty-state', 'jump-latest', 'load-older',
  'visible-count', 'match-count', 'buffer-note', 'history-note', 'history-clear', 'drop-note',
  'copy-json', 'copy-link', 'detail', 'detail-body', 'detail-copy', 'detail-close',
]) {
  dom[camel(id)] = document.getElementById(id);
}

function camel(id) {
  return id.replace(/-([a-z])/g, (_, c) => c.toUpperCase());
}

/* ------------------------------------------------------------- utilities -- */

function setText(node, text) {
  if (node && node.textContent !== text) node.textContent = text;
}

function api(path) {
  return fetch(path, { headers: { Accept: 'application/json' } });
}

function debounce(fn, ms) {
  return (...args) => {
    clearTimeout(state.filterDebounce);
    state.filterDebounce = setTimeout(() => fn(...args), ms);
  };
}

function formatTime(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  const pad = (n, w = 2) => String(n).padStart(w, '0');
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`;
}

function escapeRegex(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

function formatBytes(bytes) {
  if (!bytes) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value < 10 && unit > 0 ? value.toFixed(1) : Math.round(value)} ${units[unit]}`;
}

function formatSpan(fromISO, toISO) {
  if (!fromISO || !toISO) return '';
  const seconds = (new Date(toISO) - new Date(fromISO)) / 1000;
  if (!Number.isFinite(seconds) || seconds < 0) return '';
  if (seconds < 90) return `${seconds.toFixed(0)}s`;
  if (seconds < 5400) return `${(seconds / 60).toFixed(1)}m`;
  return `${(seconds / 3600).toFixed(1)}h`;
}

/* -------------------------------------------------- filters <-> URL/local -- */

function filterParams() {
  const params = new URLSearchParams();
  const f = state.filters;
  for (const v of f.subsystems) params.append('subsystem', v);
  for (const v of f.categories) params.append('category', v);
  for (const v of f.processes) params.append('process', v);
  for (const v of f.levels) params.append('level', v);
  if (f.query.trim()) {
    params.set('q', f.query.trim());
    if (f.regex) params.set('regex', 'true');
    if (f.caseSensitive) params.set('case', 'true');
  }
  if (f.requireSubsystem) params.set('hasSubsystem', 'true');
  return params;
}

function syncURL() {
  const params = filterParams();
  const url = params.toString() ? `${location.pathname}?${params}` : location.pathname;
  history.replaceState(null, '', url);
}

function filtersFromURL() {
  const params = new URLSearchParams(location.search);
  const f = state.filters;
  f.subsystems = new Set(params.getAll('subsystem'));
  f.categories = new Set(params.getAll('category'));
  f.processes = new Set(params.getAll('process'));
  f.levels = new Set(params.getAll('level'));
  f.query = params.get('q') || '';
  f.regex = params.get('regex') === 'true';
  f.caseSensitive = params.get('case') === 'true';
  f.requireSubsystem = params.get('hasSubsystem') === 'true';
}

function loadPrefs() {
  try {
    const raw = localStorage.getItem('oslog-live.prefs');
    if (!raw) return;
    const saved = JSON.parse(raw);
    if (saved.level) state.filters.level = saved.level;
    if (saved.predicate) state.filters.predicate = saved.predicate;
    if (typeof saved.newestFirst === 'boolean') state.newestFirst = saved.newestFirst;
    if (typeof saved.autoScroll === 'boolean') state.autoScroll = saved.autoScroll;
    if (saved.targetKey) state.filters.targetKey = saved.targetKey;
  } catch { /* ignore corrupt prefs */ }
}

function savePrefs() {
  try {
    localStorage.setItem('oslog-live.prefs', JSON.stringify({
      level: state.filters.level,
      predicate: state.filters.predicate,
      newestFirst: state.newestFirst,
      autoScroll: state.autoScroll,
      targetKey: state.filters.targetKey,
    }));
  } catch { /* storage may be unavailable */ }
}

/* ------------------------------------------------------------ multiselect -- */

class MultiSelect {
  constructor(root, label, dimension, placeholder) {
    this.root = root;
    this.label = label;
    this.dimension = dimension;
    this.placeholder = placeholder;
    this.selected = new Set();
    this.facets = [];
    this.open = false;
    this.filterText = '';
    this.build();
  }

  currentSet() {
    return state.filters[this.dimension];
  }

  syncFromState() {
    this.selected = this.currentSet();
    this.renderButton();
  }

  build() {
    this.root.innerHTML = '';
    this.button = document.createElement('button');
    this.button.type = 'button';
    this.button.className = 'ms-button';
    this.button.setAttribute('aria-haspopup', 'listbox');
    this.button.setAttribute('aria-expanded', 'false');

    this.labelEl = document.createElement('span');
    this.labelEl.className = 'ms-label';
    this.labelEl.textContent = this.label;
    this.valueEl = document.createElement('span');
    this.valueEl.className = 'ms-value';
    this.caret = document.createElement('span');
    this.caret.className = 'caret';
    this.caret.textContent = '▾';
    this.button.append(this.labelEl, this.valueEl, this.caret);

    this.button.addEventListener('click', (ev) => {
      ev.stopPropagation();
      this.toggle();
    });

    this.popover = document.createElement('div');
    this.popover.className = 'ms-popover';
    this.popover.hidden = true;

    const head = document.createElement('div');
    head.className = 'ms-head';
    this.search = document.createElement('input');
    this.search.type = 'search';
    this.search.placeholder = 'Filter values…';
    this.search.addEventListener('input', () => {
      this.filterText = this.search.value.trim().toLowerCase();
      this.renderList();
    });
    this.search.addEventListener('click', (ev) => ev.stopPropagation());

    const allBtn = document.createElement('button');
    allBtn.type = 'button';
    allBtn.textContent = 'All';
    allBtn.addEventListener('click', () => this.setAll(true));
    const noneBtn = document.createElement('button');
    noneBtn.type = 'button';
    noneBtn.textContent = 'None';
    noneBtn.addEventListener('click', () => this.setAll(false));
    head.append(this.search, allBtn, noneBtn);

    this.list = document.createElement('div');
    this.list.className = 'ms-list';
    this.list.setAttribute('role', 'listbox');
    this.list.addEventListener('click', (ev) => ev.stopPropagation());

    this.popover.append(head, this.list);
    this.root.append(this.button, this.popover);

    this.renderButton();
    this.renderList();
  }

  toggle() {
    this.open = !this.open;
    this.popover.hidden = !this.open;
    this.button.setAttribute('aria-expanded', String(this.open));
    if (this.open) {
      closeOtherPopovers(this);
      // Always show the current values rather than whatever was rendered last.
      this.renderList();
    }
  }

  close() {
    this.open = false;
    this.popover.hidden = true;
    this.button.setAttribute('aria-expanded', 'false');
  }

  setAll(on) {
    if (on) {
      this.facets.forEach((f) => this.selected.add(f.value));
    } else {
      this.selected.clear();
    }
    this.commit();
    this.renderList();
  }

  setFacets(facets) {
    this.facets = facets || [];
    this.renderButton();
    // Keep the rendered list in step with the data. The earlier "only refresh
    // while open" guard meant a value that appeared after the picker was last
    // opened never showed up, so the list could contradict the table.
    this.renderList();
  }

  renderButton() {
    const n = this.selected.size;
    this.button.classList.toggle('active', n > 0);
    if (n === 0) {
      this.valueEl.textContent = this.placeholder;
      this.valueEl.style.color = '';
    } else if (n === 1) {
      this.valueEl.textContent = [...this.selected][0];
      this.valueEl.style.color = '';
    } else {
      this.valueEl.textContent = `${n} selected`;
      this.valueEl.style.color = 'var(--accent)';
    }
  }

  renderList() {
    // Rebuilding must not jump the list or steal a click that is in progress.
    const previousScroll = this.list.scrollTop;
    this.list.innerHTML = '';
    const rows = this.facets.filter((f) => !this.filterText || f.value.toLowerCase().includes(this.filterText));
    const custom = [...this.selected].filter((v) => !this.facets.some((f) => f.value === v));

    const add = (value, count, isCustom) => {
      const option = document.createElement('label');
      option.className = 'ms-option' + (isCustom ? ' custom' : '');
      const box = document.createElement('input');
      box.type = 'checkbox';
      box.checked = this.selected.has(value);
      box.addEventListener('change', () => {
        if (box.checked) this.selected.add(value);
        else this.selected.delete(value);
        this.commit();
        this.renderButton();
      });
      const valueEl = document.createElement('span');
      valueEl.className = 'ms-opt-value';
      valueEl.textContent = value;
      valueEl.title = value;
      option.append(box, valueEl);
      if (typeof count === 'number') {
        const countEl = document.createElement('span');
        countEl.className = 'ms-opt-count';
        countEl.textContent = String(count);
        option.append(countEl);
      }
      this.list.append(option);
    };

    for (const value of custom) add(value, undefined, true);
    for (const facet of rows) add(facet.value, facet.count, false);

    if (!this.list.childElementCount) {
      const empty = document.createElement('div');
      empty.className = 'ms-empty';
      empty.textContent = this.facets.length ? 'No values match' : 'No values seen yet';
      this.list.append(empty);
    }
    this.list.scrollTop = previousScroll;
  }

  commit() {
    this.syncFromState();
    scheduleFilterChange();
  }
}

const selects = [];

function closeOtherPopovers(keep) {
  selects.forEach((s) => {
    if (s !== keep) s.close();
  });
}

/* ---------------------------------------------------------------- capture -- */

function targetKey(t) {
  return `${t.kind}:${t.id}`;
}

function findTarget(key) {
  const all = [...state.targets.simulators, ...state.targets.devices];
  return all.find((t) => targetKey(t) === key) || null;
}

async function fetchTargets() {
  try {
    const res = await api('/api/targets');
    const data = await res.json();
    state.targets = {
      simulators: data.simulators || [],
      devices: data.devices || [],
      errors: data.errors || {},
    };
    renderTargets();
  } catch (err) {
    showBanner(`Could not list log sources: ${err.message}`, 'error');
  }
}

function renderTargets() {
  const select = dom.targetSelect;
  const keep = state.filters.targetKey || select.value;
  select.innerHTML = '';

  const groups = [
    ['Simulators', state.targets.simulators],
    ['Connected devices', state.targets.devices],
  ];

  let firstReady = null;
  for (const [label, list] of groups) {
    if (!list.length) continue;
    const group = document.createElement('optgroup');
    group.label = label;
    for (const t of list) {
      const option = document.createElement('option');
      option.value = targetKey(t);
      const bits = [t.name];
      if (t.kind === 'simulator' && t.runtime) bits.push(t.runtime);
      if (t.kind === 'device' && t.osVersion) bits.push(`iOS ${t.osVersion}`);
      bits.push(t.ready ? `(${t.state || 'ready'})` : '— not ready');
      option.textContent = bits.join(' · ');
      group.append(option);
      if (t.ready && !firstReady) firstReady = targetKey(t);
    }
    select.append(group);
  }

  if (state.targets.errors.devices) {
    const group = document.createElement('optgroup');
    group.label = 'Connected devices';
    const option = document.createElement('option');
    option.value = '';
    option.disabled = true;
    option.textContent = 'unavailable — Xcode devicectl not found';
    group.append(option);
    select.append(group);
  }

  const candidates = [keep, firstReady].filter(Boolean);
  const chosen = candidates.find((key) => findTarget(key));
  if (chosen) select.value = chosen;
  state.filters.targetKey = select.value;

  const target = findTarget(select.value);
  const canBoot = target && target.kind === 'simulator' && !target.ready;
  dom.bootSimulator.hidden = !canBoot;
  dom.captureToggle.disabled = !target;
}

async function toggleCapture() {
  if (state.status && state.status.running) {
    dom.captureToggle.disabled = true;
    try {
      await fetch('/api/stream/stop', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
    } catch (err) {
      showBanner(`Could not stop the capture: ${err.message}`, 'error');
    } finally {
      dom.captureToggle.disabled = false;
    }
    return;
  }

  const target = findTarget(state.filters.targetKey);
  if (!target) {
    showBanner('Select a log source first.', 'error');
    return;
  }
  if (!target.ready && target.kind === 'simulator') {
    showBanner('This simulator is not booted. Press Boot, then start the capture.', 'error');
    return;
  }

  dom.captureToggle.disabled = true;
  try {
    const res = await fetch('/api/stream/start', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        kind: target.kind,
        id: target.id,
        name: target.name,
        runtime: target.runtime || '',
        model: target.model || '',
        level: state.filters.level,
        predicate: state.filters.predicate,
      }),
    });
    const data = await res.json();
    if (!res.ok) {
      showBanner(data.error || 'Could not start the capture.', 'error');
      return;
    }
    resetView();
    applyStatus(data, true);
  } catch (err) {
    showBanner(`Could not start the capture: ${err.message}`, 'error');
  } finally {
    dom.captureToggle.disabled = false;
  }
}

async function bootSelectedSimulator() {
  const target = findTarget(state.filters.targetKey);
  if (!target) return;
  dom.bootSimulator.disabled = true;
  try {
    const res = await fetch('/api/simulators/boot', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: target.id }),
    });
    const data = await res.json();
    if (!res.ok) showBanner(data.error || 'Could not boot the simulator.', 'error');
    else showBanner(`Booting ${target.name}…`, 'info');
  } catch (err) {
    showBanner(`Could not boot the simulator: ${err.message}`, 'error');
  } finally {
    setTimeout(() => {
      dom.bootSimulator.disabled = false;
      fetchTargets();
    }, 4000);
  }
}

async function clearHistory() {
  try {
    const res = await fetch('/api/history/clear', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
    if (!res.ok) {
      const data = await res.json().catch(() => ({}));
      throw new Error(data.error || `HTTP ${res.status}`);
    }
    showBanner('History cleared; recording resumed.', 'info', true);
    await pollStatus();
  } catch (err) {
    showBanner(`Could not clear the history: ${err.message}`, 'error');
  }
}

/* ------------------------------------------------------------------ status -- */

function applyStatus(status, skipEpochCheck) {
  state.status = status;

  const dotClass = status.state === 'running' ? 'running'
    : status.state === 'restarting' ? 'restarting'
    : status.state === 'error' ? 'error' : 'idle';
  dom.statusDot.className = `dot ${dotClass}`;

  const running = !!status.running;
  let text = 'Idle';
  if (status.state === 'running') text = `Capturing · ${status.target?.name || ''} (level ${status.level})`;
  else if (status.state === 'restarting') text = `Restarting · ${status.target?.name || ''}`;
  else if (status.state === 'error') text = 'Stopped with an error';
  else if (status.total > 0) text = 'Stopped';
  setText(dom.statusText, text);

  setText(dom.statRate, `${(status.rate || 0).toFixed(1)}/s`);
  setText(dom.statCount, `${Number(status.received || 0).toLocaleString()} received`);
  setText(dom.statBuffer, `buffer ${Number(status.buffered || 0).toLocaleString()}/${Number(status.capacity || 0).toLocaleString()}`);
  setText(dom.statClients, `${status.clients || 0} viewer${status.clients === 1 ? '' : 's'}`);

  dom.captureToggle.textContent = running ? 'Stop capture' : 'Start capture';
  dom.captureToggle.classList.toggle('running', running);

  if (status.lastError) {
    showBanner(status.lastError, status.state === 'error' ? 'error' : 'info', status.state !== 'error');
  } else if (!running && status.deviceNotice && state.targets.devices.length) {
    showBanner(status.deviceNotice, 'info', true);
  }

  renderNotes();

  if (!skipEpochCheck && state.epoch !== null && status.epoch !== state.epoch) {
    state.epoch = status.epoch;
    openStream();
  } else if (state.epoch === null) {
    state.epoch = status.epoch;
  }
}

function renderNotes() {
  const status = state.status;
  if (!status) return;

  const span = formatSpan(status.bufferFrom, status.bufferTo);
  let bufferText = '';
  if (status.buffered) {
    bufferText = span ? `· buffer covers ≈${span}` : '';
    if (status.evicted) bufferText += ` (${status.evicted.toLocaleString()} evicted)`;
  }
  setText(dom.bufferNote, bufferText);

  const history = status.history || {};
  const enabled = !!history.enabled;
  dom.historyClear.hidden = !enabled;
  if (enabled) {
    setText(dom.historyNote, `· history ${Number(history.entries || 0).toLocaleString()} entries (${formatBytes(history.bytes)})`);
    dom.historyNote.title = `${history.path}\nScrolling to the top, or "Load older", reads entries the buffer has already evicted.`;
    if (history.full) {
      dom.dropNote.hidden = false;
      setText(dom.dropNote, `history recording stopped at ${formatBytes(history.maxBytes)} — clear it to resume`);
      return;
    }
  } else {
    setText(dom.historyNote, '· history off');
    dom.historyNote.title = 'Start with -history to keep entries beyond the in-memory buffer.';
  }
  dom.dropNote.hidden = state.clientDropped + state.pendingDropped === 0;
}

function showBanner(message, kind, dismissable) {
  if (!message) {
    dom.banner.hidden = true;
    return;
  }
  dom.bannerText.textContent = message;
  dom.banner.className = `banner ${kind === 'info' ? 'info' : ''}`;
  dom.banner.hidden = false;
  dom.bannerClose.hidden = !dismissable;
}

/* -------------------------------------------------------------- SSE stream -- */

let source = null;

function openStream({ reset } = {}) {
  if (source) {
    source.close();
    source = null;
  }
  if (reset) resetView();

  const params = filterParams();
  params.set('snapshot', String(MAX_LIVE));
  const es = new EventSource(`/api/stream?${params}`);
  source = es;

  setApplying(true);

  es.addEventListener('snapshot', (ev) => {
    const payload = JSON.parse(ev.data);
    state.epoch = payload.status?.epoch ?? state.epoch;
    state.snapshot = { matched: payload.matched, truncated: payload.truncated };
    state.matched = payload.matched;
    state.hasMoreOlder = !!payload.older;
    if (payload.facets) applyFacets(payload.facets);
    setApplying(false);

    if (payload.mode === 'full') {
      // Replace the loaded set and swap the DOM once: the previous filter's rows
      // stay on screen until the new ones are ready to take their place.
      state.rows = payload.entries || [];
      state.inbox.length = 0;
      state.lastId = state.rows.length ? state.rows[state.rows.length - 1].id : 0;
      state.newCount = 0;
      state.windowStart = tailWindowStart();
      renderWindow();
      scrollToLatest(true);
    } else {
      queueEntries(payload.entries || []);
    }
    renderCounts();
  });

  es.addEventListener('log', (ev) => {
    queueEntries(JSON.parse(ev.data).entries || []);
  });

  es.addEventListener('status', (ev) => {
    applyStatus(JSON.parse(ev.data));
  });

  es.addEventListener('warning', (ev) => {
    state.clientDropped += JSON.parse(ev.data).dropped || 0;
    renderCounts();
  });

  // EventSource reports CLOSED when it has given up, which happens if the server
  // rejects the request. Surface it rather than leaving an empty view behind.
  es.addEventListener('error', () => {
    if (es.readyState === EventSource.CLOSED) {
      setApplying(false);
      showBanner('The server rejected the filter, or the stream stopped. Fix the filter and try again.', 'error', true);
    }
  });
}

function setApplying(on) {
  state.applying = on;
  dom.logScroll.parentElement.classList.toggle('updating', on);
  if (on) {
    setText(dom.visibleCount, 'applying filter…');
    setText(dom.matchCount, '');
  } else {
    renderCounts();
  }
}

/* ---------------------------------------------------------------- render -- */

function resetView() {
  state.rows = [];
  state.lastId = 0;
  state.windowStart = 0;
  state.inbox.length = 0;
  state.newCount = 0;
  state.pending = [];
  state.selectedId = null;
  state.snapshot = { matched: 0, truncated: false };
  state.matched = 0;
  dom.logRows.innerHTML = '';
  dom.detail.hidden = true;
  renderCounts();
}

// isFollowing reports whether the view tracks the live tail. In newest-first
// mode the tail is at the top of the scroller, but "following" is still true:
// appendLiveRows and scrollToLatest already know which end that is.
function isFollowing() {
  return state.autoScroll;
}

// atOlderEdge reports whether the scroller is at the end where older entries
// arrive: the top in chronological order, the bottom when newest first.
function atOlderEdge() {
  const node = dom.logScroll;
  if (state.newestFirst) {
    return node.scrollHeight - node.scrollTop - node.clientHeight <= TOP_TRIGGER;
  }
  return node.scrollTop <= TOP_TRIGGER;
}

// With a busy stream the loaded set quickly outgrows what any page should hold in
// the DOM, so the two are decoupled: state.rows keeps every loaded entry, while
// the DOM holds the window state.rows[windowStart:]. Old rows are revealed on
// demand instead of being rendered up front, which keeps appends and reflows
// cheap no matter how fast entries arrive.
function renderWindow() {
  const end = state.rows.length;
  state.windowStart = Math.min(Math.max(0, state.windowStart), end);
  const slice = state.rows.slice(state.windowStart, end);
  const ordered = state.newestFirst ? [...slice].reverse() : slice;

  const frag = document.createDocumentFragment();
  for (const entry of ordered) frag.append(buildRow(entry));
  dom.logRows.replaceChildren(frag);
}

// tailWindowStart is where the rendered window should begin while following.
function tailWindowStart() {
  return Math.max(0, state.rows.length - RENDER_WINDOW);
}

// queueEntries accepts live entries and schedules a single render for the next
// animation frame. Doing the DOM work per network batch meant a forced reflow per
// batch, which is what made a fast stream feel sluggish.
function queueEntries(entries) {
  let accepted = false;
  for (const entry of entries) {
    if (entry.id <= state.lastId) continue;   // resume overlap
    state.lastId = entry.id;
    if (state.paused) {
      state.pending.push(entry);
    } else {
      state.inbox.push(entry);
    }
    accepted = true;
  }
  if (!accepted) return;

  if (state.paused) {
    const overflow = state.pending.length - HARD_MAX;
    if (overflow > 0) {
      state.pending.splice(0, overflow);
      state.pendingDropped += overflow;
    }
    renderCounts();
    return;
  }

  const overflow = state.inbox.length - HARD_MAX;
  if (overflow > 0) {
    state.inbox.splice(0, overflow);
    state.pendingDropped += overflow;
  }
  scheduleFrame();
}

let frameHandle = 0;

// lastUserScrollAt records when the user last deliberately scrolled the log. A
// scroll event on its own is not evidence of intent: appending rows and scrolling
// programmatically both produce them.
let lastUserScrollAt = 0;

function markUserScroll() {
  lastUserScrollAt = performance.now();
}

function scheduleFrame() {
  if (frameHandle) return;
  frameHandle = requestAnimationFrame(flushFrame);
}

// flushFrame moves everything queued during the frame into the loaded set and
// updates the DOM once.
function flushFrame() {
  frameHandle = 0;
  if (!state.inbox.length) {
    renderCounts();
    return;
  }

  const fresh = state.inbox.splice(0, state.inbox.length);
  for (const entry of fresh) state.rows.push(entry);
  // Every entry the server forwarded already matched the filter; the status poll
  // corrects any drift.
  state.matched += fresh.length;

  const following = isFollowing();
  const cap = following ? MAX_LIVE : HARD_MAX;
  let dropped = 0;
  if (state.rows.length > cap) {
    dropped = state.rows.length - cap;
    state.rows.splice(0, dropped);
  }

  if (following) {
    // Slide the window forward by the same amount the loaded set lost, then by
    // whatever is needed to keep it at the tail.
    state.windowStart = Math.max(0, state.windowStart - dropped);
    const target = tailWindowStart();
    if (target > state.windowStart) {
      removeOldestRows(target - state.windowStart);
      state.windowStart = target;
    }
    appendLiveRows(fresh);
    scrollToLatest(true);
  } else {
    state.windowStart = Math.max(0, state.windowStart - dropped);
    state.newCount += fresh.length;
  }
  renderCounts();
}

function appendLiveRows(entries) {
  const frag = document.createDocumentFragment();
  if (state.newestFirst) {
    for (let i = entries.length - 1; i >= 0; i--) frag.append(buildRow(entries[i]));
    dom.logRows.insertBefore(frag, dom.logRows.firstChild);
  } else {
    for (const entry of entries) frag.append(buildRow(entry));
    dom.logRows.append(frag);
  }
}

function removeOldestRows(count) {
  for (let i = 0; i < count; i++) {
    const node = state.newestFirst ? dom.logRows.lastElementChild : dom.logRows.firstElementChild;
    if (!node) return;
    node.remove();
  }
}

// extendWindow reveals up to count older loaded rows, keeping the content that is
// already on screen exactly where it is.
function extendWindow(count) {
  const take = Math.min(count, state.windowStart);
  if (take <= 0) return false;

  const start = state.windowStart - take;
  const older = state.rows.slice(start, state.windowStart);
  const scroll = dom.logScroll;
  const previousHeight = scroll.scrollHeight;
  const previousTop = scroll.scrollTop;

  const frag = document.createDocumentFragment();
  if (state.newestFirst) {
    for (const entry of older) frag.append(buildRow(entry));
    dom.logRows.append(frag);
  } else {
    for (const entry of older) frag.append(buildRow(entry));
    dom.logRows.insertBefore(frag, dom.logRows.firstChild);
  }
  state.windowStart = start;

  if (!state.newestFirst) {
    // Rows inserted above must not move what the user is reading.
    scroll.scrollTop = previousTop + Math.max(0, scroll.scrollHeight - previousHeight);
  }
  return true;
}

function buildRow(entry) {
  const row = document.createElement('div');
  row.className = 'row';
  row.dataset.level = entry.level || 'Default';
  row.dataset.id = entry.id;
  row.__entry = entry;

  row.append(
    cell('c-time', formatTime(entry.timestamp)),
    cell('c-level', entry.level || 'Default'),
    cell('c-process', entry.process || '—', !entry.process),
    cell('c-subsystem', entry.subsystem || '—', !entry.subsystem),
    cell('c-category', entry.category || '—', !entry.category),
    cell('c-message', '', false, highlight(entry.message || '', true)),
  );
  return row;
}

function cell(className, text, isEmpty, nodes) {
  const div = document.createElement('div');
  div.className = className;
  if (isEmpty) div.classList.add('empty-dim');
  if (nodes) div.append(...nodes);
  else div.textContent = text;
  return div;
}

/**
 * Splits text into text nodes and <mark> elements for the active query. DOM nodes
 * are used instead of innerHTML so log content can never be interpreted as HTML.
 */
function highlight(text, isMessage) {
  const nodes = [];
  const query = state.filters.query.trim();
  if (!isMessage || !query) {
    nodes.push(document.createTextNode(text));
    return nodes;
  }

  let regex = null;
  if (state.filters.regex) {
    try {
      regex = new RegExp(query, `g${state.filters.caseSensitive ? '' : 'i'}`);
    } catch {
      nodes.push(document.createTextNode(text));
      return nodes;
    }
  } else {
    regex = new RegExp(escapeRegex(query), `g${state.filters.caseSensitive ? '' : 'i'}`);
  }

  let last = 0;
  let match;
  let guard = 0;
  while ((match = regex.exec(text)) !== null && guard++ < 500) {
    if (match[0] === '') {
      regex.lastIndex++;
      continue;
    }
    if (match.index > last) nodes.push(document.createTextNode(text.slice(last, match.index)));
    const mark = document.createElement('mark');
    mark.textContent = match[0];
    nodes.push(mark);
    last = match.index + match[0].length;
  }
  if (last < text.length) nodes.push(document.createTextNode(text.slice(last)));
  if (!nodes.length) nodes.push(document.createTextNode(text));
  return nodes;
}

function renderCounts() {
  const loaded = state.rows.length;
  const rendered = dom.logRows.childElementCount;
  let visibleText = `${loaded.toLocaleString()} loaded`;
  if (rendered < loaded) visibleText += ` · ${rendered.toLocaleString()} rendered`;
  if (state.paused) visibleText += ' (paused)';
  if (state.newCount > 0 && !state.autoScroll) visibleText += ` · ${state.newCount.toLocaleString()} new not shown`;
  setText(dom.visibleCount, visibleText);

  // state.matched starts from the snapshot and is corrected by every status poll,
  // which recounts the buffer, so evictions are reflected instead of accumulating.
  if (!state.applying) {
    let matchText = `${state.matched.toLocaleString()} match the filter in the buffer`;
    if (state.snapshot.truncated) matchText += ` · newest ${loaded.toLocaleString()} loaded`;
    if (state.hasMoreOlder) matchText += ' · older entries on disk';
    setText(dom.matchCount, matchText);
  }

  const dropped = state.clientDropped + state.pendingDropped;
  const historyFull = !!state.status?.history?.full;
  if (dropped > 0 && !historyFull) {
    dom.dropNote.hidden = false;
    setText(dom.dropNote, `${dropped.toLocaleString()} entries dropped for this viewer`);
  } else if (!historyFull) {
    dom.dropNote.hidden = true;
  }

  dom.emptyState.hidden = loaded + state.pending.length > 0;
  renderOlderButton();
  renderNewBadge();
}

function renderOlderButton() {
  const full = state.rows.length >= HARD_MAX;
  // The button is for going past the buffer; while older rows are still only in
  // memory, scrolling reveals them without a round trip.
  const show = (state.hasMoreOlder || full) && state.rows.length > 0 && state.windowStart === 0;
  dom.loadOlder.hidden = !show;
  if (!show || state.loadingOlder) return;
  if (full) {
    dom.loadOlder.disabled = true;
    setText(dom.loadOlder, `${HARD_MAX.toLocaleString()} row limit reached — clear the view to load more`);
  } else {
    dom.loadOlder.disabled = false;
    setText(dom.loadOlder, `↑ Load older entries (before ${formatTime(state.rows[0].timestamp)})`);
  }
}

function renderNewBadge() {
  if (state.newCount > 0 && !state.autoScroll) {
    dom.jumpLatest.hidden = false;
    const arrow = state.newestFirst ? '↑' : '↓';
    setText(dom.jumpLatest, `${state.newCount.toLocaleString()} new ${arrow}`);
  } else {
    dom.jumpLatest.hidden = true;
  }
}

function scrollToLatest(instant) {
  const node = dom.logScroll;
  if (state.newestFirst) {
    node.scrollTo({ top: 0, behavior: instant ? 'auto' : 'smooth' });
    return;
  }
  node.scrollTo({ top: node.scrollHeight, behavior: instant ? 'auto' : 'smooth' });
}

function isAtLatest() {
  const node = dom.logScroll;
  if (state.newestFirst) return node.scrollTop <= TOP_TRIGGER;
  return node.scrollHeight - node.scrollTop - node.clientHeight <= TOP_TRIGGER;
}

/* ------------------------------------------------------------ older pages -- */

async function loadOlder() {
  if (state.loadingOlder || !state.rows.length || state.rows.length >= HARD_MAX) return;
  state.loadingOlder = true;
  dom.loadOlder.disabled = true;
  setText(dom.loadOlder, 'Loading older entries…');

  const oldest = state.rows[0].id;
  try {
    const params = filterParams();
    params.set('limit', String(PAGE_SIZE));
    params.set('before', String(oldest));
    const res = await api(`/api/logs?${params}`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();

    if (data.entries && data.entries.length) {
      prependEntries(data.entries);
      state.hasMoreOlder = !!data.hasMore;
    } else {
      state.hasMoreOlder = false;
    }
  } catch (err) {
    showBanner(`Could not load older entries: ${err.message}`, 'error');
  } finally {
    state.loadingOlder = false;
    renderCounts();
  }
}

// prependEntries adds a page of older entries fetched from the server, then
// reveals as much of it as the window allows without moving the current view.
function prependEntries(entries) {
  const scroll = dom.logScroll;
  const previousHeight = scroll.scrollHeight;
  const previousTop = scroll.scrollTop;

  state.rows = entries.concat(state.rows);
  // The rendered window still refers to the same entries, which have shifted by
  // the number just inserted.
  state.windowStart += entries.length;
  extendWindow(Math.min(entries.length, RENDER_EXTEND));

  if (state.newestFirst) {
    scroll.scrollTop = previousTop;
  } else {
    scroll.scrollTop = previousTop + Math.max(0, scroll.scrollHeight - previousHeight) - 0;
  }
}

/* ----------------------------------------------------------------- facets -- */

// applyFacets is fed by the snapshot, so the counts always describe the same
// instant as the rows on screen.
function applyFacets(facets) {
  selects[0].setFacets(facets.subsystems);
  selects[1].setFacets(facets.categories);
  selects[2].setFacets(facets.processes);
  state.levelFacets = new Map((facets.levels || []).map((f) => [f.value, f.count]));
  renderLevelChips();
}

// refreshFacets tops the counts up between snapshots. It no longer skips while a
// picker is open: setFacets keeps the list in place, so a value that starts
// logging now shows up in the picker immediately instead of on the next reopen.
async function refreshFacets() {
  if (state.applying) return;
  if (!state.status || (!state.status.running && !state.status.total)) return;
  try {
    const res = await api(`/api/facets?${filterParams()}&limit=400`);
    if (!res.ok) return;
    applyFacets(await res.json());
  } catch { /* transient */ }
}

function renderLevelChips() {
  const values = new Set(KNOWN_LEVELS);
  for (const value of state.levelFacets.keys()) values.add(value);
  for (const value of state.filters.levels) values.add(value);

  const ordered = [...values].sort((a, b) => {
    const ai = KNOWN_LEVELS.indexOf(a);
    const bi = KNOWN_LEVELS.indexOf(b);
    if (ai !== -1 && bi !== -1) return ai - bi;
    if (ai !== -1) return -1;
    if (bi !== -1) return 1;
    return a.localeCompare(b);
  });

  const existing = [...dom.levelFilter.children];
  const sameSet = existing.length === ordered.length
    && existing.every((chip, i) => chip.dataset.level === ordered[i]);

  if (sameSet) {
    // Only the counts changed: update text in place so the layout stays put.
    existing.forEach((chip) => {
      const count = state.levelFacets.get(chip.dataset.level);
      const label = count === undefined ? chip.dataset.level : `${chip.dataset.level} ${count}`;
      if (chip.textContent !== label) chip.textContent = label;
      chip.classList.toggle('on', state.filters.levels.has(chip.dataset.level));
    });
    return;
  }

  dom.levelFilter.innerHTML = '';
  for (const value of ordered) {
    const count = state.levelFacets.get(value);
    const chip = document.createElement('button');
    chip.type = 'button';
    chip.className = 'chip' + (state.filters.levels.has(value) ? ' on' : '');
    chip.dataset.level = value;
    chip.textContent = count === undefined ? value : `${value} ${count}`;
    chip.title = `Show only ${value} entries`;
    chip.addEventListener('click', () => {
      if (state.filters.levels.has(value)) state.filters.levels.delete(value);
      else state.filters.levels.add(value);
      renderLevelChips();
      scheduleFilterChange();
    });
    dom.levelFilter.append(chip);
  }
}

/* -------------------------------------------------------- filter handling -- */

const scheduleFilterChange = debounce(() => applyFilters(), 220);

function applyFilters() {
  if (!validateRegex()) {
    showBanner('That regular expression is not valid, so the filter was not applied.', 'error', true);
    return;
  }
  syncURL();
  savePrefs();
  closeOtherPopovers(null);
  // The rows on screen stay put until the new snapshot arrives; the previous
  // filter's match count is cleared rather than left to look current.
  state.matched = 0;
  openStream();
}

/* ---------------------------------------------------------------- export -- */

function exportAs(format) {
  const params = filterParams();
  params.set('format', format);
  const link = document.createElement('a');
  link.href = `/api/export?${params}`;
  link.download = '';
  document.body.append(link);
  link.click();
  link.remove();
}

async function copyText(text, label) {
  try {
    await navigator.clipboard.writeText(text);
    showBanner(`${label} copied to the clipboard.`, 'info', true);
  } catch (err) {
    showBanner(`Could not copy: ${err.message}`, 'error');
  }
}

function copyLoadedJSON() {
  const payload = {
    exportedAt: new Date().toISOString(),
    filter: describeFilter(),
    count: state.rows.length,
    entries: state.rows,
  };
  copyText(JSON.stringify(payload, null, 2), `${state.rows.length} loaded entries`);
}

function describeFilter() {
  const f = state.filters;
  const parts = [];
  if (f.subsystems.size) parts.push(`subsystem=${[...f.subsystems].join('|')}`);
  if (f.categories.size) parts.push(`category=${[...f.categories].join('|')}`);
  if (f.processes.size) parts.push(`process=${[...f.processes].join('|')}`);
  if (f.levels.size) parts.push(`level=${[...f.levels].join('|')}`);
  if (f.query) parts.push(`${f.regex ? 'regex' : 'text'}=${f.query}`);
  return parts.length ? parts.join(' ') : 'all events';
}

/* ------------------------------------------------------------------ detail -- */

function showDetail(entry) {
  state.selectedId = entry.id;
  for (const node of dom.logRows.children) {
    node.classList.toggle('selected', node.__entry && node.__entry.id === entry.id);
  }
  dom.detailBody.textContent = JSON.stringify(entry, null, 2);
  dom.detail.hidden = false;
}

/* ------------------------------------------------------------------- init -- */

function wireEvents() {
  selects.push(new MultiSelect(dom.subsystemFilter, 'Subsystem', 'subsystems', 'All subsystems'));
  selects.push(new MultiSelect(dom.categoryFilter, 'Category', 'categories', 'All categories'));
  selects.push(new MultiSelect(dom.processFilter, 'Process', 'processes', 'All processes'));

  document.addEventListener('click', () => closeOtherPopovers(null));

  dom.targetSelect.addEventListener('change', () => {
    state.filters.targetKey = dom.targetSelect.value;
    const target = findTarget(dom.targetSelect.value);
    dom.bootSimulator.hidden = !(target && target.kind === 'simulator' && !target.ready);
    savePrefs();
  });

  dom.levelSelect.addEventListener('change', () => {
    state.filters.level = dom.levelSelect.value;
    savePrefs();
  });

  dom.refreshTargets.addEventListener('click', () => fetchTargets());
  dom.bootSimulator.addEventListener('click', () => bootSelectedSimulator());
  dom.captureToggle.addEventListener('click', () => toggleCapture());
  dom.historyClear.addEventListener('click', () => clearHistory());

  dom.advancedToggle.addEventListener('click', () => {
    dom.advancedRow.hidden = !dom.advancedRow.hidden;
  });

  dom.predicate.value = state.filters.predicate;
  dom.predicate.addEventListener('input', () => {
    state.filters.predicate = dom.predicate.value;
    savePrefs();
  });

  dom.query.value = state.filters.query;
  dom.query.addEventListener('input', () => {
    state.filters.query = dom.query.value;
    validateRegex();
    scheduleFilterChange();
  });

  dom.regexToggle.classList.toggle('active', state.filters.regex);
  dom.regexToggle.addEventListener('click', () => {
    state.filters.regex = !state.filters.regex;
    dom.regexToggle.classList.toggle('active', state.filters.regex);
    validateRegex();
    scheduleFilterChange();
  });

  dom.caseToggle.classList.toggle('active', state.filters.caseSensitive);
  dom.caseToggle.addEventListener('click', () => {
    state.filters.caseSensitive = !state.filters.caseSensitive;
    dom.caseToggle.classList.toggle('active', state.filters.caseSensitive);
    validateRegex();
    scheduleFilterChange();
  });

  dom.requireSubsystem.checked = state.filters.requireSubsystem;
  dom.requireSubsystem.addEventListener('change', () => {
    state.filters.requireSubsystem = dom.requireSubsystem.checked;
    scheduleFilterChange();
  });

  dom.pauseToggle.addEventListener('click', () => {
    state.paused = !state.paused;
    dom.pauseToggle.classList.toggle('active', state.paused);
    dom.pauseToggle.textContent = state.paused ? 'Resume' : 'Pause';
    if (!state.paused && state.pending.length) {
      state.inbox.push(...state.pending);
      state.pending = [];
      scheduleFrame();
      scrollToLatest(false);
    }
    renderCounts();
  });

  dom.autoscrollToggle.classList.toggle('active', state.autoScroll);
  dom.autoscrollToggle.addEventListener('click', () => {
    setAutoScroll(!state.autoScroll);
    if (state.autoScroll) scrollToLatest(false);
  });

  dom.sortToggle.classList.toggle('active', state.newestFirst);
  dom.sortToggle.addEventListener('click', () => {
    state.newestFirst = !state.newestFirst;
    dom.sortToggle.classList.toggle('active', state.newestFirst);
    savePrefs();
    renderWindow();
    scrollToLatest(true);
  });

  dom.clearView.addEventListener('click', () => resetView());

  dom.logRows.addEventListener('click', (ev) => {
    const row = ev.target.closest('.row');
    if (row && row.__entry) showDetail(row.__entry);
  });

  for (const gesture of ['wheel', 'touchmove', 'pointerdown']) {
    dom.logScroll.addEventListener(gesture, markUserScroll, { passive: true });
  }
  dom.logScroll.addEventListener('keydown', markUserScroll);

  dom.logScroll.addEventListener('scroll', () => {
    // Only a real gesture should stop the view following the tail. Reacting to
    // bare scroll events let our own layout changes un-follow the stream, so the
    // rows kept arriving but stopped being drawn.
    const userDriven = performance.now() - lastUserScrollAt < 1200;
    const atLatest = isAtLatest();

    if (atLatest) {
      if (!state.autoScroll) setAutoScroll(true);
    } else if (userDriven && state.autoScroll) {
      setAutoScroll(false);
      state.newCount = 0;
    }

    if (userDriven && !state.loadingOlder && state.hasMoreOlder && atOlderEdge()) {
      // Reveal what is already loaded first; only ask the server once the
      // in-memory entries above the window are exhausted.
      if (!extendWindow(RENDER_EXTEND)) loadOlder();
    }
    renderCounts();
  });

  dom.jumpLatest.addEventListener('click', () => {
    setAutoScroll(true);
    scrollToLatest(false);
  });
  dom.loadOlder.addEventListener('click', () => loadOlder());

  // A hidden tab does not run animation frames, so flush on return instead of
  // leaving the queued entries invisible.
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) scheduleFrame();
  });

  dom.detailClose.addEventListener('click', () => { dom.detail.hidden = true; });
  dom.detailCopy.addEventListener('click', () => {
    const entry = state.rows.find((e) => e.id === state.selectedId);
    if (entry) copyText(JSON.stringify(entry, null, 2), 'Entry');
  });

  dom.bannerClose.addEventListener('click', () => { dom.banner.hidden = true; });

  document.querySelectorAll('.bottombar [data-format]').forEach((button) => {
    button.addEventListener('click', () => exportAs(button.dataset.format));
  });
  dom.copyJson.addEventListener('click', copyLoadedJSON);
  dom.copyLink.addEventListener('click', () => copyText(location.href, 'Share link'));

  document.addEventListener('keydown', (ev) => {
    if (ev.key === '/' && document.activeElement !== dom.query && !isTyping(ev.target)) {
      ev.preventDefault();
      dom.query.focus();
      dom.query.select();
    } else if (ev.key === 'Escape') {
      if (!dom.detail.hidden) {
        dom.detail.hidden = true;
      } else if (document.activeElement === dom.query && dom.query.value) {
        dom.query.value = '';
        state.filters.query = '';
        scheduleFilterChange();
      } else {
        closeOtherPopovers(null);
      }
    }
  });
}

function setAutoScroll(on) {
  state.autoScroll = on;
  dom.autoscrollToggle.classList.toggle('active', on);
  savePrefs();
  if (on) {
    state.newCount = 0;
    if (state.rows.length > MAX_LIVE) {
      state.rows.splice(0, state.rows.length - MAX_LIVE);
    }
    state.windowStart = tailWindowStart();
    renderWindow();
    scrollToLatest(true);
  }
  renderCounts();
}

function isTyping(node) {
  if (!node) return false;
  const tag = node.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || node.isContentEditable;
}

function validateRegex() {
  if (!state.filters.regex || !state.filters.query.trim()) {
    dom.query.classList.remove('regex-error');
    return true;
  }
  try {
    new RegExp(state.filters.query, state.filters.caseSensitive ? '' : 'i');
    dom.query.classList.remove('regex-error');
    return true;
  } catch {
    dom.query.classList.add('regex-error');
    return false;
  }
}

async function pollStatus() {
  try {
    const res = await api(`/api/status?${filterParams()}`);
    const status = await res.json();
    if (typeof status.matched === 'number') state.matched = status.matched;
    applyStatus(status);
  } catch { /* transient */ }
}

async function init() {
  loadPrefs();
  filtersFromURL();

  dom.query.value = state.filters.query;
  dom.regexToggle.classList.toggle('active', state.filters.regex);
  dom.caseToggle.classList.toggle('active', state.filters.caseSensitive);
  dom.requireSubsystem.checked = state.filters.requireSubsystem;
  dom.levelSelect.value = state.filters.level;
  dom.sortToggle.classList.toggle('active', state.newestFirst);
  dom.autoscrollToggle.classList.toggle('active', state.autoScroll);
  dom.predicate.value = state.filters.predicate;
  if (state.filters.predicate) dom.advancedRow.hidden = false;

  wireEvents();
  selects.forEach((s) => s.syncFromState());
  renderLevelChips();

  await fetchTargets();
  await pollStatus();
  openStream();
  refreshFacets();

  setInterval(pollStatus, STATUS_REFRESH_MS);
  setInterval(refreshFacets, FACET_REFRESH_MS);
}

init();
