// k8shark v2 UI — dashboard-style explorer.
// Single-file vanilla JS. Hash-based router so a refresh stays on the right view.
// View functions render into #content; the topbar/nav/scrubber are shared.

(() => {
  'use strict';

  const $ = (id) => document.getElementById(id);
  const el = (tag, attrs = {}, ...children) => {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === 'class') n.className = v;
      else if (k === 'html') n.innerHTML = v;
      else if (k.startsWith('on') && typeof v === 'function') n.addEventListener(k.slice(2), v);
      else if (v !== undefined && v !== null) n.setAttribute(k, v);
    }
    for (const c of children) {
      if (c === null || c === undefined || c === false) continue;
      n.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
    }
    return n;
  };

  // ── State ────────────────────────────────────────────────────────────────
  const state = {
    captureMeta: null,
    snapshots: [],   // RFC3339 strings
    at: '',          // selected snapshot timestamp ('' = latest)
    route: { name: 'overview' },
  };

  // ── Router ───────────────────────────────────────────────────────────────
  // Routes:
  //   #/overview              dashboard landing
  //   #/namespaces            namespace browser
  //   #/ns/<name>             namespace drilldown
  //   #/ns/<name>/pod/<pod>   pod drilldown
  //   #/timeline              timeline
  //   #/logs                  logs full-screen
  function parseRoute() {
    const h = (location.hash || '#/overview').replace(/^#/, '');
    const parts = h.split('/').filter(Boolean);
    if (parts.length === 0) return { name: 'overview' };
    if (parts[0] === 'overview' || parts[0] === 'namespaces' || parts[0] === 'timeline' || parts[0] === 'logs') {
      return { name: parts[0] };
    }
    if (parts[0] === 'ns' && parts[1]) {
      if (parts[2] === 'pod' && parts[3]) {
        return { name: 'pod', ns: decodeURIComponent(parts[1]), pod: decodeURIComponent(parts[3]) };
      }
      return { name: 'namespace', ns: decodeURIComponent(parts[1]) };
    }
    return { name: 'overview' };
  }

  function go(hash) {
    if (location.hash === hash) {
      render();
    } else {
      location.hash = hash;
    }
  }
  window.addEventListener('hashchange', () => {
    state.route = parseRoute();
    render();
  });

  // ── Top nav / scrubber ───────────────────────────────────────────────────
  function renderNav() {
    const nav = $('nav');
    nav.innerHTML = '';
    const items = [
      { key: 'overview',   label: 'Overview',   hash: '#/overview' },
      { key: 'namespaces', label: 'Namespaces', hash: '#/namespaces' },
      { key: 'timeline',   label: 'Timeline',   hash: '#/timeline' },
      { key: 'logs',       label: 'Logs',       hash: '#/logs' },
    ];
    const activeKey = state.route.name === 'namespace' || state.route.name === 'pod' ? 'namespaces' : state.route.name;
    for (const it of items) {
      nav.appendChild(el('div', {
        class: 'tab' + (it.key === activeKey ? ' active' : ''),
        onclick: () => go(it.hash),
      }, it.label));
    }
  }

  function renderScrubber() {
    const s = $('scrubber');
    s.innerHTML = '';
    const prev = el('button', { class: 'btn', onclick: stepSnapshot.bind(null, -1) }, '◀');
    const next = el('button', { class: 'btn', onclick: stepSnapshot.bind(null, +1) }, '▶');
    const range = el('input', { type: 'range', min: '0', max: String(Math.max(0, state.snapshots.length - 1)), value: String(currentSnapshotIndex()) });
    range.addEventListener('input', () => {
      const idx = Number(range.value);
      state.at = state.snapshots[idx] || '';
      ts.textContent = formatTS(state.at);
    });
    range.addEventListener('change', () => {
      const idx = Number(range.value);
      state.at = state.snapshots[idx] || '';
      render();
    });
    const ts = el('span', { class: 'ts' }, formatTS(state.at));
    s.appendChild(prev);
    s.appendChild(range);
    s.appendChild(next);
    s.appendChild(ts);
  }

  function currentSnapshotIndex() {
    if (!state.at) return state.snapshots.length - 1; // latest
    const i = state.snapshots.indexOf(state.at);
    return i >= 0 ? i : state.snapshots.length - 1;
  }

  function stepSnapshot(delta) {
    const idx = Math.max(0, Math.min(state.snapshots.length - 1, currentSnapshotIndex() + delta));
    state.at = state.snapshots[idx] || '';
    render();
  }

  function formatTS(s) {
    if (!s) return 'latest';
    return s.replace('T', ' ').replace(/Z$/, ' UTC');
  }

  // ── HTTP ─────────────────────────────────────────────────────────────────
  async function getJSON(path) {
    const url = new URL(path, location.href);
    if (state.at) url.searchParams.set('at', state.at);
    const res = await fetch(url.toString());
    let data = null;
    try { data = await res.json(); } catch (_) { /* not JSON */ }
    if (!res.ok) {
      const msg = (data && data.error) || `${res.status} ${res.statusText}`;
      throw new Error(msg);
    }
    return data;
  }

  // ── Toasts ───────────────────────────────────────────────────────────────
  function toast(kind, message, ttl) {
    const box = el('div', { class: 'toast ' + kind }, message);
    $('toasts').appendChild(box);
    setTimeout(() => box.remove(), ttl || (kind === 'error' ? 6000 : 3000));
  }

  // ── View rendering ───────────────────────────────────────────────────────
  function setContent(...nodes) {
    const c = $('content');
    c.innerHTML = '';
    for (const n of nodes) c.appendChild(n);
  }

  function loadingState(msg) {
    return el('div', { class: 'state' }, msg || 'Loading…');
  }
  function errorState(msg) {
    return el('div', { class: 'state' }, 'Error: ' + msg);
  }

  // ─ Views (placeholders to be filled in subsequent commits) ──────────────
  async function renderOverview() {
    setContent(loadingState('Loading cluster overview…'));
    try {
      const data = await getJSON('/v2/api/overview');
      // Real rendering lands in the next commit.
      setContent(el('div', { class: 'state' }, 'Overview data loaded (' + Object.keys(data || {}).length + ' fields). Real layout in next commit.'));
    } catch (e) {
      setContent(errorState(e.message));
    }
  }

  async function renderNamespacesIndex() {
    setContent(loadingState('Loading namespaces…'));
    try {
      const data = await getJSON('/v2/api/overview');
      const list = (data && data.namespaces) || [];
      setContent(el('div', { class: 'state' }, list.length + ' namespaces. Card grid lands in next commit.'));
    } catch (e) {
      setContent(errorState(e.message));
    }
  }

  async function renderNamespace(ns) {
    setContent(loadingState('Loading namespace ' + ns + '…'));
    try {
      const data = await getJSON('/v2/api/namespace?ns=' + encodeURIComponent(ns));
      setContent(el('div', { class: 'state' }, 'Namespace ' + ns + ' data loaded. Real layout in next commit.'));
    } catch (e) {
      setContent(errorState(e.message));
    }
  }

  async function renderPod(ns, name) {
    setContent(loadingState('Loading pod ' + ns + '/' + name + '…'));
    try {
      const data = await getJSON('/v2/api/pod?ns=' + encodeURIComponent(ns) + '&name=' + encodeURIComponent(name));
      setContent(el('div', { class: 'state' }, 'Pod ' + ns + '/' + name + ' data loaded. Real layout in next commit.'));
    } catch (e) {
      setContent(errorState(e.message));
    }
  }

  function renderTimeline() {
    setContent(el('div', { class: 'state' }, 'Timeline view — coming soon.'));
  }
  function renderLogs() {
    setContent(el('div', { class: 'state' }, 'Logs view — coming soon.'));
  }

  function render() {
    renderNav();
    renderScrubber();
    const r = state.route;
    if (r.name === 'overview') return renderOverview();
    if (r.name === 'namespaces') return renderNamespacesIndex();
    if (r.name === 'namespace') return renderNamespace(r.ns);
    if (r.name === 'pod') return renderPod(r.ns, r.pod);
    if (r.name === 'timeline') return renderTimeline();
    if (r.name === 'logs') return renderLogs();
    return setContent(errorState('Unknown route'));
  }

  // ── Bootstrap ────────────────────────────────────────────────────────────
  async function init() {
    state.route = parseRoute();
    try {
      const ts = await getJSON('/v2/api/timestamps');
      state.snapshots = (ts && ts.timestamps) || [];
      state.captureMeta = ts && {
        captured_at: ts.captured_at,
        captured_until: ts.captured_until,
        total_count: ts.total_count,
      };
      if (state.captureMeta) {
        $('capture-meta').textContent =
          `${state.captureMeta.captured_at?.slice(0, 19) ?? ''} → ${state.captureMeta.captured_until?.slice(0, 19) ?? ''} · ${state.captureMeta.total_count} records`;
      }
    } catch (e) {
      // Timestamps endpoint not implemented yet — keep going with empty snapshots.
      $('capture-meta').textContent = 'capture loaded';
    }
    render();
  }
  document.addEventListener('DOMContentLoaded', init);
  // Expose for debugging in the browser console.
  window.kshrk = { state, go, render };
})();
