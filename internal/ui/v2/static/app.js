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
    let h = (location.hash || '#/overview').replace(/^#/, '');
    // Strip any ?query=... before splitting on slashes; route params live in
    // the path segments, the query is parsed per-view.
    const qIdx = h.indexOf('?');
    if (qIdx >= 0) h = h.slice(0, qIdx);
    const parts = h.split('/').filter(Boolean);
    if (parts.length === 0) return { name: 'overview' };
    if (parts[0] === 'overview' || parts[0] === 'namespaces' || parts[0] === 'timeline' || parts[0] === 'logs' || parts[0] === 'diff') {
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
      { key: 'diff',       label: 'Diff',       hash: '#/diff' },
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
    let d;
    try {
      d = await getJSON('/v2/api/namespace?ns=' + encodeURIComponent(ns));
    } catch (e) {
      setContent(errorState(e.message));
      return;
    }

    const kpis = d.kpis || {};
    const root = el('div', {});

    // Hero — breadcrumb + title + action row
    const hero = el('div', { class: 'hero', style: 'margin:-18px -20px 18px; padding:14px 20px 14px;' },
      el('div', { class: 'crumbs' },
        el('a', { onclick: () => go('#/overview') }, '← Cluster'),
        el('span', { class: 'sep' }, '/'),
        el('a', { onclick: () => go('#/namespaces') }, 'Namespaces'),
        el('span', { class: 'sep' }, '/'),
        el('b', {}, d.name),
      ),
      el('div', { class: 'hero-row' },
        el('div', {},
          el('div', { class: 'title-row' },
            el('div', { class: 'title' }, d.name),
            (d.metadata && d.metadata.phase) ? el('span', { class: 'pill ' + (d.metadata.phase === 'Active' ? 'good' : 'warn') }, d.metadata.phase) : null,
            kpis.unhealthy_pods > 0 ? el('span', { class: 'pill bad' }, kpis.unhealthy_pods + ' unhealthy') : null,
          ),
          el('div', { class: 'sub' },
            (kpis.resources || 0) + ' resources · created ' + (d.metadata?.age || '?') + ' ago',
          ),
        ),
        el('div', { class: 'hero-actions' },
          el('button', { class: 'btn', onclick: () => go('#/logs') }, '▶ Open in Logs'),
          el('button', { class: 'btn' }, '⏷ Compare snapshots'),
        ),
      ),
    );
    root.appendChild(hero);

    // KPIs
    const kpiStrip = el('div', { class: 'kpis k6' });
    kpiStrip.appendChild(kpi('Workloads', kpis.workloads || 0));
    kpiStrip.appendChild(kpi('Pods', kpis.pods || 0));
    kpiStrip.appendChild(kpi('Unhealthy pods', kpis.unhealthy_pods || 0, { severity: (kpis.unhealthy_pods || 0) > 0 ? 'alert' : '' }));
    kpiStrip.appendChild(kpi('VirtualMachines', kpis.virtual_machines || 0));
    kpiStrip.appendChild(kpi('ConfigMaps', kpis.configmaps || 0));
    kpiStrip.appendChild(kpi('Secrets', kpis.secrets || 0));
    root.appendChild(kpiStrip);

    // Spark + issues row
    const sparkCard = el('div', { class: 'card' },
      cardHeader('Pod-state transitions (this namespace)',
        ((d.sparkline && d.sparkline.total_events) || 0) + ' events'),
      sparkChart(d.sparkline || {}));
    const issuesCard = el('div', { class: 'card' },
      cardHeader('Issues in this namespace', String((d.issues || []).length)),
      issuesList(d.issues));
    const row1 = el('div', { class: 'grid-2', style: 'margin-bottom:18px;' });
    row1.appendChild(sparkCard);
    row1.appendChild(issuesCard);
    root.appendChild(row1);

    // Workloads + VMs row
    const wlCard = el('div', { class: 'card' }, cardHeader('Workloads', String((d.workloads || []).length)));
    if (!d.workloads || d.workloads.length === 0) {
      wlCard.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, 'No workloads.'));
    } else {
      for (const w of d.workloads.slice(0, 12)) wlCard.appendChild(resourceRowEl(w));
      if (d.workloads.length > 12) wlCard.appendChild(el('div', { style: 'padding:6px 4px; font-size:11px; color:var(--fg-faint);' }, `+ ${d.workloads.length - 12} more`));
    }
    const vmCard = el('div', { class: 'card' }, cardHeader('VirtualMachines', String((d.vms || []).length)));
    if (!d.vms || d.vms.length === 0) {
      vmCard.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, 'No VirtualMachines captured.'));
    } else {
      for (const v of d.vms.slice(0, 12)) vmCard.appendChild(resourceRowEl(v));
      if (d.vms.length > 12) vmCard.appendChild(el('div', { style: 'padding:6px 4px; font-size:11px; color:var(--fg-faint);' }, `+ ${d.vms.length - 12} more`));
    }
    const row2 = el('div', { class: 'grid-2', style: 'margin-bottom:18px;' });
    row2.appendChild(wlCard);
    row2.appendChild(vmCard);
    root.appendChild(row2);

    // Pods table
    const podHeader = cardHeader('Pods', `${d.pods?.length || 0} total · ${kpis.unhealthy_pods || 0} unhealthy`);
    const podCard = el('div', { class: 'card', style: 'margin-bottom:18px;' }, podHeader);
    if (!d.pods || d.pods.length === 0) {
      podCard.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, 'No pods captured.'));
    } else {
      for (const p of d.pods.slice(0, 40)) podCard.appendChild(podRowEl(p));
      if (d.pods.length > 40) podCard.appendChild(el('div', { style: 'padding:6px 4px; font-size:11px; color:var(--fg-faint);' }, `+ ${d.pods.length - 40} more (filtering UI coming)`));
    }
    root.appendChild(podCard);

    // Other resources tile grid
    if (d.resources && d.resources.length > 0) {
      root.appendChild(el('div', { class: 'section-title' }, 'Other resources'));
      const tileCard = el('div', { class: 'card', style: 'margin-bottom:18px;' });
      const tileGrid = el('div', { class: 'resource-grid', style: 'grid-template-columns:repeat(6, minmax(0,1fr));' });
      for (const t of d.resources) tileGrid.appendChild(resourceTile(t));
      tileCard.appendChild(tileGrid);
      root.appendChild(tileCard);
    }

    // Namespace metadata
    if (d.metadata && (d.metadata.labels || d.metadata.annotations)) {
      root.appendChild(el('div', { class: 'section-title' }, 'Namespace metadata'));
      const grid = el('div', { class: 'grid-2' });

      const labCard = el('div', { class: 'card' }, cardHeader('Labels & annotations', ''));
      const labels = d.metadata.labels || {};
      if (Object.keys(labels).length === 0) {
        labCard.appendChild(el('div', { class: 'state', style: 'padding:14px;' }, 'No labels.'));
      } else {
        const wrap = el('div', { class: 'labels' });
        for (const k of Object.keys(labels)) {
          wrap.appendChild(el('span', { class: 'lab' }, `${k}=${labels[k]}`));
        }
        labCard.appendChild(wrap);
      }
      grid.appendChild(labCard);

      const ownersCard = el('div', { class: 'card' }, cardHeader('Status', ''));
      ownersCard.appendChild(el('div', { style: 'font-size:12.5px; color:var(--fg-dim); display:flex; flex-direction:column; gap:6px;' },
        el('div', {}, 'Phase: ', el('span', { style: 'color:var(--fg);' }, d.metadata.phase || '?')),
        el('div', {}, 'Created: ', el('span', { style: 'color:var(--fg); font-family:var(--mono);' }, d.metadata.created_at || '?')),
        el('div', {}, 'Age: ', el('span', { style: 'color:var(--fg); font-family:var(--mono);' }, d.metadata.age || '?')),
      ));
      grid.appendChild(ownersCard);

      root.appendChild(grid);
    }

    setContent(root);
  }

  // Render a workload/VM ResourceRow into a .resrow element.
  function resourceRowEl(r) {
    return el('div', {
      class: 'resrow',
      onclick: () => { /* TODO: link to detail */ },
    },
      el('span', { class: 'dot ' + (r.severity || 'neutral') }),
      el('span', { class: 'kind' }, r.kind || ''),
      el('span', { class: 'nm' }, r.name || ''),
      el('span', { class: 'status ' + (r.severity || '') }, r.status || ''),
      el('span', { class: 'num' }, r.restarts ? String(r.restarts) : ''),
      el('span', { class: 'num' }, r.age || ''),
    );
  }

  // Render a PodRow into a .resrow with a click-handler to the pod drill-down.
  function podRowEl(p) {
    return el('div', {
      class: 'resrow',
      onclick: () => p.link && go(p.link),
    },
      el('span', { class: 'dot ' + (p.severity || 'neutral') }),
      el('span', { class: 'kind' }, 'Pod'),
      el('span', { class: 'nm' }, p.name || ''),
      el('span', { class: 'status ' + (p.severity || '') }, p.status || p.phase || ''),
      el('span', { class: 'num' }, p.restarts ? String(p.restarts) : ''),
      el('span', { class: 'num' }, p.age || ''),
    );
  }

  async function renderPod(ns, name) {
    setContent(loadingState('Loading pod ' + ns + '/' + name + '…'));
    let d;
    try {
      d = await getJSON('/v2/api/pod?ns=' + encodeURIComponent(ns) + '&name=' + encodeURIComponent(name));
    } catch (e) {
      setContent(errorState(e.message));
      return;
    }

    const root = el('div', {});
    // Assigned once the tab bar is built below; hero action buttons jump to a tab.
    let selectTab = () => {};

    // Hero
    const sevToBadge = { good: 'good', warn: 'warn', bad: 'bad' };
    root.appendChild(el('div', { class: 'hero', style: 'margin:-18px -20px 0; padding:14px 20px 14px;' },
      el('div', { class: 'crumbs' },
        el('a', { onclick: () => go('#/overview') }, '← Cluster'),
        el('span', { class: 'sep' }, '/'),
        el('a', { onclick: () => go('#/ns/' + encodeURIComponent(d.namespace)) }, d.namespace),
        el('span', { class: 'sep' }, '/'),
        d.related && d.related.owner ? el('span', {}, d.related.owner.kind + ' / ' + d.related.owner.name) : null,
        d.related && d.related.owner ? el('span', { class: 'sep' }, '/') : null,
        el('b', {}, 'Pod / ' + d.name),
      ),
      el('div', { class: 'hero-row' },
        el('div', {},
          el('div', { class: 'title-row' },
            el('div', { class: 'title' }, d.name),
            d.hero ? el('span', { class: 'pill ' + (sevToBadge[d.hero.severity] || 'neutral') }, d.hero.reason || d.hero.phase) : null,
          ),
          el('div', { class: 'sub' }, 'Pod · ' + d.namespace + (d.hero?.subtitle ? ' · ' + d.hero.subtitle : '')),
        ),
        el('div', { class: 'hero-actions' },
          el('button', { class: 'btn', onclick: () => go('#/logs?ns=' + encodeURIComponent(d.namespace) + '&pod=' + encodeURIComponent(d.name)) }, '▶ Logs'),
          el('button', { class: 'btn', onclick: () => selectTab('events') }, 'Events · ' + ((d.events || []).length)),
          el('button', { class: 'btn', onclick: () => go('#/diff?path=' + encodeURIComponent('/api/v1/namespaces/' + d.namespace + '/pods') + '&name=' + encodeURIComponent(d.name)) }, '⏷ Compare snapshots'),
          el('button', { class: 'btn', onclick: () => selectTab('yaml') }, 'YAML'),
        ),
      ),
    ));

    // KPIs (persistent — shown under the tab bar on every tab).
    const kpis = d.kpis || {};
    const kpiStrip = el('div', { class: 'kpis k6' });
    kpiStrip.appendChild(kpi('Phase', kpis.phase || '?', {
      severity: d.hero?.severity === 'bad' ? 'alert' : (d.hero?.severity === 'warn' ? 'warn' : ''),
      delta: d.hero?.reason || '',
    }));
    kpiStrip.appendChild(kpi('Restarts', kpis.restarts || 0, {
      severity: (kpis.restarts || 0) > 0 ? 'alert' : '',
    }));
    kpiStrip.appendChild(kpi('Age', kpis.age || '?'));
    kpiStrip.appendChild(kpi('Ready', kpis.ready || '0/0'));
    kpiStrip.appendChild(kpi('Node', kpis.node || '—', { delta: kpis.pod_ip || '' }));
    kpiStrip.appendChild(kpi('QoS', kpis.qos_class || '—'));

    // ── Tab panels ───────────────────────────────────────────────────────────
    const emptyState = (msg) => el('div', { class: 'state', style: 'padding:24px;' }, msg);

    const eventRow = (ev) => el('div', { class: 'event ' + (ev.severity || 'normal') },
      el('span', { class: 'kind ' + (ev.severity === 'bad' ? 'bad' : (ev.severity === 'warn' ? 'warn' : '')) }, ev.reason || ''),
      el('span', { class: 'body' },
        el('b', {}, ev.message || ev.reason || ''),
        ev.source || ev.count ? el('span', { class: 'small' }, (ev.source ? ev.source : '') + (ev.count ? ' · ' + ev.count + 'x' : '')) : null,
      ),
      el('span', { class: 'age' }, formatShortTS(ev.time)),
    );

    const metaRow = (k, v, color) => el('div', { style: 'display:grid; grid-template-columns:140px 1fr; gap:8px; font-size:12.5px;' },
      el('span', { style: 'color:var(--fg-faint);' }, k),
      el('span', { style: 'font-family:var(--mono); color:' + (color || 'var(--fg)') + ';' }, v));

    function buildSummary() {
      const wrap = el('div', {});
      const sparkData = { buckets: d.restart_sparkline || [], total_events: (d.restart_sparkline || []).reduce((s, b) => s + (b.total || 0), 0), start_time: d.capture?.captured_at, end_time: d.capture?.captured_until };
      const sparkCard = el('div', { class: 'card' },
        cardHeader('Container restarts over capture window', sparkData.total_events + ' events'),
        sparkChart(sparkData));
      const evCard = el('div', { class: 'card' }, cardHeader('Recent events', String((d.events || []).length)));
      if (!d.events || d.events.length === 0) {
        evCard.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, 'No events captured for this pod. (Did the capture include /api/v1/events?)'));
      } else {
        const w = el('div', { class: 'events' });
        for (const ev of d.events.slice(0, 6)) w.appendChild(eventRow(ev));
        evCard.appendChild(w);
      }
      const row1 = el('div', { class: 'grid-2', style: 'margin-bottom:18px;' });
      row1.appendChild(sparkCard);
      row1.appendChild(evCard);
      wrap.appendChild(row1);

      const metaCard = el('div', { class: 'card' }, cardHeader('Metadata', ''));
      if (d.related?.owner) metaCard.appendChild(metaRow('Owner', (d.related.owner.kind || '') + '/' + (d.related.owner.name || '')));
      if (d.kpis?.node) metaCard.appendChild(metaRow('Node', d.kpis.node));
      if (d.kpis?.pod_ip) metaCard.appendChild(metaRow('Pod IP', d.kpis.pod_ip));
      if (d.metadata?.created_at) metaCard.appendChild(metaRow('Created', d.metadata.created_at));
      if (d.metadata?.labels && Object.keys(d.metadata.labels).length > 0) {
        metaCard.appendChild(el('div', { class: 'section-title', style: 'margin-top:10px;' }, 'Labels'));
        const labWrap = el('div', { class: 'labels', style: 'margin-top:10px;' });
        for (const k of Object.keys(d.metadata.labels)) labWrap.appendChild(el('span', { class: 'lab' }, k + '=' + d.metadata.labels[k]));
        metaCard.appendChild(labWrap);
      }
      if (d.metadata?.conditions && d.metadata.conditions.length > 0) {
        metaCard.appendChild(el('div', { class: 'section-title', style: 'margin-top:10px;' }, 'Conditions'));
        for (const c of d.metadata.conditions) {
          const sevColor = c.severity === 'good' ? 'var(--good)' : c.severity === 'bad' ? 'var(--bad)' : 'var(--fg-dim)';
          metaCard.appendChild(metaRow(c.type, c.status + (c.reason ? ' · ' + c.reason : ''), sevColor));
        }
      }
      wrap.appendChild(metaCard);
      return wrap;
    }

    function buildContainers() {
      if (!(d.containers || []).length) return emptyState('No containers found for this pod.');
      const card = el('div', { class: 'card' });
      for (const c of d.containers) card.appendChild(containerCardEl(c));
      return card;
    }

    function buildLogs() {
      const withLogs = (d.containers || []).filter((c) => (c.log_preview || []).length > 0 || c.log_path);
      if (!withLogs.length) return emptyState('No logs captured for this pod. Set "logs: <N>" on the pods resource in the capture config to capture container logs.');
      const wrap = el('div', {});
      for (const c of withLogs) {
        const card = el('div', { class: 'card', style: 'margin-bottom:14px;' },
          cardHeader('Logs · ' + c.name, (c.role && c.role !== 'main') ? c.role : ''));
        if ((c.log_preview || []).length) {
          card.appendChild(el('pre', { class: 'log-preview', style: 'max-height:340px; padding:12px;' }, c.log_preview.join('\n')));
        } else {
          card.appendChild(el('div', { class: 'state', style: 'padding:12px;' }, 'No preview captured.'));
        }
        const actions = el('div', { style: 'display:flex; gap:8px; margin-top:10px;' });
        if (c.log_path) actions.appendChild(el('button', { class: 'btn', onclick: () => go(c.log_path) }, 'Open full logs'));
        if (c.has_previous_log && c.log_path) actions.appendChild(el('button', { class: 'btn', onclick: () => go(c.log_path + '&previous=true') }, 'Previous logs'));
        if (actions.childNodes.length) card.appendChild(actions);
        wrap.appendChild(card);
      }
      return wrap;
    }

    function buildEvents() {
      if (!d.events || d.events.length === 0) return emptyState('No events captured for this pod. (Did the capture include /api/v1/events?)');
      const card = el('div', { class: 'card' }, cardHeader('Events', String(d.events.length)));
      const w = el('div', { class: 'events' });
      for (const ev of d.events) w.appendChild(eventRow(ev));
      card.appendChild(w);
      return card;
    }

    function buildHistory() {
      if (!d.history || d.history.length === 0) return emptyState('No watch events captured for this pod. Enable "watch: true" on the pods resource to record transitions.');
      const card = el('div', { class: 'card' }, cardHeader('State history in capture', String(d.history.length)));
      for (const t of d.history) {
        const evCls = t.event_type === 'ADDED' ? 'added' : (t.event_type === 'DELETED' ? 'deleted' : 'modified');
        card.appendChild(el('div', { class: 'timeline-row' },
          el('span', { class: 'ts' }, formatShortTS(t.time)),
          el('span', { class: 'dot ' + evCls }),
          el('span', { class: 'ev' }, t.event_type + (t.detail ? ' · ' + t.detail : '')),
        ));
      }
      return card;
    }

    function buildRelationships() {
      const r = d.related || {};
      const rows = [];
      if (r.owner) rows.push(['Owner', r.owner.kind + '/' + r.owner.name]);
      if (r.workload) rows.push(['Workload', r.workload.kind + '/' + r.workload.name]);
      if (typeof r.sibling_pods === 'number' && r.sibling_pods > 0) rows.push(['Sibling pods', String(r.sibling_pods)]);
      for (const cm of (r.config_maps || [])) rows.push(['Mounts ConfigMap', cm.name]);
      for (const s of (r.secrets || [])) rows.push(['Mounts Secret', s.name]);
      for (const p of (r.pvcs || [])) rows.push(['Mounts PVC', p.name]);
      if (!rows.length) return emptyState('No related objects found for this pod.');
      const card = el('div', { class: 'card' }, cardHeader('Relationships', String(rows.length)));
      for (const [k, v] of rows) {
        card.appendChild(el('div', { class: 'timeline-row' },
          el('span', { class: 'ts' }, k),
          el('span', { class: 'ev' }, v)));
      }
      return card;
    }

    function buildYaml() {
      return emptyState('Raw YAML view isn\'t available here yet — the captured object lives in the archive. Use the Compare snapshots (Diff) view or "kshrk inspect" to view it.');
    }

    const logsCount = (d.containers || []).filter((c) => (c.log_preview || []).length > 0).length;
    const relCount = (d.related?.owner ? 1 : 0) + (d.related?.config_maps || []).length + (d.related?.secrets || []).length + (d.related?.pvcs || []).length;
    const panelDefs = [
      { key: 'summary', label: 'Summary', count: null, build: buildSummary },
      { key: 'containers', label: 'Containers', count: (d.containers || []).length, build: buildContainers },
      { key: 'logs', label: 'Logs', count: logsCount, build: buildLogs },
      { key: 'events', label: 'Events', count: (d.events || []).length, build: buildEvents },
      { key: 'history', label: 'History', count: (d.history || []).length, build: buildHistory },
      { key: 'relationships', label: 'Relationships', count: relCount, build: buildRelationships },
      { key: 'yaml', label: 'YAML', count: null, build: buildYaml },
    ];

    // Tab bar: clicking a tab lazily builds its panel and shows it.
    const subtabs = el('div', { class: 'subtabs', style: 'margin:0 -20px 18px;' });
    const panelHost = el('div', {});
    const tabEls = {};
    const panelEls = {};
    selectTab = (key) => {
      for (const def of panelDefs) {
        const active = def.key === key;
        tabEls[def.key].classList.toggle('active', active);
        if (active && !panelEls[def.key]) {
          panelEls[def.key] = def.build();
          panelHost.appendChild(panelEls[def.key]);
        }
        if (panelEls[def.key]) panelEls[def.key].style.display = active ? '' : 'none';
      }
    };
    for (const def of panelDefs) {
      const t = el('div', { class: 'tab', onclick: () => selectTab(def.key) }, def.label);
      if (def.count !== null) t.appendChild(el('span', { class: 'ct' }, ' · ' + def.count));
      tabEls[def.key] = t;
      subtabs.appendChild(t);
    }
    root.appendChild(subtabs);
    root.appendChild(kpiStrip);
    root.appendChild(panelHost);
    selectTab('summary');
    setContent(root);
  }

  function containerCardEl(c) {
    const card = el('div', { class: 'container-card ' + (c.severity || '') });
    card.appendChild(el('div', { class: 'container-head' },
      el('span', { class: 'nm' }, c.name),
      el('span', { class: 'role ' + (c.role === 'init' ? 'init' : (c.role === 'side' ? 'side' : '')) }, c.role || 'main'),
      el('span', { class: 'state ' + (c.severity || 'good') }, c.state_badge || c.state || ''),
    ));
    const meta = el('div', { class: 'container-meta' });
    const mkRow = (k, v, cls) => {
      meta.appendChild(el('span', { class: 'k' }, k));
      meta.appendChild(el('span', { class: 'v ' + (cls || '') }, v || ''));
    };
    if (c.image) mkRow('Image', c.image);
    if (c.last_terminated) {
      mkRow('Last state', `Terminated · ${c.last_terminated}${c.last_exit_code ? ' · exit ' + c.last_exit_code : ''}`, 'bad');
    }
    if (c.resources) {
      if (c.resources.cpu_limit || c.resources.cpu_request) {
        mkRow('CPU', (c.resources.cpu_request || '?') + ' req · ' + (c.resources.cpu_limit || 'no limit'));
      }
      if (c.resources.memory_limit || c.resources.memory_request) {
        mkRow('Memory', (c.resources.memory_request || '?') + ' req · ' + (c.resources.memory_limit || 'no limit'));
      }
    }
    if (c.probes && c.probes.length > 0) mkRow('Probes', c.probes.join(' · '));
    if (c.ports && c.ports.length > 0) mkRow('Ports', c.ports.join(', '));
    card.appendChild(meta);
    if (c.log_preview && c.log_preview.length > 0) {
      const pre = el('div', { class: 'log-preview' });
      for (const line of c.log_preview) {
        pre.appendChild(document.createTextNode(line + '\n'));
      }
      card.appendChild(pre);
    } else {
      card.appendChild(el('div', { class: 'state', style: 'padding:10px; font-size:11.5px;' }, 'No captured log lines for this container.'));
    }
    const actions = el('div', { style: 'display:flex; gap:8px; margin-top:8px;' },
      c.log_path ? el('button', { class: 'btn', onclick: () => go(c.log_path) }, 'View logs') : null,
      c.has_previous_log && c.log_path ? el('button', { class: 'btn', onclick: () => go(c.log_path + '&previous=true') }, 'Previous logs') : null,
    );
    card.appendChild(actions);
    return card;
  }

  // ── Timeline ─────────────────────────────────────────────────────────────
  async function renderTimeline() {
    setContent(loadingState('Loading timeline…'));
    let data;
    try {
      data = await getJSON('/v2/api/overview');
    } catch (e) {
      setContent(errorState(e.message));
      return;
    }
    const root = el('div', {});
    const sparkData = data.sparkline || {};
    const sparkCard = el('div', { class: 'card', style: 'margin-bottom:18px;' },
      cardHeader('Watch events across capture window', ((sparkData.total_events) || 0) + ' events'),
      sparkChart(sparkData),
    );
    root.appendChild(sparkCard);
    root.appendChild(el('div', { class: 'section-title' }, 'All recent transitions'));
    const recentCard = el('div', { class: 'card' });
    if (!data.recent || data.recent.length === 0) {
      recentCard.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, 'No watch events were captured. Enable `watch: true` on your config’s resource entries to see live transitions.'));
    } else {
      const wrap = el('div', { class: 'events' });
      for (const t of data.recent) wrap.appendChild(recentRow(t));
      recentCard.appendChild(wrap);
    }
    root.appendChild(recentCard);
    setContent(root);
  }

  // ── Logs ─────────────────────────────────────────────────────────────────
  async function renderLogs() {
    // Parse query params from the hash (e.g. #/logs?ns=X&pod=Y&container=Z)
    const params = parseLogsParams();
    if (!params.ns || !params.pod) {
      setContent(el('div', { class: 'state' }, 'Open a pod first, then click "Logs" to view its captured logs here.'));
      return;
    }

    setContent(loadingState('Loading logs for ' + params.pod + '…'));
    let data;
    try {
      const q = new URLSearchParams();
      q.set('ns', params.ns);
      q.set('pod', params.pod);
      if (params.container) q.set('container', params.container);
      if (params.previous) q.set('previous', 'true');
      data = await getJSON('/v2/api/logs?' + q.toString());
    } catch (e) {
      setContent(errorState(e.message));
      return;
    }

    const root = el('div', {});

    // Hero
    root.appendChild(el('div', { class: 'hero', style: 'margin:-18px -20px 18px; padding:14px 20px 14px;' },
      el('div', { class: 'crumbs' },
        el('a', { onclick: () => go('#/overview') }, '← Cluster'),
        el('span', { class: 'sep' }, '/'),
        el('a', { onclick: () => go('#/ns/' + encodeURIComponent(data.namespace)) }, data.namespace),
        el('span', { class: 'sep' }, '/'),
        el('a', { onclick: () => go('#/ns/' + encodeURIComponent(data.namespace) + '/pod/' + encodeURIComponent(data.pod)) }, 'Pod / ' + data.pod),
        el('span', { class: 'sep' }, '/'),
        el('b', {}, 'Logs · ' + data.container + (data.previous ? ' (previous)' : '')),
      ),
      el('div', { class: 'hero-row' },
        el('div', {},
          el('div', { class: 'title-row' },
            el('div', { class: 'title' }, data.container),
            el('span', { class: 'pill neutral' }, data.line_count + ' lines'),
          ),
          el('div', { class: 'sub' }, data.pod + (data.previous ? ' · previous container' : ' · current container')),
        ),
      ),
    ));

    // Container picker + previous toggle
    const picker = el('div', { class: 'card', style: 'margin-bottom:18px; padding:10px 14px; flex-direction:row; align-items:center; gap:10px;' });
    picker.appendChild(el('span', { style: 'font-size:11px; color:var(--fg-faint); text-transform:uppercase; letter-spacing:.06em;' }, 'Container'));
    for (const c of (data.containers || [])) {
      const isActive = c.name === data.container;
      const btn = el('button', {
        class: 'btn' + (isActive ? ' primary' : ''),
        onclick: () => go('#/logs?ns=' + encodeURIComponent(data.namespace) + '&pod=' + encodeURIComponent(data.pod) + '&container=' + encodeURIComponent(c.name) + (data.previous ? '&previous=true' : '')),
      }, c.name);
      picker.appendChild(btn);
    }
    picker.appendChild(el('span', { style: 'margin-left:auto; font-size:11px; color:var(--fg-faint);' }, data.has_previous_variant ? 'previous log captured for this container' : ''));
    picker.appendChild(el('button', {
      class: 'btn' + (data.previous ? ' primary' : ''),
      disabled: data.has_previous_variant ? null : 'disabled',
      onclick: () => go('#/logs?ns=' + encodeURIComponent(data.namespace) + '&pod=' + encodeURIComponent(data.pod) + '&container=' + encodeURIComponent(data.container) + (data.previous ? '' : '&previous=true')),
    }, data.previous ? '← current logs' : 'previous logs →'));
    root.appendChild(picker);

    // Log text
    const pre = el('pre', { class: 'log-preview', style: 'max-height: calc(100vh - 320px); padding:14px;' }, data.text || '');
    root.appendChild(pre);

    setContent(root);
  }

  // ── Diff ─────────────────────────────────────────────────────────────────
  async function renderDiff() {
    const params = parseDiffParams();
    const root = el('div', {});

    // Form card
    const form = el('div', { class: 'card', style: 'margin-bottom:18px;' },
      cardHeader('Compare an object at two snapshots', ''));

    const inputStyle = 'background:var(--bg-row); border:1px solid var(--border-strong); color:var(--fg); padding:6px 10px; border-radius:5px; font-size:12.5px; outline:none; min-width:240px;';
    const pathInput = el('input', { value: params.path || '', placeholder: '/api/v1/namespaces/<ns>/pods', style: inputStyle });
    const nameInput = el('input', { value: params.name || '', placeholder: 'pod name (optional, filters list)', style: inputStyle });
    const beforeSelect = el('select', { style: inputStyle });
    const afterSelect = el('select', { style: inputStyle });
    for (const ts of state.snapshots) {
      beforeSelect.appendChild(el('option', { value: ts, selected: params.before === ts ? 'selected' : null }, ts));
      afterSelect.appendChild(el('option', { value: ts, selected: params.after === ts ? 'selected' : null }, ts));
    }
    if (!params.before && state.snapshots.length > 0) {
      beforeSelect.value = state.snapshots[0];
    }
    if (!params.after && state.snapshots.length > 0) {
      afterSelect.value = state.snapshots[state.snapshots.length - 1];
    }
    const goBtn = el('button', { class: 'btn primary', onclick: () => runDiff() }, 'Compare');

    const formRow = el('div', { style: 'display:grid; grid-template-columns:max-content 1fr; gap:8px 16px; align-items:center;' },
      el('span', {}, 'Path'),
      pathInput,
      el('span', {}, 'Name (optional)'),
      nameInput,
      el('span', {}, 'Before'),
      beforeSelect,
      el('span', {}, 'After'),
      afterSelect,
    );
    form.appendChild(formRow);
    form.appendChild(el('div', { style: 'display:flex; gap:8px; justify-content:flex-end; margin-top:8px;' }, goBtn));
    root.appendChild(form);

    const out = el('div', {});
    root.appendChild(out);
    setContent(root);

    async function runDiff() {
      const q = new URLSearchParams();
      q.set('path', pathInput.value.trim());
      if (nameInput.value.trim()) q.set('name', nameInput.value.trim());
      q.set('before', beforeSelect.value);
      q.set('after', afterSelect.value);
      out.innerHTML = '';
      out.appendChild(loadingState('Diffing…'));
      try {
        const data = await getJSON('/v2/api/diff?' + q.toString());
        out.innerHTML = '';
        if (data.before_missing || data.after_missing) {
          out.appendChild(el('div', { class: 'state' },
            data.before_missing && data.after_missing
              ? 'Object not present at either snapshot.'
              : (data.before_missing ? 'Object did not exist at the "before" snapshot — full add.' : 'Object did not exist at the "after" snapshot — full delete.'),
          ));
        }
        if (!data.has_diff && !data.before_missing && !data.after_missing) {
          out.appendChild(el('div', { class: 'state' }, 'No changes between snapshots.'));
        }
        const diff = el('div', { class: 'diff', style: 'padding:14px;' });
        for (const h of (data.hunks || [])) {
          const cls = h.type === 'add' ? 'add' : (h.type === 'del' ? 'del' : 'ctx');
          diff.appendChild(el('div', { class: cls }, h.text));
        }
        out.appendChild(diff);
      } catch (e) {
        out.innerHTML = '';
        out.appendChild(errorState(e.message));
      }
    }
    if (params.path && params.before && params.after) runDiff();
  }

  function parseDiffParams() {
    const idx = (location.hash || '').indexOf('?');
    if (idx < 0) return {};
    const q = new URLSearchParams(location.hash.slice(idx + 1));
    return {
      path: q.get('path') || '',
      name: q.get('name') || '',
      before: q.get('before') || '',
      after: q.get('after') || '',
    };
  }

  function parseLogsParams() {
    const idx = (location.hash || '').indexOf('?');
    if (idx < 0) return {};
    const q = new URLSearchParams(location.hash.slice(idx + 1));
    return {
      ns: q.get('ns') || '',
      pod: q.get('pod') || '',
      container: q.get('container') || '',
      previous: q.get('previous') === 'true',
    };
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
    if (r.name === 'diff') return renderDiff();
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
