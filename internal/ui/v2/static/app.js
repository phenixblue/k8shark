// k8shark v2 UI — dashboard-style explorer.
// Single-file vanilla JS. Hash-based router so a refresh stays on the right view.
// View functions render into #content; the topbar/nav/scrubber are shared.

(() => {
  'use strict';

  const $ = (id) => document.getElementById(id);
  const FORBIDDEN_CHILD_TAGS = ['script', 'iframe', 'object', 'embed', 'link', 'meta', 'style'];
  const FORBIDDEN_CHILD_TAGS_SET = new Set(FORBIDDEN_CHILD_TAGS.map((t) => t.toUpperCase()));
  const FORBIDDEN_CHILD_SELECTOR = FORBIDDEN_CHILD_TAGS.join(',');
  const SAFE_CHILD_NODE_CACHE = new WeakMap();
  const isSafeChildNode = (node) => {
    if (!(node instanceof Node)) return false;
    const cached = SAFE_CHILD_NODE_CACHE.get(node);
    if (cached !== undefined) return cached;

    let ok = true;
    if (
      node.nodeType !== Node.ELEMENT_NODE &&
      node.nodeType !== Node.TEXT_NODE &&
      node.nodeType !== Node.DOCUMENT_FRAGMENT_NODE
    ) {
      ok = false;
    }

    if (ok && (node.nodeType === Node.ELEMENT_NODE || node.nodeType === Node.DOCUMENT_FRAGMENT_NODE)) {
      // Block forbidden tags on the node itself (ELEMENT_NODE) and anywhere in its subtree.
      if (node.nodeType === Node.ELEMENT_NODE) {
        const tn = (node.tagName || '').toUpperCase();
        if (FORBIDDEN_CHILD_TAGS_SET.has(tn)) ok = false;
      }
      if (ok && typeof node.querySelector === 'function' && node.querySelector(FORBIDDEN_CHILD_SELECTOR)) ok = false;
    }

    SAFE_CHILD_NODE_CACHE.set(node, ok);
    return ok;
  };
  const el = (tag, attrs = {}, ...children) => {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === 'class') n.className = v;
      else if (k === 'text' || k === 'html') n.textContent = String(v); // 'html' kept as deprecated alias
      else if (k.startsWith('on') && typeof v === 'function') n.addEventListener(k.slice(2), v);
      else if (v !== undefined && v !== null) n.setAttribute(k, v);
    }
    for (const c of children) {
      if (c === null || c === undefined || c === false) continue;
      if (typeof c === 'string' || typeof c === 'number' || typeof c === 'boolean' || typeof c === 'bigint') {
        n.appendChild(document.createTextNode(String(c)));
      } else if (isSafeChildNode(c)) {
        n.appendChild(c);
      } else {
        n.appendChild(document.createTextNode(String(c)));
      }
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
    if (parts[0] === 'overview' || parts[0] === 'namespaces' || parts[0] === 'pods' || parts[0] === 'workloads' || parts[0] === 'resources' || parts[0] === 'resource' || parts[0] === 'object' || parts[0] === 'timeline' || parts[0] === 'logs' || parts[0] === 'diff') {
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
      { key: 'workloads',  label: 'Workloads',  hash: '#/workloads' },
      { key: 'pods',       label: 'Pods',       hash: '#/pods' },
      { key: 'resources',  label: 'Resources',  hash: '#/resources' },
      { key: 'timeline',   label: 'Timeline',   hash: '#/timeline' },
    ];
    const activeKey = state.route.name === 'namespace' ? 'namespaces'
      : state.route.name === 'pod' ? 'pods'
      : (state.route.name === 'resource' || state.route.name === 'object') ? 'resources'
      : state.route.name;
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
    for (const n of nodes) if (n) c.appendChild(n);
  }

  function loadingState(msg) {
    return el('div', { class: 'state' }, msg || 'Loading…');
  }
  function errorState(msg) {
    return el('div', { class: 'state' }, 'Error: ' + msg);
  }

  // ── Shared bits ──────────────────────────────────────────────────────────
  function kpi(label, value, opts = {}) {
    const attrs = { class: 'kpi' + (opts.severity ? ' ' + opts.severity : '') + (opts.link ? ' clickable' : '') };
    if (opts.link) attrs.onclick = () => go(opts.link);
    return el('div', attrs,
      el('span', { class: 'label' }, label),
      el('span', { class: 'value' }, formatNumber(value)),
      opts.delta ? el('span', { class: 'delta ' + (opts.deltaKind || 'neutral') }, opts.delta) : null);
  }

  function formatNumber(n) {
    if (typeof n !== 'number') return n;
    return n.toLocaleString('en-US');
  }

  function humanBytes(n) {
    if (!n || n < 0) return '0 B';
    const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    let i = 0, v = n;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return (i === 0 ? v : v.toFixed(1)) + ' ' + u[i];
  }

  function formatDuration(secs) {
    secs = Math.max(0, Math.round(secs || 0));
    const h = Math.floor(secs / 3600), m = Math.floor((secs % 3600) / 60), s = secs % 60;
    if (h) return h + 'h' + (m ? m + 'm' : '');
    if (m) return m + 'm' + (s ? s + 's' : '');
    return s + 's';
  }

  // Capture details card for the overview, fed by /v2/api/capture.
  function captureDetailsCard() {
    const card = el('div', { class: 'card', style: 'margin-bottom:18px;' }, cardHeader('Capture details', ''));
    const body = el('div', {}, loadingState('Loading…'));
    card.appendChild(body);
    getJSON('/v2/api/capture')
      .then((c) => { body.innerHTML = ''; body.appendChild(captureMetaGrid(c)); })
      .catch(() => { body.innerHTML = ''; body.appendChild(el('div', { class: 'state', style: 'padding:14px;' }, 'Capture details unavailable.')); });
    return card;
  }

  function captureMetaGrid(c) {
    const grid = el('div', { class: 'capmeta' });
    const row = (k, v) => {
      if (v === null || v === undefined || v === '') return;
      grid.appendChild(el('div', { class: 'row' }, el('span', { class: 'k' }, k), el('span', { class: 'v' }, String(v))));
    };
    row('Captured', c.captured_at ? new Date(c.captured_at).toLocaleString() : '');
    row('Until', c.captured_until ? new Date(c.captured_until).toLocaleString() : '');
    row('Length', formatDuration(c.duration_seconds));
    row('Kubernetes', c.kubernetes_version);
    row('Server', c.server_address);
    row('Archive size', c.compressed_bytes ? humanBytes(c.compressed_bytes) + ' compressed' : '');
    row('Uncompressed', c.uncompressed_bytes ? humanBytes(c.uncompressed_bytes) : (c.has_config_meta ? '' : 'not recorded'));
    row('Records', formatNumber(c.record_count || 0) + (c.deduplicated_count ? ' · ' + formatNumber(c.deduplicated_count) + ' deduplicated' : ''));
    row('Resource paths', formatNumber(c.resource_paths || 0));
    row('Resource types', formatNumber(c.resource_types || 0));
    row('Namespaces', formatNumber(c.namespaces || 0));
    row('Watch events', formatNumber(c.watch_events || 0));
    if (c.has_config_meta) {
      row('Dynamic discovery', c.auto_discovered ? 'yes' : 'no');
      row('Watch enabled', c.watch_enabled ? 'yes' : 'no');
      if (c.intervals && c.intervals.length) row('Poll interval(s)', c.intervals.join(', '));
      row('Redaction', c.redacted ? ('yes' + ((c.secrets_redacted || c.fields_redacted) ? ' (' + (c.secrets_redacted || 0) + ' secrets, ' + (c.fields_redacted || 0) + ' fields)' : '')) : 'no');
    } else {
      row('Capture config', 'not recorded (captured before this was tracked)');
    }
    row('Archive', c.archive_path);
    return grid;
  }

  // ── Syntax highlighting (dependency-free, builds DOM nodes via textContent
  //    so captured content can never inject markup) ──────────────────────────
  function tok(cls, text) {
    const s = document.createElement('span');
    s.className = cls;
    s.textContent = text;
    return s;
  }

  function highlightJSON(text) {
    const frag = document.createDocumentFragment();
    const re = /("(?:\\.|[^"\\])*")(\s*:)?|\b(true|false|null)\b|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g;
    let last = 0, m;
    while ((m = re.exec(text)) !== null) {
      if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
      if (m[1] !== undefined) {
        const isKey = m[2] !== undefined;
        frag.appendChild(tok(isKey ? 'k-key' : 'k-str', m[1]));
        if (isKey) frag.appendChild(document.createTextNode(m[2]));
      } else if (m[3] !== undefined) {
        frag.appendChild(tok('k-lit', m[3]));
      } else if (m[4] !== undefined) {
        frag.appendChild(tok('k-num', m[4]));
      }
      last = re.lastIndex;
    }
    if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
    return frag;
  }

  function yamlScalar(frag, s) {
    if (s === '') return;
    const t = s.trim();
    if (/^(true|false|null|~)$/i.test(t)) frag.appendChild(tok('k-lit', s));
    else if (/^-?\d+(\.\d+)?$/.test(t)) frag.appendChild(tok('k-num', s));
    else frag.appendChild(tok('k-str', s));
  }

  function highlightYAML(text) {
    const frag = document.createDocumentFragment();
    const lines = text.split('\n');
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      let m;
      if ((m = line.match(/^(\s*)(#.*)$/))) {
        frag.appendChild(document.createTextNode(m[1]));
        frag.appendChild(tok('k-com', m[2]));
      } else if ((m = line.match(/^(\s*)(- )?([^:\s][^:]*?):(\s*)(.*)$/))) {
        frag.appendChild(document.createTextNode(m[1]));
        if (m[2]) frag.appendChild(tok('k-punc', m[2]));
        frag.appendChild(tok('k-key', m[3]));
        frag.appendChild(tok('k-punc', ':'));
        frag.appendChild(document.createTextNode(m[4]));
        yamlScalar(frag, m[5]);
      } else if ((m = line.match(/^(\s*)(- )(.*)$/))) {
        frag.appendChild(document.createTextNode(m[1]));
        frag.appendChild(tok('k-punc', m[2]));
        yamlScalar(frag, m[3]);
      } else {
        yamlScalar(frag, line);
      }
      if (i < lines.length - 1) frag.appendChild(document.createTextNode('\n'));
    }
    return frag;
  }

  // codeBlock renders text as a highlighted <pre>. Highlighting is skipped for
  // very large bodies to keep rendering snappy.
  function codeBlock(text, lang) {
    const pre = el('pre', { class: 'code', style: 'max-height:calc(100vh - 360px); padding:14px;' });
    text = text || '';
    if (text.length > 300000) { pre.textContent = text; return pre; }
    pre.appendChild(lang === 'json' ? highlightJSON(text) : highlightYAML(text));
    return pre;
  }

  function copyToClipboard(text, btn) {
    const flash = () => { const old = btn.textContent; btn.textContent = '✓ Copied'; setTimeout(() => { btn.textContent = old; }, 1200); };
    const fallback = () => {
      const ta = document.createElement('textarea');
      ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
      document.body.appendChild(ta); ta.select();
      try { document.execCommand('copy'); flash(); } catch (_) {}
      document.body.removeChild(ta);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(flash).catch(fallback);
    } else { fallback(); }
  }

  function downloadText(text, filename, mime) {
    const blob = new Blob([text], { type: mime || 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = el('a', { href: url, download: filename });
    document.body.appendChild(a); a.click(); document.body.removeChild(a);
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  // codePanel wraps a codeBlock with Copy / Download actions.
  function codePanel(text, lang, baseName) {
    text = text || '';
    const wrap = el('div', {});
    const copyBtn = el('button', { class: 'btn', onclick: () => copyToClipboard(text, copyBtn) }, 'Copy');
    const dlBtn = el('button', { class: 'btn', onclick: () => downloadText(text, (baseName || 'object') + '.' + lang, lang === 'json' ? 'application/json' : 'application/yaml') }, 'Download');
    wrap.appendChild(el('div', { style: 'display:flex; gap:8px; margin-bottom:10px;' }, copyBtn, dlBtn));
    wrap.appendChild(codeBlock(text, lang));
    return wrap;
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
    const link = transitionLink(t);
    const attrs = { class: 'event ' + cls };
    if (link) { attrs.onclick = () => go(link); attrs.style = 'cursor:pointer;'; }
    return el('div', attrs,
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

    // KPI strip — each tile drills into the relevant view.
    const kpiStrip = el('div', { class: 'kpis' });
    kpiStrip.appendChild(kpi('Namespaces', kpis.namespaces || 0, { link: '#/namespaces' }));
    kpiStrip.appendChild(kpi('Workloads', kpis.workloads || 0, { link: '#/workloads' }));
    kpiStrip.appendChild(kpi('Pods', kpis.pods || 0, { link: '#/pods' }));
    const unhealthyDelta = unhealthyDeltaText(kpis);
    kpiStrip.appendChild(kpi('Unhealthy pods', kpis.unhealthy_pods || 0, {
      severity: (kpis.unhealthy_pods || 0) > 0 ? 'alert' : '',
      delta: unhealthyDelta,
      deltaKind: 'down',
      link: '#/pods?health=unhealthy',
    }));
    kpiStrip.appendChild(kpi('Watch events', kpis.watch_events || 0, {
      delta: data.sparkline && data.sparkline.buckets ? 'over ' + data.sparkline.buckets.length + ' buckets' : '',
      deltaKind: 'neutral',
      link: '#/timeline',
    }));
    root.appendChild(kpiStrip);

    // Capture details (file size, timing, config facts).
    root.appendChild(captureDetailsCard());

    // Spark + issues row
    const sparkCard = el('div', { class: 'card' },
      cardHeader('Resource transitions (capture window)',
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

    // Hint when the Resources filter is hiding some types from this page.
    const banner = resourceFilterBanner();
    if (banner) root.appendChild(banner);

    // Resource tiles (honor the global Resources filter; keep the "+N more" tile).
    root.appendChild(el('div', { class: 'section-title' }, 'Resources captured'));
    const resourceCard = el('div', { class: 'card', style: 'margin-bottom:18px;' });
    const tileGrid = el('div', { class: 'resource-grid' });
    for (const t of (data.resources || [])) {
      if (t.resource && !resourceEnabled(t.resource)) continue;
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
    const recent = (data.recent || []).filter((t) => resourceEnabled(t.resource));
    if (recent.length === 0) {
      recentCard.appendChild(el('div', { class: 'state', style: 'padding:18px;' },
        (data.recent && data.recent.length) ? 'No transitions for the enabled resource types.' : 'No watch events captured.'));
    } else {
      const evWrap = el('div', { class: 'events' });
      for (const t of recent) evWrap.appendChild(recentRow(t));
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
    const hq = new URLSearchParams((location.hash.split('?')[1]) || '');
    const unhealthyOnly = hq.get('health') === 'unhealthy';
    const fb = filterBar({
      placeholder: 'Filter namespaces… (name=, label=value, /regex/)',
      rows: () => list,
      facets: [{ key: 'name', label: 'name', field: (n) => n.name }],
      labels: (n) => n.labels,
      bareFields: (n) => [n.name],
      onChange: () => build(),
    });
    const barWrap = el('div', { style: 'margin-bottom:14px; max-width:680px;' }, fb.el);
    const banner = unhealthyOnly ? el('div', { class: 'state', style: 'padding:10px 14px; margin-bottom:14px; display:flex; gap:12px; align-items:center;' },
      el('span', {}, 'Showing namespaces with unhealthy pods'),
      el('a', { onclick: () => go('#/namespaces'), style: 'cursor:pointer;' }, 'Show all →'),
    ) : null;
    const grid = el('div', { class: 'resource-grid', style: 'grid-template-columns:repeat(4, minmax(0,1fr));' });
    function build() {
      grid.innerHTML = '';
      let shown = 0;
      for (const ns of list) {
        if (unhealthyOnly && !(ns.unhealthy > 0)) continue;
        if (!fb.matches(ns)) continue;
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
        grid.appendChild(el('div', { class: 'state', style: 'grid-column:1/-1;' },
          unhealthyOnly ? 'No namespaces with unhealthy pods at this snapshot.' : 'No namespaces match.'));
      }
    }
    build();
    setContent(barWrap, banner, grid);
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

    // KPIs — Workloads/Pods/Unhealthy drill into the cluster lists scoped to
    // this namespace. VMs/ConfigMaps/Secrets have no list view yet.
    const nsq = encodeURIComponent(d.name);
    const kpiStrip = el('div', { class: 'kpis k6' });
    kpiStrip.appendChild(kpi('Workloads', kpis.workloads || 0, { link: '#/workloads?ns=' + nsq }));
    kpiStrip.appendChild(kpi('Pods', kpis.pods || 0, { link: '#/pods?ns=' + nsq }));
    kpiStrip.appendChild(kpi('Unhealthy pods', kpis.unhealthy_pods || 0, {
      severity: (kpis.unhealthy_pods || 0) > 0 ? 'alert' : '',
      link: '#/pods?ns=' + nsq + '&health=unhealthy',
    }));
    kpiStrip.appendChild(kpi('VirtualMachines', kpis.virtual_machines || 0, { link: '#/resource?resource=virtualmachines&ns=' + nsq }));
    kpiStrip.appendChild(kpi('ConfigMaps', kpis.configmaps || 0, { link: '#/resource?resource=configmaps&ns=' + nsq }));
    kpiStrip.appendChild(kpi('Secrets', kpis.secrets || 0, { link: '#/resource?resource=secrets&ns=' + nsq }));
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

    // Hint when the Resources filter hides some types from this namespace.
    const nsBanner = resourceFilterBanner();
    if (nsBanner) root.appendChild(nsBanner);

    // Workloads + VMs row (honor the global Resources filter, by each row's
    // resource type).
    const wls = (d.workloads || []).filter((w) => resourceEnabled(w.resource));
    const wlCard = el('div', { class: 'card' }, cardHeader('Workloads', String(wls.length)));
    if (wls.length === 0) {
      wlCard.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, (d.workloads || []).length ? 'No workloads for the enabled resource types.' : 'No workloads.'));
    } else {
      for (const w of wls.slice(0, 12)) wlCard.appendChild(resourceRowEl(w));
      if (wls.length > 12) wlCard.appendChild(el('div', { style: 'padding:6px 4px; font-size:11px; color:var(--fg-faint);' }, `+ ${wls.length - 12} more`));
    }
    const vms = (d.vms || []).filter((v) => resourceEnabled(v.resource));
    const vmCard = el('div', { class: 'card' }, cardHeader('VirtualMachines', String(vms.length)));
    if (vms.length === 0) {
      vmCard.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, (d.vms || []).length ? 'No VirtualMachines for the enabled resource types.' : 'No VirtualMachines captured.'));
    } else {
      for (const v of vms.slice(0, 12)) vmCard.appendChild(resourceRowEl(v));
      if (vms.length > 12) vmCard.appendChild(el('div', { style: 'padding:6px 4px; font-size:11px; color:var(--fg-faint);' }, `+ ${vms.length - 12} more`));
    }
    const row2 = el('div', { class: 'grid-2', style: 'margin-bottom:18px;' });
    row2.appendChild(wlCard);
    row2.appendChild(vmCard);
    root.appendChild(row2);

    // Pods table (hidden when "pods" is toggled off in the Resources filter).
    const podsOn = resourceEnabled('pods');
    const podRows = podsOn ? (d.pods || []) : [];
    const podHeader = cardHeader('Pods', `${podRows.length} total · ${kpis.unhealthy_pods || 0} unhealthy`);
    const podCard = el('div', { class: 'card', style: 'margin-bottom:18px;' }, podHeader);
    if (podRows.length === 0) {
      podCard.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, !podsOn ? 'Pods are hidden by your Resources filter.' : 'No pods captured.'));
    } else {
      for (const p of podRows.slice(0, 40)) podCard.appendChild(podRowEl(p));
      if (podRows.length > 40) podCard.appendChild(el('div', { style: 'padding:6px 4px; font-size:11px; color:var(--fg-faint);' }, `+ ${podRows.length - 40} more`));
    }
    root.appendChild(podCard);

    // Other resources tile grid (honors the global Resources filter).
    const nsTiles = (d.resources || []).filter((t) => !t.resource || resourceEnabled(t.resource));
    if (nsTiles.length > 0) {
      root.appendChild(el('div', { class: 'section-title' }, 'Other resources'));
      const tileCard = el('div', { class: 'card', style: 'margin-bottom:18px;' });
      const tileGrid = el('div', { class: 'resource-grid', style: 'grid-template-columns:repeat(6, minmax(0,1fr));' });
      for (const t of nsTiles) tileGrid.appendChild(resourceTile(t));
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

  // Render a workload/VM ResourceRow into a .resrow element. These have no
  // dedicated detail page, so the row is non-interactive unless it carries a
  // link.
  function resourceRowEl(r) {
    const attrs = { class: 'resrow' + (r.link ? '' : ' static') };
    if (r.link) attrs.onclick = () => go(r.link);
    return el('div', attrs,
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

  // One row in a cluster-wide object list (pods / workloads).
  function objRow(o) {
    const attrs = { class: 'objrow' + (o.link ? ' click' : '') };
    if (o.link) attrs.onclick = () => go(o.link);
    return el('div', attrs,
      el('span', { class: 'dot ' + (o.severity || 'neutral') }),
      el('span', { class: 'kind' }, o.kind || ''),
      el('span', { class: 'ns' }, o.namespace || ''),
      el('span', { class: 'nm' }, o.name || ''),
      el('span', { class: 'status ' + (o.severity || '') }, o.status || ''),
      el('span', { class: 'num' }, o.num1 || ''),
      el('span', { class: 'num' }, o.num2 || ''),
    );
  }

  const listFilterStyle = 'background:var(--bg-row); border:1px solid var(--border-strong); color:var(--fg); padding:7px 10px; border-radius:6px; outline:none; min-width:280px;';

  function hashQuery() {
    const i = (location.hash || '').indexOf('?');
    return new URLSearchParams(i < 0 ? '' : location.hash.slice(i + 1));
  }

  // ── Chip / token filter bar ────────────────────────────────────────────────
  // opts: {
  //   rows: () => [row],                         // full dataset (for suggestions)
  //   facets: [{ key, aliases?, label, field(row)->string }],
  //   labels: (row) => ({k:v}),                  // label map for label selectors
  //   bareFields(row)->[string],                 // fields a bare term matches
  //   placeholder?, initial?: [{key,value}], onChange(),
  // }
  // Semantics: chips AND together. A value wrapped in /.../ is a case-insensitive
  // regex. Facet and bare terms are case-insensitive substrings; a label
  // selector (any non-facet key=value, e.g. app.kubernetes.io/name=web) is an
  // exact (case-insensitive) match on metadata.labels[key]. Suggestions are
  // aggregated: each facet/label's value list is scoped by the other chips.
  function filterBar(opts) {
    const facets = opts.facets || [];
    const byKey = {};
    for (const f of facets) { byKey[f.key] = f; for (const a of (f.aliases || [])) byKey[a] = f; }
    // Resolve a typed key to a facet key, else keep it verbatim (label keys are
    // case-sensitive, so don't lowercase those).
    const resolveKey = (k) => { const f = byKey[k.toLowerCase()]; return f ? f.key : k; };
    const labelsOf = (row) => (opts.labels ? (opts.labels(row) || {}) : {});

    let chips = [];
    let suggestions = [];
    let sel = -1;

    const chipHost = el('span', { class: 'fb-chips' });
    const input = el('input', { class: 'fb-input', placeholder: opts.placeholder || 'Filter… (ns=, name=, label=value, /regex/)' });
    const drop = el('div', { class: 'fb-suggest', style: 'display:none;' });
    const wrap = el('div', { class: 'filterbar', onclick: () => input.focus() }, chipHost, input, drop);

    function makeChip(key, value) {
      let regex = false, v = value;
      if (v.length >= 2 && v[0] === '/' && v[v.length - 1] === '/') { regex = true; v = v.slice(1, -1); }
      return { key, value: v, regex, label: (key ? key + '=' : '') + value };
    }
    function renderChips() {
      chipHost.innerHTML = '';
      chips.forEach((c, i) => {
        chipHost.appendChild(el('span', { class: 'fb-chip' }, c.label,
          el('span', { class: 'fb-x', title: 'Remove', onclick: (e) => { e.stopPropagation(); chips.splice(i, 1); renderChips(); fire(); } }, '✕')));
      });
    }
    function fire() { if (opts.onChange) opts.onChange(); }
    function addChip(key, value) {
      if (value === '') return;
      chips.push(makeChip(key, value));
      renderChips(); input.value = ''; closeDrop(); fire();
    }

    const test = (c, str, exact) => {
      str = String(str == null ? '' : str);
      if (c.regex) { try { return new RegExp(c.value, 'i').test(str); } catch (_) { return false; } }
      if (exact) return str.toLowerCase() === c.value.toLowerCase();
      return str.toLowerCase().includes(c.value.toLowerCase());
    };
    function chipMatches(c, row) {
      if (c.key && byKey[c.key]) return test(c, byKey[c.key].field(row), false);
      if (c.key) { const lv = labelsOf(row)[c.key]; return lv !== undefined && test(c, lv, true); }
      for (const fv of (opts.bareFields ? opts.bareFields(row) : [])) if (test(c, fv, false)) return true;
      return false;
    }
    const matchesChips = (row) => { for (const c of chips) if (!chipMatches(c, row)) return false; return true; };
    const scopedRows = () => (opts.rows ? opts.rows() : []).filter(matchesChips);

    function distinct(values, partial, cap) {
      const seen = new Set(), out = [];
      for (let v of values) {
        if (v == null || v === '') continue;
        v = String(v);
        if (partial && !v.toLowerCase().includes(partial)) continue;
        if (seen.has(v)) continue;
        seen.add(v); out.push(v);
        if (out.length >= cap) break;
      }
      return out;
    }
    function labelKeys(rows) {
      const set = new Set();
      for (const r of rows) for (const k in labelsOf(r)) set.add(k);
      return [...set].sort();
    }

    function updateSuggestions() {
      const tok = input.value;
      suggestions = [];
      const eq = tok.indexOf('=');
      const scope = scopedRows();
      if (eq < 0) {
        const p = tok.toLowerCase();
        const keyMatch = [], labelMatch = [];
        for (const f of facets) {
          if (!p || f.key.startsWith(p)) keyMatch.push(f);
          else if ((f.label || '').toLowerCase().startsWith(p)) labelMatch.push(f);
        }
        for (const f of keyMatch.concat(labelMatch)) suggestions.push({ type: 'key', label: f.key + '=', hint: f.label, key: f.key });
        for (const lk of labelKeys(scope)) {
          if (suggestions.length >= 16) break;
          if (!p || lk.toLowerCase().includes(p)) suggestions.push({ type: 'key', label: lk + '=', hint: 'label', key: lk });
        }
      } else {
        const key = resolveKey(tok.slice(0, eq));
        const partial = tok.slice(eq + 1).toLowerCase().replace(/^\/|\/$/g, '');
        const f = byKey[key];
        const vals = f ? scope.map(f.field) : scope.map((r) => labelsOf(r)[key]);
        for (const v of distinct(vals, partial, 12)) suggestions.push({ type: 'value', label: v, key: f ? f.key : key, value: v });
      }
      sel = -1; // no auto-highlight: Enter commits typed text unless a suggestion is chosen
      renderDrop();
    }
    function renderDrop() {
      drop.innerHTML = '';
      if (!suggestions.length) { drop.style.display = 'none'; return; }
      suggestions.forEach((sg, i) => {
        drop.appendChild(el('div', { class: 'fb-opt' + (i === sel ? ' active' : ''), onmousedown: (e) => { e.preventDefault(); choose(i); } },
          el('span', {}, sg.label), sg.hint ? el('span', { class: 'fb-hint' }, sg.hint) : null));
      });
      drop.style.display = '';
    }
    function closeDrop() { suggestions = []; sel = -1; drop.style.display = 'none'; }
    function choose(i) {
      const sg = suggestions[i];
      if (!sg) return;
      if (sg.type === 'key') { input.value = sg.key + '='; updateSuggestions(); input.focus(); }
      else addChip(sg.key, sg.value);
    }
    function commitInput() {
      const tok = input.value.trim();
      if (!tok) return;
      const eq = tok.indexOf('=');
      if (eq > 0) addChip(resolveKey(tok.slice(0, eq)), tok.slice(eq + 1));
      else addChip('', tok);
    }

    input.addEventListener('input', updateSuggestions);
    input.addEventListener('focus', updateSuggestions);
    input.addEventListener('blur', () => setTimeout(closeDrop, 120));
    input.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowDown' && suggestions.length) { sel = (sel + 1) % suggestions.length; renderDrop(); e.preventDefault(); }
      else if (e.key === 'ArrowUp' && suggestions.length) { sel = (sel - 1 + suggestions.length) % suggestions.length; renderDrop(); e.preventDefault(); }
      else if (e.key === 'Tab' && suggestions.length) { e.preventDefault(); choose(sel >= 0 ? sel : 0); }
      else if (e.key === 'Enter') {
        if (sel >= 0 && suggestions.length && suggestions[sel].type === 'value') { e.preventDefault(); choose(sel); }
        else { e.preventDefault(); commitInput(); }
      } else if (e.key === 'Escape') { closeDrop(); }
      else if (e.key === 'Backspace' && input.value === '' && chips.length) { chips.pop(); renderChips(); fire(); }
    });

    for (const c of (opts.initial || [])) { if (c && c.value) chips.push(makeChip(resolveKey(c.key || ''), c.value)); }
    renderChips();

    return {
      el: wrap,
      chips: () => chips,
      matches: matchesChips,
    };
  }

  async function renderPodsList() {
    setContent(loadingState('Loading pods…'));
    let data;
    try {
      data = await getJSON('/v2/api/pods');
    } catch (e) {
      setContent(errorState(e.message));
      return;
    }
    const all = data.pods || [];
    const q = hashQuery();
    const nsExact = q.get('ns') || '';
    let unhealthyOnly = q.get('health') === 'unhealthy';

    const root = el('div', {});
    root.appendChild(el('div', { class: 'section-title', style: 'font-size:15px; margin-bottom:2px;' }, 'Pods'));
    const sub = el('div', { style: 'color:var(--fg-dim); font-size:12px; margin-bottom:14px;' });
    root.appendChild(sub);

    const toggle = el('button', { class: 'btn' });
    const fb = filterBar({
      placeholder: 'Filter pods… (ns=, name=, status=, label=value, /regex/)',
      rows: () => all,
      facets: [
        { key: 'ns', aliases: ['namespace'], label: 'namespace', field: (p) => p.namespace },
        { key: 'name', label: 'name', field: (p) => p.name },
        { key: 'status', label: 'status', field: (p) => p.status || p.phase },
      ],
      labels: (p) => p.labels,
      bareFields: (p) => [p.name, p.namespace, p.status, p.phase],
      initial: nsExact ? [{ key: 'ns', value: nsExact }] : [],
      onChange: () => build(),
    });
    root.appendChild(el('div', { style: 'display:flex; gap:10px; align-items:center; margin-bottom:12px;' }, fb.el, toggle));

    const podsBanner = resourceFilterBanner();
    if (podsBanner) root.appendChild(podsBanner);

    const card = el('div', { class: 'card' });
    card.appendChild(el('div', { class: 'objhead' },
      el('span', {}, ''), el('span', {}, 'Kind'), el('span', {}, 'Namespace'), el('span', {}, 'Name'),
      el('span', {}, 'Status'), el('span', { style: 'text-align:right;' }, 'Restarts'), el('span', { style: 'text-align:right;' }, 'Age')));
    const listWrap = el('div', {});
    card.appendChild(listWrap);
    root.appendChild(card);

    function refreshToggle() {
      toggle.className = 'btn' + (unhealthyOnly ? ' primary' : '');
      toggle.textContent = (unhealthyOnly ? '✓ ' : '') + 'Unhealthy only';
    }
    function build() {
      listWrap.innerHTML = '';
      let shown = 0;
      const podsOn = resourceEnabled('pods');
      for (const p of all) {
        if (!podsOn) break;
        if (unhealthyOnly && !p.unhealthy) continue;
        if (!fb.matches(p)) continue;
        const status = (p.status || p.phase || '') + (p.ready ? ' · ' + p.ready : '');
        listWrap.appendChild(objRow({ severity: p.severity, kind: 'Pod', namespace: p.namespace, name: p.name, status, num1: p.restarts ? String(p.restarts) : '', num2: p.age || '', link: p.link }));
        shown++;
      }
      if (!shown) listWrap.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, !podsOn ? 'Pods are hidden by your Resources filter.' : (unhealthyOnly ? 'No unhealthy pods match.' : 'No pods match.')));
      sub.textContent = data.total + ' pods · ' + data.unhealthy + ' unhealthy · ' + shown + ' shown';
    }
    toggle.addEventListener('click', () => { unhealthyOnly = !unhealthyOnly; refreshToggle(); build(); });
    refreshToggle();
    build();
    setContent(root);
  }

  async function renderWorkloadsList() {
    setContent(loadingState('Loading workloads…'));
    let data;
    try {
      data = await getJSON('/v2/api/workloads');
    } catch (e) {
      setContent(errorState(e.message));
      return;
    }
    const all = data.workloads || [];
    const q = hashQuery();
    const nsExact = q.get('ns') || '';

    const root = el('div', {});
    root.appendChild(el('div', { class: 'section-title', style: 'font-size:15px; margin-bottom:2px;' }, 'Workloads'));
    const sub = el('div', { style: 'color:var(--fg-dim); font-size:12px; margin-bottom:14px;' });
    root.appendChild(sub);

    const fb = filterBar({
      placeholder: 'Filter workloads… (ns=, kind=, name=, label=value, /regex/)',
      rows: () => all,
      facets: [
        { key: 'ns', aliases: ['namespace'], label: 'namespace', field: (w) => w.namespace },
        { key: 'kind', label: 'kind', field: (w) => w.kind },
        { key: 'name', label: 'name', field: (w) => w.name },
      ],
      labels: (w) => w.labels,
      bareFields: (w) => [w.name, w.namespace, w.kind, w.status],
      initial: nsExact ? [{ key: 'ns', value: nsExact }] : [],
      onChange: () => build(),
    });
    root.appendChild(el('div', { style: 'display:flex; gap:10px; align-items:center; margin-bottom:12px;' }, fb.el));

    const wlBanner = resourceFilterBanner();
    if (wlBanner) root.appendChild(wlBanner);

    const card = el('div', { class: 'card' });
    card.appendChild(el('div', { class: 'objhead' },
      el('span', {}, ''), el('span', {}, 'Kind'), el('span', {}, 'Namespace'), el('span', {}, 'Name'),
      el('span', {}, 'Status'), el('span', { style: 'text-align:right;' }, ''), el('span', { style: 'text-align:right;' }, 'Age')));
    const listWrap = el('div', {});
    card.appendChild(listWrap);
    root.appendChild(card);

    function build() {
      listWrap.innerHTML = '';
      let shown = 0;
      for (const w of all) {
        if (!resourceEnabled(w.resource)) continue;
        if (!fb.matches(w)) continue;
        listWrap.appendChild(objRow({ severity: w.severity, kind: w.kind, namespace: w.namespace, name: w.name, status: w.status, num1: w.restarts ? String(w.restarts) : '', num2: w.age || '', link: w.link }));
        shown++;
      }
      if (!shown) listWrap.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, 'No workloads match.'));
      sub.textContent = data.total + ' workloads · ' + shown + ' shown';
    }
    build();
    setContent(root);
  }

  // parseListPath extracts {ns, resource} from an API list path so the object
  // view can show a breadcrumb.
  function parseListPath(p) {
    const parts = (p || '').replace(/^\//, '').split('/').filter(Boolean);
    const i = parts.indexOf('namespaces');
    if (i >= 0 && parts.length > i + 2) return { ns: parts[i + 1], resource: parts[i + 2] };
    return { ns: '', resource: parts[parts.length - 1] || '' };
  }

  // transitionLink maps a watch-event/transition to its best destination:
  // the pod detail for pods, otherwise the generic object view.
  function transitionLink(t) {
    if (t.resource === 'pods' && t.namespace && t.name) {
      return '#/ns/' + encodeURIComponent(t.namespace) + '/pod/' + encodeURIComponent(t.name);
    }
    if (t.path && t.name) {
      return '#/object?path=' + encodeURIComponent(t.path) + '&name=' + encodeURIComponent(t.name);
    }
    return '';
  }

  // rawObjectPanel fetches one object and shows it as highlighted YAML/JSON
  // with a toggle.
  function rawObjectPanel(path, name) {
    const wrap = el('div', {});
    const host = el('div', {}, loadingState('Loading…'));
    let data = null, mode = 'yaml';
    const ybtn = el('button', { class: 'btn primary', onclick: () => { mode = 'yaml'; paint(); } }, 'YAML');
    const jbtn = el('button', { class: 'btn', onclick: () => { mode = 'json'; paint(); } }, 'JSON');
    const curText = () => data ? (mode === 'yaml' ? data.yaml : data.json) || '' : '';
    const copyBtn = el('button', { class: 'btn', onclick: () => copyToClipboard(curText(), copyBtn) }, 'Copy');
    const dlBtn = el('button', { class: 'btn', onclick: () => downloadText(curText(), (name || 'object') + '.' + mode, mode === 'json' ? 'application/json' : 'application/yaml') }, 'Download');
    function paint() {
      host.innerHTML = '';
      if (!data) return;
      if (!data.found) { host.appendChild(el('div', { class: 'state', style: 'padding:14px;' }, 'Object not present at this snapshot.')); return; }
      host.appendChild(codeBlock(mode === 'yaml' ? data.yaml : data.json, mode));
      ybtn.className = 'btn' + (mode === 'yaml' ? ' primary' : '');
      jbtn.className = 'btn' + (mode === 'json' ? ' primary' : '');
    }
    wrap.appendChild(el('div', { style: 'display:flex; gap:8px; align-items:center; justify-content:space-between; margin-bottom:10px;' },
      el('div', { style: 'display:flex; gap:8px;' }, ybtn, jbtn),
      el('div', { style: 'display:flex; gap:8px;' }, copyBtn, dlBtn)));
    wrap.appendChild(host);
    getJSON('/v2/api/object?path=' + encodeURIComponent(path) + (name ? '&name=' + encodeURIComponent(name) : ''))
      .then((d) => { data = d; paint(); })
      .catch((e) => { host.innerHTML = ''; host.appendChild(errorState(e.message)); });
    return wrap;
  }

  // relatedRow renders a "<label>: <kind/name>" timeline row, linking the
  // value to the object/pod view when the item carries a link.
  function relatedRow(label, item) {
    const text = (item.kind ? item.kind + '/' : '') + (item.name || '');
    const val = item.link
      ? el('a', { style: 'cursor:pointer; color:var(--accent);', onclick: () => go(item.link) }, text)
      : el('span', {}, text);
    return el('div', { class: 'timeline-row' },
      el('span', { class: 'ts' }, label),
      el('span', { class: 'ev' }, val));
  }

  // objectRelationshipsPanel fetches and renders the related-objects groups for
  // an arbitrary captured object (PV↔PVC↔Pod, ConfigMap/Secret→Pod, owners…).
  function objectRelationshipsPanel(path, name) {
    const card = el('div', { class: 'card' }, cardHeader('Relationships', ''));
    const body = el('div', {}, loadingState('Loading relationships…'));
    card.appendChild(body);
    getJSON('/v2/api/object-relationships?path=' + encodeURIComponent(path) + (name ? '&name=' + encodeURIComponent(name) : ''))
      .then((d) => {
        body.innerHTML = '';
        const groups = (d && d.groups) || [];
        if (!groups.length) { body.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, 'No related objects found.')); return; }
        for (const g of groups) {
          body.appendChild(el('div', { class: 'section-title', style: 'margin-top:10px;' }, g.title + ' (' + (g.items || []).length + ')'));
          for (const it of (g.items || [])) body.appendChild(relatedRow(it.kind || '', it));
        }
      })
      .catch((e) => { body.innerHTML = ''; body.appendChild(errorState(e.message)); });
    return card;
  }

  function objectHistoryPanel(path, name) {
    const card = el('div', { class: 'card' }, cardHeader('Watch event history', ''));
    const body = el('div', {}, loadingState('Loading history…'));
    card.appendChild(body);
    getJSON('/v2/api/object-history?path=' + encodeURIComponent(path) + (name ? '&name=' + encodeURIComponent(name) : ''))
      .then((h) => {
        body.innerHTML = '';
        const events = (h && h.events) || [];
        if (!events.length) { body.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, 'No watch events recorded for this object. (Enable watch on its resource to capture transitions.)')); return; }
        for (const e of events) {
          const cls = e.event_type === 'ADDED' ? 'added' : (e.event_type === 'DELETED' ? 'deleted' : 'modified');
          body.appendChild(el('div', { class: 'timeline-row' },
            el('span', { class: 'ts' }, formatShortTS(e.time)),
            el('span', { class: 'dot ' + cls }),
            el('span', { class: 'ev' }, e.event_type + (e.detail ? ' · ' + e.detail : ''))));
        }
      })
      .catch((e) => { body.innerHTML = ''; body.appendChild(errorState(e.message)); });
    return card;
  }

  function objectDiffPanel(path, name) {
    const card = el('div', { class: 'card' }, cardHeader('Compare snapshots', ''));
    if (state.snapshots.length < 2) {
      card.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, 'Need at least two snapshots to diff.'));
      return card;
    }
    const mkSel = (val) => {
      const s = el('select', { style: listFilterStyle });
      for (const ts of state.snapshots) s.appendChild(el('option', { value: ts, selected: ts === val ? 'selected' : null }, ts));
      return s;
    };
    const before = mkSel(state.snapshots[0]);
    const after = mkSel(state.snapshots[state.snapshots.length - 1]);
    const out = el('div', { style: 'margin-top:12px;' });
    const run = async () => {
      out.innerHTML = '';
      out.appendChild(loadingState('Diffing…'));
      try {
        const data = await getJSON('/v2/api/diff?path=' + encodeURIComponent(path) + (name ? '&name=' + encodeURIComponent(name) : '') + '&before=' + encodeURIComponent(before.value) + '&after=' + encodeURIComponent(after.value));
        out.innerHTML = '';
        if (data.before_missing || data.after_missing) {
          out.appendChild(el('div', { class: 'state' }, data.before_missing && data.after_missing ? 'Object not present at either snapshot.' : (data.before_missing ? 'Did not exist at the "before" snapshot — full add.' : 'Did not exist at the "after" snapshot — full delete.')));
        }
        if (!data.has_diff && !data.before_missing && !data.after_missing) {
          out.appendChild(el('div', { class: 'state' }, 'No changes between these snapshots.'));
        }
        const diff = el('div', { class: 'diff', style: 'padding:14px;' });
        for (const hk of (data.hunks || [])) diff.appendChild(el('div', { class: hk.type === 'add' ? 'add' : (hk.type === 'del' ? 'del' : 'ctx') }, hk.text));
        out.appendChild(diff);
      } catch (e) {
        out.innerHTML = '';
        out.appendChild(errorState(e.message));
      }
    };
    card.appendChild(el('div', { style: 'display:flex; gap:8px; align-items:center; flex-wrap:wrap;' },
      el('span', { style: 'font-size:12px; color:var(--fg-dim);' }, 'Before'), before,
      el('span', { style: 'font-size:12px; color:var(--fg-dim);' }, 'After'), after,
      el('button', { class: 'btn primary', onclick: run }, 'Compare')));
    card.appendChild(out);
    return card;
  }

  // Generic object view: YAML / JSON / History / Diff for any captured object.
  async function renderObject() {
    const q = hashQuery();
    const path = q.get('path') || '';
    const name = q.get('name') || '';
    if (!path) { setContent(errorState('Missing object path.')); return; }
    setContent(loadingState('Loading ' + (name || path) + '…'));
    let d;
    try {
      d = await getJSON('/v2/api/object?path=' + encodeURIComponent(path) + (name ? '&name=' + encodeURIComponent(name) : ''));
    } catch (e) {
      setContent(errorState(e.message));
      return;
    }
    const parsed = parseListPath(path);
    const kind = d.kind || kindLabelFor(parsed.resource);
    const root = el('div', {});
    root.appendChild(el('div', { class: 'hero', style: 'margin:-18px -20px 18px; padding:14px 20px;' },
      el('div', { class: 'crumbs' },
        el('a', { onclick: () => go('#/overview') }, '← Cluster'),
        el('span', { class: 'sep' }, '/'),
        parsed.ns ? el('a', { onclick: () => go('#/ns/' + encodeURIComponent(parsed.ns)) }, parsed.ns) : null,
        parsed.ns ? el('span', { class: 'sep' }, '/') : null,
        el('a', { onclick: () => go('#/resource?resource=' + encodeURIComponent(parsed.resource) + (parsed.ns ? '&ns=' + encodeURIComponent(parsed.ns) : '')) }, kind),
        el('span', { class: 'sep' }, '/'),
        el('b', {}, name || parsed.resource),
      ),
      el('div', { class: 'hero-row' }, el('div', {},
        el('div', { class: 'title' }, name || parsed.resource),
        el('div', { class: 'sub' }, kind + (parsed.ns ? ' · ' + parsed.ns : '') + (d.found ? '' : ' · not present at this snapshot')))),
    ));
    if (!d.found) { root.appendChild(el('div', { class: 'state', style: 'padding:24px;' }, 'Object not present at this snapshot.')); setContent(root); return; }

    const baseName = name || parsed.resource;
    const panelDefs = [
      { key: 'yaml', label: 'YAML', build: () => codePanel(d.yaml || '', 'yaml', baseName) },
      { key: 'json', label: 'JSON', build: () => codePanel(d.json || '', 'json', baseName) },
      { key: 'relationships', label: 'Relationships', build: () => objectRelationshipsPanel(path, name) },
      { key: 'history', label: 'History', build: () => objectHistoryPanel(path, name) },
      { key: 'diff', label: 'Diff', build: () => objectDiffPanel(path, name) },
    ];
    const subtabs = el('div', { class: 'subtabs', style: 'margin:0 -20px 18px;' });
    const host = el('div', {});
    const tabEls = {}, panelEls = {};
    const select = (k) => {
      for (const def of panelDefs) {
        const a = def.key === k;
        tabEls[def.key].classList.toggle('active', a);
        if (a && !panelEls[def.key]) { panelEls[def.key] = def.build(); host.appendChild(panelEls[def.key]); }
        if (panelEls[def.key]) panelEls[def.key].style.display = a ? '' : 'none';
      }
    };
    for (const def of panelDefs) {
      const t = el('div', { class: 'tab', onclick: () => select(def.key) }, def.label);
      tabEls[def.key] = t;
      subtabs.appendChild(t);
    }
    root.appendChild(subtabs);
    root.appendChild(host);
    select('yaml');
    setContent(root);
  }

  // Small JS-side mirror of kindFromResource for headers when the captured
  // object has no top-level kind (List items often omit it).
  function kindLabelFor(resource) {
    const m = { pods: 'Pod', configmaps: 'ConfigMap', secrets: 'Secret', services: 'Service', deployments: 'Deployment', statefulsets: 'StatefulSet', daemonsets: 'DaemonSet', replicasets: 'ReplicaSet', jobs: 'Job', cronjobs: 'CronJob', persistentvolumeclaims: 'PersistentVolumeClaim', virtualmachines: 'VirtualMachine', virtualmachineinstances: 'VirtualMachineInstance', namespaces: 'Namespace', nodes: 'Node', events: 'Event' };
    return m[resource] || resource;
  }

  // Generic resource-type list: every object of a resource kind, optionally
  // scoped to a namespace. Rows open the object view.
  async function renderResourceList() {
    const q = hashQuery();
    const resource = q.get('resource') || '';
    const nsParam = q.get('ns') || '';
    if (!resource) { setContent(errorState('Missing resource.')); return; }
    setContent(loadingState('Loading ' + resource + '…'));
    let d;
    try {
      d = await getJSON('/v2/api/resource?resource=' + encodeURIComponent(resource) + (nsParam ? '&ns=' + encodeURIComponent(nsParam) : ''));
    } catch (e) {
      setContent(errorState(e.message));
      return;
    }
    const all = d.items || [];
    const root = el('div', {});
    root.appendChild(el('div', { class: 'section-title', style: 'font-size:15px; margin-bottom:2px;' }, d.kind || resource));
    const sub = el('div', { style: 'color:var(--fg-dim); font-size:12px; margin-bottom:14px;' });
    root.appendChild(sub);
    if (!resourceEnabled(resource)) {
      sub.textContent = d.total + ' ' + resource + ' · hidden';
      root.appendChild(el('div', { class: 'state', style: 'padding:24px; display:flex; gap:12px; align-items:center;' },
        el('span', {}, (d.kind || resource) + ' is hidden by your Resources filter.'),
        el('a', { onclick: () => go('#/resources'), style: 'cursor:pointer;' }, 'Manage resources →')));
      setContent(root);
      return;
    }
    const fb = filterBar({
      placeholder: 'Filter ' + resource + '… (ns=, name=, label=value, /regex/)',
      rows: () => all,
      facets: [
        { key: 'ns', aliases: ['namespace'], label: 'namespace', field: (it) => it.namespace },
        { key: 'name', label: 'name', field: (it) => it.name },
      ],
      labels: (it) => it.labels,
      bareFields: (it) => [it.name, it.namespace],
      initial: nsParam ? [{ key: 'ns', value: nsParam }] : [],
      onChange: () => build(),
    });
    root.appendChild(el('div', { style: 'display:flex; gap:10px; margin-bottom:12px;' }, fb.el));
    const gridCols = 'grid-template-columns:200px 1fr 60px;';
    const card = el('div', { class: 'card' });
    card.appendChild(el('div', { class: 'objhead', style: gridCols }, el('span', {}, 'Namespace'), el('span', {}, 'Name'), el('span', { style: 'text-align:right;' }, 'Age')));
    const listWrap = el('div', {});
    card.appendChild(listWrap);
    root.appendChild(card);
    function build() {
      listWrap.innerHTML = '';
      let shown = 0;
      for (const it of all) {
        if (!fb.matches(it)) continue;
        listWrap.appendChild(el('div', { class: 'objrow click', style: gridCols, onclick: () => go(it.link) },
          el('span', { class: 'ns' }, it.namespace || '—'),
          el('span', { class: 'nm' }, it.name || ''),
          el('span', { class: 'num' }, it.age || '')));
        shown++;
      }
      if (!shown) listWrap.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, 'No objects match.'));
      sub.textContent = d.total + ' ' + resource + ' · ' + shown + ' shown';
    }
    build();
    setContent(root);
  }

  // Resources catalog: every captured API resource type, grouped by API group,
  // with per-resource toggles (persisted, v1-sidebar style) and links to the
  // resource list.
  const RES_ENABLED_KEY = 'kshrk.v2.resources.enabled';
  function loadEnabledResources() {
    try { const s = localStorage.getItem(RES_ENABLED_KEY); if (s) return new Set(JSON.parse(s)); } catch (_) {}
    return null; // null means "all enabled"
  }
  function saveEnabledResources(set) {
    try { if (set) localStorage.setItem(RES_ENABLED_KEY, JSON.stringify(Array.from(set))); else localStorage.removeItem(RES_ENABLED_KEY); } catch (_) {}
  }
  // resourceEnabled reports whether a resource type is currently toggled on in
  // the Resources catalog (used to globally filter other views). A null set
  // (no customization, or "All") means everything is enabled.
  function resourceEnabled(resource) {
    if (!resource) return true;
    const set = loadEnabledResources();
    return !set || set.has(resource);
  }
  function resourceFilterActive() {
    return loadEnabledResources() !== null;
  }
  // A small banner linking to the catalog, shown on views that honor the
  // resource filter when it's active, so hidden data is discoverable.
  function resourceFilterBanner() {
    if (!resourceFilterActive()) return null;
    return el('div', { class: 'state', style: 'padding:9px 14px; margin-bottom:14px; display:flex; gap:12px; align-items:center; font-size:12px;' },
      el('span', {}, 'Some resource types are hidden by your Resources filter.'),
      el('a', { onclick: () => go('#/resources'), style: 'cursor:pointer;' }, 'Manage →'));
  }

  async function renderResourceCatalog() {
    setContent(loadingState('Loading resources…'));
    let d;
    try {
      d = await getJSON('/v2/api/resources');
    } catch (e) {
      setContent(errorState(e.message));
      return;
    }
    const all = d.resources || [];
    let enabled = loadEnabledResources();
    const isOn = (res) => !enabled || enabled.has(res);

    const root = el('div', {});
    root.appendChild(el('div', { class: 'section-title', style: 'font-size:15px; margin-bottom:2px;' }, 'API resources'));
    const sub = el('div', { style: 'color:var(--fg-dim); font-size:12px; margin-bottom:14px;' });
    root.appendChild(sub);

    const allBtn = el('button', { class: 'btn' }, 'All');
    const noneBtn = el('button', { class: 'btn' }, 'None');
    const fb = filterBar({
      placeholder: 'Filter resources… (kind=, group=, resource=, namespaced=, /regex/)',
      rows: () => all,
      facets: [
        { key: 'kind', label: 'kind', field: (r) => r.kind },
        { key: 'group', label: 'group', field: (r) => r.group || 'core' },
        { key: 'resource', label: 'resource', field: (r) => r.resource },
        { key: 'namespaced', label: 'namespaced', field: (r) => (r.namespaced ? 'true' : 'false') },
      ],
      bareFields: (r) => [r.resource, r.kind, r.singular, r.group || 'core', ...(r.short_names || [])],
      onChange: () => build(),
    });
    root.appendChild(el('div', { style: 'display:flex; gap:10px; align-items:center; margin-bottom:8px;' }, fb.el, allBtn, noneBtn));

    const body = el('div', {});
    root.appendChild(body);

    function build() {
      body.innerHTML = '';
      const groups = {};
      for (const r of all) {
        if (!fb.matches(r)) continue;
        const g = r.group || 'core';
        (groups[g] = groups[g] || []).push(r);
      }
      const gnames = Object.keys(groups).sort();
      let shown = 0, enabledCount = 0;
      const setGroup = (rows, on) => {
        if (!enabled) enabled = new Set(all.map((x) => x.resource));
        for (const r of rows) { if (on) enabled.add(r.resource); else enabled.delete(r.resource); }
        saveEnabledResources(enabled);
        build();
      };
      for (const g of gnames) {
        const rows = groups[g];
        const onInGroup = rows.filter((r) => isOn(r.resource)).length;
        const gcb = el('input', { type: 'checkbox' });
        gcb.checked = onInGroup === rows.length;
        gcb.indeterminate = onInGroup > 0 && onInGroup < rows.length;
        gcb.addEventListener('change', () => setGroup(rows, gcb.checked));
        body.appendChild(el('div', { class: 'cat-group' },
          gcb,
          el('span', { class: 'g', onclick: () => setGroup(rows, onInGroup !== rows.length) }, g),
          el('span', { class: 'gc' }, onInGroup + '/' + rows.length + ' enabled')));
        for (const r of rows) {
          shown++;
          if (isOn(r.resource)) enabledCount++;
          const cb = el('input', { type: 'checkbox' });
          cb.checked = isOn(r.resource);
          cb.addEventListener('change', () => {
            if (!enabled) enabled = new Set(all.map((x) => x.resource));
            if (cb.checked) enabled.add(r.resource); else enabled.delete(r.resource);
            saveEnabledResources(enabled);
            build();
          });
          body.appendChild(el('div', { class: 'cat-row' + (isOn(r.resource) ? '' : ' off') },
            cb,
            el('span', { class: 'kind' }, r.kind),
            el('span', { class: 'nm', style: 'cursor:pointer;', onclick: () => go(r.link) },
              r.resource,
              (r.short_names && r.short_names.length) ? el('span', { style: 'color:var(--fg-faint); margin-left:6px;' }, '(' + r.short_names.join(', ') + ')') : null),
            el('span', { class: 'meta' }, (r.group ? r.group + '/' : '') + r.version),
            el('span', { class: 'badge' }, r.namespaced ? 'namespaced' : 'cluster'),
            el('span', { class: 'num' }, r.count ? formatNumber(r.count) : ''),
            el('a', { class: 'go', style: 'cursor:pointer;', onclick: () => go(r.link) }, '→'),
          ));
        }
      }
      if (!shown) body.appendChild(el('div', { class: 'state', style: 'padding:18px;' }, 'No resources match.'));
      sub.textContent = d.total + ' resource types · ' + enabledCount + ' enabled';
    }
    allBtn.addEventListener('click', () => { enabled = null; saveEnabledResources(enabled); build(); });
    noneBtn.addEventListener('click', () => { enabled = new Set(); saveEnabledResources(enabled); build(); });
    build();
    setContent(root);
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

    // metaGrid lays out a group of key/value rows in a single shared grid so
    // the key column auto-sizes to the widest key in the group (no fixed width)
    // while keys stay aligned across rows. Values wrap in the remaining space.
    const metaGrid = (rows) => {
      const kids = [];
      for (const r of rows) {
        if (!r) continue;
        kids.push(el('span', { style: 'color:var(--fg-faint); white-space:nowrap;' }, r.k));
        kids.push(el('span', { style: 'font-family:var(--mono); color:' + (r.color || 'var(--fg)') + '; overflow-wrap:anywhere; min-width:0;' }, r.v));
      }
      return el('div', { style: 'display:grid; grid-template-columns:max-content 1fr; gap:6px 12px; font-size:12.5px; align-items:start;' }, ...kids);
    };

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
      const metaRows = [];
      if (d.related?.owner) metaRows.push({ k: 'Owner', v: (d.related.owner.kind || '') + '/' + (d.related.owner.name || '') });
      if (d.kpis?.node) metaRows.push({ k: 'Node', v: d.kpis.node });
      if (d.kpis?.pod_ip) metaRows.push({ k: 'Pod IP', v: d.kpis.pod_ip });
      if (d.metadata?.created_at) metaRows.push({ k: 'Created', v: d.metadata.created_at });
      if (metaRows.length) metaCard.appendChild(metaGrid(metaRows));
      if (d.metadata?.labels && Object.keys(d.metadata.labels).length > 0) {
        metaCard.appendChild(el('div', { class: 'section-title', style: 'margin-top:10px;' }, 'Labels'));
        const labWrap = el('div', { class: 'labels', style: 'margin-top:10px;' });
        for (const k of Object.keys(d.metadata.labels)) labWrap.appendChild(el('span', { class: 'lab' }, k + '=' + d.metadata.labels[k]));
        metaCard.appendChild(labWrap);
      }
      if (d.metadata?.conditions && d.metadata.conditions.length > 0) {
        metaCard.appendChild(el('div', { class: 'section-title', style: 'margin-top:10px;' }, 'Conditions'));
        metaCard.appendChild(metaGrid(d.metadata.conditions.map((c) => ({
          k: c.type,
          v: c.status + (c.reason ? ' · ' + c.reason : ''),
          color: c.severity === 'good' ? 'var(--good)' : c.severity === 'bad' ? 'var(--bad)' : 'var(--fg-dim)',
        }))));
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
      if (r.owner) rows.push(['Owner', r.owner]);
      if (r.workload) rows.push(['Workload', r.workload]);
      for (const cm of (r.config_maps || [])) rows.push(['Mounts ConfigMap', cm]);
      for (const s of (r.secrets || [])) rows.push(['Mounts Secret', s]);
      for (const p of (r.pvcs || [])) rows.push(['Mounts PVC', p]);
      if (!rows.length) return emptyState('No related objects found for this pod.');
      const card = el('div', { class: 'card' }, cardHeader('Relationships', String(rows.length)));
      for (const [label, item] of rows) card.appendChild(relatedRow(label, item));
      return card;
    }

    function buildYaml() {
      // Render the pod's captured object as YAML/JSON via the generic endpoint.
      return rawObjectPanel('/api/v1/namespaces/' + d.namespace + '/pods', d.name);
    }

    const relCount = (d.related?.owner ? 1 : 0) + (d.related?.config_maps || []).length + (d.related?.secrets || []).length + (d.related?.pvcs || []).length;
    const panelDefs = [
      { key: 'summary', label: 'Summary', count: null, build: buildSummary },
      { key: 'containers', label: 'Containers & Logs', count: (d.containers || []).length, build: buildContainers },
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
    const tlBanner = resourceFilterBanner();
    if (tlBanner) root.appendChild(tlBanner);
    root.appendChild(el('div', { class: 'section-title' }, 'All recent transitions'));
    const tlRecent = (data.recent || []).filter((t) => resourceEnabled(t.resource));
    const fb = filterBar({
      placeholder: 'Filter transitions… (kind=, ns=, name=, event=, /regex/)',
      rows: () => tlRecent,
      facets: [
        { key: 'kind', label: 'kind', field: (t) => t.kind },
        { key: 'ns', aliases: ['namespace'], label: 'namespace', field: (t) => t.namespace },
        { key: 'name', label: 'name', field: (t) => t.name },
        { key: 'event', aliases: ['type'], label: 'event type', field: (t) => t.event_type },
      ],
      bareFields: (t) => [t.name, t.kind, t.namespace, t.event_type],
      onChange: () => build(),
    });
    root.appendChild(el('div', { style: 'margin-bottom:12px; max-width:720px;' }, fb.el));
    const recentCard = el('div', { class: 'card' });
    const listWrap = el('div', { class: 'events' });
    recentCard.appendChild(listWrap);
    root.appendChild(recentCard);
    function build() {
      listWrap.innerHTML = '';
      let shown = 0;
      for (const t of tlRecent) { if (!fb.matches(t)) continue; listWrap.appendChild(recentRow(t)); shown++; }
      if (!shown) listWrap.appendChild(el('div', { class: 'state', style: 'padding:18px;' },
        (data.recent && data.recent.length) ? 'No transitions match.' : 'No watch events were captured. Enable `watch: true` on your config’s resource entries to see live transitions.'));
    }
    build();
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
    if (r.name === 'pods') return renderPodsList();
    if (r.name === 'workloads') return renderWorkloadsList();
    if (r.name === 'resources') return renderResourceCatalog();
    if (r.name === 'resource') return renderResourceList();
    if (r.name === 'object') return renderObject();
    if (r.name === 'namespace') return renderNamespace(r.ns);
    if (r.name === 'pod') return renderPod(r.ns, r.pod);
    if (r.name === 'timeline') return renderTimeline();
    if (r.name === 'logs') return renderLogs();
    if (r.name === 'diff') return renderDiff();
    return setContent(errorState('Unknown route'));
  }

  // ── Bootstrap ────────────────────────────────────────────────────────────
  // ── Theme (light/dark) ─────────────────────────────────────────────────────
  const THEME_KEY = 'kshrk.v2.theme';
  function currentTheme() {
    try { return localStorage.getItem(THEME_KEY) === 'light' ? 'light' : 'dark'; } catch (_) { return 'dark'; }
  }
  function applyTheme(t) {
    if (t === 'light') document.documentElement.setAttribute('data-theme', 'light');
    else document.documentElement.removeAttribute('data-theme');
  }
  function setupThemeToggle() {
    const btn = el('button', { class: 'theme-toggle', title: 'Toggle light / dark theme' });
    const paint = () => { btn.textContent = currentTheme() === 'light' ? '☾ Dark' : '☀ Light'; };
    btn.addEventListener('click', () => {
      const next = currentTheme() === 'light' ? 'dark' : 'light';
      try { localStorage.setItem(THEME_KEY, next); } catch (_) {}
      applyTheme(next);
      paint();
    });
    paint();
    // Append last so it sits at the far right of the topbar (after the
    // capture-meta and scrubber).
    const bar = $('topbar');
    if (bar) bar.appendChild(btn);
  }

  async function init() {
    applyTheme(currentTheme());
    setupThemeToggle();
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
