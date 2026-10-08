'use strict';

/**
 * OSLog Live front end.
 *
 * Filtering happens on the server: whenever the filter changes the page opens a
 * new SSE connection whose query string is the filter, so the view always
 * reflects the whole server side buffer rather than only what the browser has
 * already loaded. The browser keeps its own bounded window of rows for display.
 */

const MAX_ROWS = 3000;
const MAX_PENDING = 20000;
const FACET_REFRESH_MS = 4000;
const STATUS_REFRESH_MS = 2000;

const KNOWN_LEVELS = ['Error', 'Fault', 'Default', 'Info', 'Debug', 'Signpost', 'Activity', 'Loss', 'Timesync', 'State', 'Trace'];

const state = {
  targets: { simulators: [], devices: [], errors: {} },
  status: null,
  epoch: null,
  connected: false,
  rows: [],
  selectedId: null,
  paused: false,
  pending: [],
  autoScroll: true,
  newestFirst: false,
  snapshotMeta: { matched: 0, truncated: false },
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
  'log-scroll', 'log-head', 'log-rows', 'empty-state', 'jump-latest',
  'visible-count', 'match-count', 'drop-note',
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
    if (this.open) closeOtherPopovers(this);
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
    if (this.open) this.renderList();
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
    clearRows();
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

/* ------------------------------------------------------------------ status -- */

function applyStatus(status, skipEpochCheck) {
  state.status = status;

  const dotClass = status.state === 'running' ? 'running'
    : status.state === 'restarting' ? 'restarting'
    : status.state === 'error' ? 'error' : 'idle';
  dom.statusDot.className = `dot ${dotClass}`;

  const running = !!status.running;
  let text = 'Idle';
  if (status.state === 'running') text = `Capturing · ${status.target?.name || ''}`;
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

  if (!skipEpochCheck && state.epoch !== null && status.epoch !== state.epoch) {
    // A new capture started elsewhere: rebuild our view from the fresh buffer.
    state.epoch = status.epoch;
    openStream({ reset: true });
  } else if (state.epoch === null) {
    state.epoch = status.epoch;
  }
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
  if (reset) clearRows();

  const params = filterParams();
  params.set('snapshot', String(MAX_ROWS));
  const es = new EventSource(`/api/stream?${params}`);
  source = es;

  es.addEventListener('snapshot', (ev) => {
    const payload = JSON.parse(ev.data);
    state.epoch = payload.status?.epoch ?? state.epoch;
    state.snapshotMeta = { matched: payload.matched, truncated: payload.truncated };
    state.connected = true;
    if (payload.mode === 'full') {
      // A full snapshot answers an explicit filter change, so it replaces the
      // view even while live updates are paused.
      clearRows();
      commitEntries(payload.entries, { force: true });
    } else {
      addEntries(payload.entries);
    }
    renderCounts();
  });

  es.addEventListener('log', (ev) => {
    const payload = JSON.parse(ev.data);
    addEntries(payload.entries);
  });

  es.addEventListener('status', (ev) => {
    applyStatus(JSON.parse(ev.data));
  });

  es.addEventListener('warning', (ev) => {
    const payload = JSON.parse(ev.data);
    state.clientDropped += payload.dropped || 0;
    renderCounts();
  });

  es.addEventListener('error', () => {
    state.connected = false;
    if (!state.status || !state.status.running) {
      // Nothing to stream: fall back to polling so controls stay in sync.
      setText(dom.statusText, state.status && state.status.total > 0 ? 'Stopped' : 'Idle');
    }
  });
}

/* ---------------------------------------------------------------- render -- */

function clearRows() {
  state.rows = [];
  state.pending = [];
  state.selectedId = null;
  dom.logRows.innerHTML = '';
  dom.detail.hidden = true;
  renderCounts();
}

function addEntries(entries) {
  if (!entries || !entries.length) return;

  const lastKnown = state.rows.length ? state.rows[state.rows.length - 1].id : 0;
  const fresh = [];
  for (const entry of entries) {
    // IDs are monotonic, so anything at or below the watermark is a duplicate
    // from a resume or a snapshot overlap.
    if (entry.id <= lastKnown) continue;
    fresh.push(entry);
  }
  if (!fresh.length) return;

  if (state.paused) {
    state.pending.push(...fresh);
    const overflow = state.pending.length - MAX_PENDING;
    if (overflow > 0) {
      state.pending.splice(0, overflow);
      state.pendingDropped += overflow;
    }
    renderCounts();
    return;
  }
  commitEntries(fresh);
}

function commitEntries(fresh, { force } = {}) {
  if (force) {
    state.rows = [];
    dom.logRows.innerHTML = '';
  }
  state.rows.push(...fresh);
  if (state.rows.length > MAX_ROWS) {
    const overflow = state.rows.length - MAX_ROWS;
    state.rows.splice(0, overflow);
    removeRows(overflow);
  }
  appendRows(fresh);
  renderCounts();
  if (state.autoScroll) scrollToLatest(true);
}

function appendRows(entries) {
  const frag = document.createDocumentFragment();
  if (state.newestFirst) {
    for (let i = entries.length - 1; i >= 0; i--) frag.append(buildRow(entries[i]));
    dom.logRows.insertBefore(frag, dom.logRows.firstChild);
  } else {
    for (const entry of entries) frag.append(buildRow(entry));
    dom.logRows.append(frag);
  }
}

function removeRows(count) {
  for (let i = 0; i < count; i++) {
    const node = state.newestFirst ? dom.logRows.lastElementChild : dom.logRows.firstElementChild;
    if (!node) break;
    node.remove();
  }
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
    const flags = state.filters.caseSensitive ? 'g' : 'gi';
    regex = new RegExp(escapeRegex(query), flags);
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
  const shown = state.rows.length;
  setText(dom.visibleCount, `${shown.toLocaleString()} shown${state.paused ? ' (paused)' : ''}`);

  const meta = state.snapshotMeta;
  let matchText = '';
  if (meta && typeof meta.matched === 'number') {
    matchText = `${meta.matched.toLocaleString()} match the filter in the server buffer`;
    if (meta.truncated) matchText += ` · showing the most recent ${shown.toLocaleString()}`;
  }
  setText(dom.matchCount, matchText);

  const dropped = state.clientDropped + state.pendingDropped;
  if (dropped > 0) {
    dom.dropNote.hidden = false;
    setText(dom.dropNote, `${dropped.toLocaleString()} entries dropped for this viewer`);
  } else {
    dom.dropNote.hidden = true;
  }

  const total = state.rows.length + state.pending.length;
  dom.emptyState.hidden = total > 0;
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
  if (state.newestFirst) return node.scrollTop <= 24;
  return node.scrollHeight - node.scrollTop - node.clientHeight <= 24;
}

function rebuildRows() {
  dom.logRows.innerHTML = '';
  const ordered = state.newestFirst ? [...state.rows].reverse() : state.rows;
  const frag = document.createDocumentFragment();
  for (const entry of ordered) frag.append(buildRow(entry));
  dom.logRows.append(frag);
}

/* ----------------------------------------------------------------- facets -- */

async function fetchFacets() {
  if (state.status && !state.status.running && !state.status.total) return;
  try {
    const res = await api(`/api/facets?${filterParams()}&limit=400`);
    if (!res.ok) return;
    const data = await res.json();
    selects[0].setFacets(data.subsystems);
    selects[1].setFacets(data.categories);
    selects[2].setFacets(data.processes);

    state.levelFacets = new Map((data.levels || []).map((f) => [f.value, f.count]));
    renderLevelChips();
    updateClientMatch(data.matched, data.buffered, data.truncated);
  } catch { /* transient */ }
}

function updateClientMatch(matched, buffered, truncated) {
  if (typeof matched !== 'number') return;
  state.snapshotMeta = { matched, truncated: !!truncated || matched > state.rows.length };
  renderCounts();
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
  syncURL();
  savePrefs();
  closeOtherPopovers(null);
  openStream({ reset: true });
  fetchFacets();
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
  for (const node of dom.logRows.children) node.classList.toggle('selected', node.__entry && node.__entry.id === entry.id);
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
      const pending = state.pending;
      state.pending = [];
      commitEntries(pending);
      scrollToLatest(false);
    }
    renderCounts();
  });

  dom.autoscrollToggle.classList.toggle('active', state.autoScroll);
  dom.autoscrollToggle.addEventListener('click', () => {
    state.autoScroll = !state.autoScroll;
    dom.autoscrollToggle.classList.toggle('active', state.autoScroll);
    savePrefs();
    if (state.autoScroll) {
      scrollToLatest(false);
      dom.jumpLatest.hidden = true;
    }
  });

  dom.sortToggle.classList.toggle('active', state.newestFirst);
  dom.sortToggle.addEventListener('click', () => {
    state.newestFirst = !state.newestFirst;
    dom.sortToggle.classList.toggle('active', state.newestFirst);
    savePrefs();
    rebuildRows();
    scrollToLatest(true);
  });

  dom.clearView.addEventListener('click', () => clearRows());

  dom.logRows.addEventListener('click', (ev) => {
    const row = ev.target.closest('.row');
    if (row && row.__entry) showDetail(row.__entry);
  });

  dom.logScroll.addEventListener('scroll', () => {
    const atLatest = isAtLatest();
    if (atLatest && state.autoScroll) return;
    if (!atLatest && state.autoScroll) {
      // The user scrolled away: stop following so new rows do not yank the view.
      state.autoScroll = false;
      dom.autoscrollToggle.classList.remove('active');
      savePrefs();
    }
    dom.jumpLatest.hidden = atLatest;
  });

  dom.jumpLatest.addEventListener('click', () => {
    state.autoScroll = true;
    dom.autoscrollToggle.classList.add('active');
    savePrefs();
    scrollToLatest(false);
    dom.jumpLatest.hidden = true;
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

function isTyping(node) {
  if (!node) return false;
  const tag = node.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || node.isContentEditable;
}

function validateRegex() {
  if (!state.filters.regex || !state.filters.query.trim()) {
    dom.query.classList.remove('regex-error');
    return;
  }
  try {
    new RegExp(state.filters.query, state.filters.caseSensitive ? '' : 'i');
    dom.query.classList.remove('regex-error');
  } catch {
    dom.query.classList.add('regex-error');
  }
}

async function pollStatus() {
  try {
    const res = await api('/api/status');
    applyStatus(await res.json());
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
  openStream({ reset: true });
  fetchFacets();

  setInterval(pollStatus, STATUS_REFRESH_MS);
  setInterval(fetchFacets, FACET_REFRESH_MS);
}

init();
