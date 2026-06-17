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

  // ── Shared bits ──────────────────────────────────────────────────────────
  function kpi(label, value, opts = {}) {
    return el('div', { class: 'kpi' + (opts.severity ? ' ' + opts.severity : '') },
      el('span', { class: 'label' }, label),
      el('span', { class: 'value' }, formatNumber(value)),
      opts.delta ? el('span', { class: 'delta ' + (opts.deltaKind || 'neutral') }, opts.delta) : null);
  }

  function formatNumber(n) {
    if (typeof n !== 'number') return n;
    return n.toLocaleString('en-US');
  }

  function sparkChart(sparkline) {
    const wrap = el('div', { class: 'spark' });
    const buckets = (sparkline && sparkline.buckets) || [];
    const totals = buckets.map((b) => b.total || 0);
    const max = totals.reduce((a, b) => Math.max(a, b), 0) || 1;
    for (const b of buckets) {
      const v = (b.total || 0) / max;
      const h = Math.max(2, Math.round(v * 100));
      const cls = b.bad > 0 ? 'bad' : (b.warn > 0 ? 'warn' : '');
      wrap.appendChild(el('div', { class: cls, style: `height: ${h}%` }));
    }
    const axis = el('div', { class: 'spark-axis' });
    if (buckets.length) {
      const s = buckets[0].time;
      const e = buckets[buckets.length - 1].time;
      axis.appendChild(el('span', {}, formatShortTS(s)));
      axis.appendChild(el('span', {}, formatShortTS(e)));
    }
    return el('div', {},
      wrap,
      axis,
    );
  }

  function formatShortTS(s) {
    if (!s) return '';
    // 2026-06-17T05:06:31Z → 05:06:31
    const m = String(s).match(/T(\d{2}:\d{2}:\d{2})/);
    return m ? m[1] : s;
  }

  function issueRow(issue) {
    const kindLabel = issue.count && issue.count > 1
      ? `${issue.kind} ×${issue.count}`
      : issue.kind;
    return el('div', {
      class: 'issue ' + (issue.severity || 'warn'),
      onclick: () => issue.link && go(issue.link),
    },
      el('span', { class: 'kind' }, kindLabel),
      el('span', { class: 'body' },
        el('b', {}, issue.title),
        issue.namespace ? el('span', { class: 'ns' }, issue.namespace) : null,
        issue.subtitle ? el('span', { class: 'ns' }, issue.subtitle) : null,
      ),
      el('span', { class: 'age' }, issue.age || ''),
    );
  }

  function resourceTile(t) {
    return el('div', {
      class: 'res-chip',
      onclick: () => t.link && go(t.link),
    },
      el('div', { class: 'ct' }, formatNumber(t.count) || ''),
      el('div', { class: 'nm' }, t.kind),
    );
  }

  function topNamespaceRow(ns) {
    const maxRefVal = topNsMax;
    const pct = maxRefVal > 0 ? Math.round((ns.resources / maxRefVal) * 100) : 0;
    return el('div', {
      onclick: () => go('#/ns/' + encodeURIComponent(ns.name)),
      style: 'cursor:pointer; padding: 4px 0;',
    },
      el('div', { class: 'barlist' },
        el('div', { class: 'row' },
          el('span', { class: 'lbl' }, ns.name),
          el('span', { class: 'ct' }, formatNumber(ns.resources)),
        ),
        el('div', { class: 'bar' }, el('div', { style: `width: ${pct}%` })),
      ),
    );
  }
  let topNsMax = 1;

  function recentRow(t) {
    const sevByEvent = { ADDED: 'normal', MODIFIED: 'warn', DELETED: 'bad' };
    const cls = sevByEvent[t.event_type] || 'normal';
    return el('div', { class: 'event ' + cls },
      el('span', { class: 'kind ' + (cls === 'bad' ? 'bad' : (cls === 'warn' ? 'warn' : '')) }, t.event_type || ''),
      el('span', { class: 'body' },
        el('b', {}, t.name || ''),
        el('span', { class: 'small' }, (t.kind || '') + (t.namespace ? ' · ' + t.namespace : '')),
      ),
      el('span', { class: 'age' }, formatShortTS(t.time)),
    );
  }

  // ── Overview view ────────────────────────────────────────────────────────
  async function renderOverview() {
    setContent(loadingState('Loading cluster overview…'));
    let data;
    try {
      data = await getJSON('/v2/api/overview');
    } catch (e) {
      setContent(errorState(e.message));
      return;
    }

    const kpis = data.kpis || {};
    topNsMax = (data.top_namespaces || []).reduce((m, n) => Math.max(m, n.resources || 0), 1);

    const root = el('div', {});

    // KPI strip
    const kpiStrip = el('div', { class: 'kpis' });
    kpiStrip.appendChild(kpi('Namespaces', kpis.namespaces || 0));
    kpiStrip.appendChild(kpi('Workloads', kpis.workloads || 0));
    kpiStrip.appendChild(kpi('Pods', kpis.pods || 0));
    const unhealthyDelta = unhealthyDeltaText(kpis);
    kpiStrip.appendChild(kpi('Unhealthy pods', kpis.unhealthy_pods || 0, {
      severity: (kpis.unhealthy_pods || 0) > 0 ? 'alert' : '',
      delta: unhealthyDelta,
      deltaKind: 'down',
    }));
    kpiStrip.appendChild(kpi('Watch events', kpis.watch_events || 0, {
      delta: data.sparkline && data.sparkline.buckets ? 'over ' + data.sparkline.buckets.length + ' buckets' : '',
      deltaKind: 'neutral',
    }));
    root.appendChild(kpiStrip);

    // Spark + issues row
    const sparkCard = el('div', { class: 'card' },
      cardHeader('Pod-state transitions (capture window)',
        ((data.sparkline && data.sparkline.total_events) || 0) + ' events',
        { hash: '#/timeline', label: 'View timeline →' }),
      sparkChart(data.sparkline || {}));

    const issuesCard = el('div', { class: 'card' },
      cardHeader('Issues to investigate', String((data.issues || []).length)),
      issuesList(data.issues));

    const row1 = el('div', { class: 'grid-2', style: 'margin-bottom:18px;' });
    row1.appendChild(sparkCard);
    row1.appendChild(issuesCard);
    root.appendChild(row1);

    // Resource tiles
    root.appendChild(el('div', { class: 'section-title' }, 'Resources captured'));
    const resourceCard = el('div', { class: 'card', style: 'margin-bottom:18px;' });
    const tileGrid = el('div', { class: 'resource-grid' });
    for (const t of (data.resources || [])) {
      tileGrid.appendChild(resourceTile(t));
    }
    resourceCard.appendChild(tileGrid);
    root.appendChild(resourceCard);

    // Top namespaces + recent row
    const topCard = el('div', { class: 'card' },
      cardHeader('Top namespaces by resource count', '', { hash: '#/namespaces', label: 'Browse all →' }));
    for (const ns of (data.top_namespaces || [])) {
      topCard.appendChild(topNamespaceRow(ns));
    }

    const recentCard = el('div', { class: 'card' },
      cardHeader('Recent transitions', 'last events'));
    if (!data.recent || data.recent.length === 0) {
      recentCard.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, 'No watch events captured.'));
    } else {
      const evWrap = el('div', { class: 'events' });
      for (const t of data.recent) evWrap.appendChild(recentRow(t));
      recentCard.appendChild(evWrap);
    }

    const row2 = el('div', { class: 'grid-2' });
    row2.appendChild(topCard);
    row2.appendChild(recentCard);
    root.appendChild(row2);

    setContent(root);
  }

  function cardHeader(title, ct, link) {
    return el('div', { class: 'hdr' },
      el('div', { class: 'title' }, title),
      ct ? el('span', { class: 'ct' }, ct) : null,
      link ? el('a', { onclick: () => go(link.hash) }, link.label || '→') : null,
    );
  }

  function issuesList(issues) {
    if (!issues || issues.length === 0) {
      return el('div', { class: 'state', style: 'padding:18px;' }, 'No unhealthy resources at this snapshot.');
    }
    const wrap = el('div', { class: 'issues' });
    for (const i of issues) wrap.appendChild(issueRow(i));
    return wrap;
  }

  function unhealthyDeltaText(kpis) {
    const parts = [];
    if (kpis.crash_loop_back_off) parts.push('CrashLoop ' + kpis.crash_loop_back_off);
    if (kpis.oom_killed) parts.push('OOMKilled ' + kpis.oom_killed);
    if (kpis.failed) parts.push('Failed ' + kpis.failed);
    if (kpis.pending) parts.push('Pending ' + kpis.pending);
    return parts.join(' · ');
  }

  // ── Namespaces index (full grid view) ────────────────────────────────────
  async function renderNamespacesIndex() {
    setContent(loadingState('Loading namespaces…'));
    let data;
    try {
      data = await getJSON('/v2/api/overview');
    } catch (e) {
      setContent(errorState(e.message));
      return;
    }
    const list = data.namespaces || [];
    const filter = el('input', {
      placeholder: 'Filter namespaces…',
      style: 'width:100%; max-width:380px; background:var(--bg-row); border:1px solid var(--border-strong); color:var(--fg); padding:7px 10px; border-radius:6px; outline:none; margin-bottom:14px;',
    });
    const grid = el('div', { class: 'resource-grid', style: 'grid-template-columns:repeat(4, minmax(0,1fr));' });
    function build(q) {
      grid.innerHTML = '';
      let shown = 0;
      for (const ns of list) {
        if (q && !ns.name.toLowerCase().includes(q)) continue;
        const card = el('div', {
          class: 'res-chip',
          style: 'cursor:pointer; padding:12px 14px;',
          onclick: () => go('#/ns/' + encodeURIComponent(ns.name)),
        });
        card.appendChild(el('div', { class: 'nm', style: 'font-size:13px;color:var(--fg);font-weight:500;margin-bottom:4px;' }, ns.name));
        const chips = el('div', { style: 'display:flex;gap:6px;flex-wrap:wrap;' });
        if (ns.workloads) chips.appendChild(el('span', { class: 'pill neutral' }, ns.workloads + ' workload' + (ns.workloads !== 1 ? 's' : '')));
        if (ns.pods) chips.appendChild(el('span', { class: 'pill neutral' }, ns.pods + ' pod' + (ns.pods !== 1 ? 's' : '')));
        if (ns.resources) chips.appendChild(el('span', { class: 'pill neutral' }, ns.resources + ' resource' + (ns.resources !== 1 ? 's' : '')));
        if (ns.unhealthy) chips.appendChild(el('span', { class: 'pill bad' }, ns.unhealthy + ' unhealthy'));
        card.appendChild(chips);
        grid.appendChild(card);
        shown++;
      }
      if (shown === 0) {
        grid.appendChild(el('div', { class: 'state', style: 'grid-column:1/-1;' }, 'No namespaces match.'));
      }
    }
    filter.addEventListener('input', () => build(filter.value.trim().toLowerCase()));
    build('');
    setContent(filter, grid);
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
