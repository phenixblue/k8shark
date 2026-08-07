package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	kstore "github.com/phenixblue/k8shark/internal/store"
)

// handler is the http.Handler for the mock Kubernetes API server.
type handler struct {
	store   *kstore.CaptureStore
	at      time.Time
	verbose bool
	// clock, when non-nil, puts the handler in replay mode: LIST/GET reconstruct
	// state as-of the clock's advancing position and watches stream captured
	// events over time (see streamReplayWatch). nil for plain `open`/`ui`.
	clock *ReplayClock

	// overlay, when non-nil, enables writable replay: client writes land in an
	// in-memory layer merged over the replayed state (see overlay.go, writes.go).
	// nil for read-only replay (writes return 405).
	overlay *overlay

	// schedulePods, when true (writable replay), makes the overlay bind an
	// unscheduled Pod to a node on create — the scheduler replay lacks — so KWOK's
	// "Pod bound to a node → Running" stage can fire. See writes.go and #160.
	schedulePods bool

	// timelineCache memoizes the (immutable) per-path replay event timeline so
	// LIST and WATCH don't rebuild it per request — important for poll-only
	// captures, whose timeline requires diffing every snapshot.
	timelineMu    sync.Mutex
	timelineCache map[string][]replayEvent

	// crdColsCache memoizes CRD-derived Table columns per "group/version/resource".
	// A CRD's additionalPrinterColumns are effectively static, so resolving them
	// once avoids reconstructing+parsing the whole CRD list on every Table render
	// (ReconstructAt caches by exact clock time, so an advancing replay clock would
	// otherwise defeat that cache). Negative results are cached too. Trade-off: a
	// CRD created/changed mid-replay isn't reflected — acceptable for a mock.
	crdColsCache sync.Map // map["group/version/resource"] -> []tableCol (may be nil)

	// wg tracks in-flight ServeHTTP calls. Server.teardown waits on it after
	// httpServer.Shutdown returns, as a hard guarantee that no handler —
	// including a long-held watch stream — is still reading from the store
	// when the archive is closed (see #230).
	wg sync.WaitGroup
}

// waitForRequests blocks until every in-flight ServeHTTP call has returned.
func (h *handler) waitForRequests() {
	h.wg.Wait()
}

func newHandler(store *kstore.CaptureStore, at time.Time, verbose bool) *handler {
	return &handler{store: store, at: at, verbose: verbose}
}

// timelineFor returns the memoized replay timeline for a watch path. The key is
// normalized (trailing "/" trimmed) so a LIST on ".../pods/" and a WATCH on
// ".../pods" share one timeline — keeping their RVs coherent and the cache warm.
//
// The (potentially expensive, for poll-only) build runs without the lock held so
// it doesn't block concurrent requests for other paths. On a cold cache, several
// concurrent callers for the same path may each build it; the first stored result
// wins and the rest are discarded. (A singleflight-style guard could dedupe the
// redundant builds if that ever matters — in practice the cold window is brief.)
func (h *handler) timelineFor(watchPath string) []replayEvent {
	watchPath = strings.TrimSuffix(watchPath, "/")

	h.timelineMu.Lock()
	if h.timelineCache == nil {
		h.timelineCache = map[string][]replayEvent{}
	}
	if tl, ok := h.timelineCache[watchPath]; ok {
		h.timelineMu.Unlock()
		return tl
	}
	h.timelineMu.Unlock()

	tl := buildReplayTimeline(h.store, watchPath)

	h.timelineMu.Lock()
	defer h.timelineMu.Unlock()
	if existing, ok := h.timelineCache[watchPath]; ok {
		return existing // another goroutine won the race; use its result
	}
	h.timelineCache[watchPath] = tl
	return tl
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.wg.Add(1)
	defer h.wg.Done()

	// In replay mode the effective time is the clock's current position;
	// otherwise it's the server's fixed --at.
	replayAt := h.at
	if h.clock != nil {
		replayAt = h.clock.Now()
	}
	// Per-request timestamp override via header (UI time travel).
	if v := r.Header.Get("X-K8shark-At"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			replayAt = t
		}
	}

	path := r.URL.Path
	if h.verbose {
		fmt.Printf("  --> %s %s\n", r.Method, path)
	}

	// Response content negotiation: when the client prefers protobuf, buffer the
	// (non-watch) response and re-encode JSON bodies of built-in types as
	// protobuf on the way out. Installed before the early POST/compat shims so
	// their Status/object responses are negotiated too. Skipped for watch streams
	// and for endpoints that never return a protobuf object and may be large or
	// streamed (OpenAPI docs, pod logs), to avoid buffering them pointlessly.
	watchParam := r.URL.Query().Get("watch")
	isWatch := watchParam == "1" || watchParam == "true"
	if kstore.WantsProtobuf(r) && !isWatch && !kstore.IsNonProtobufPath(path) {
		pw := kstore.NewProtobufResponseWriter(w)
		defer pw.Flush()
		w = pw
	}

	// Metadata-only projection (as=PartialObjectMetadata), used by
	// kube-controller-manager's garbagecollector to walk ownerReferences (#329).
	// Installed *after* the protobuf wrapper so its deferred Flush runs first
	// (defers are LIFO): the body is projected to metadata, and protobuf
	// encoding then sees the projected object rather than the full one.
	if mv, ok := kstore.WantsPartialObjectMetadata(r); ok && !isWatch && !kstore.IsNonProtobufPath(path) {
		mw := kstore.NewPartialMetadataResponseWriter(w, mv)
		defer mw.Flush()
		w = mw
	}

	// Replay transport controls live under a reserved prefix that can't collide
	// with the Kubernetes API (which is served under /api, /apis, …). They accept
	// POST, so intercept before the read-only method check below. Match the exact
	// prefix or a subpath boundary so paths like "/_k8shark/replayfoo" don't route
	// here.
	if h.clock != nil && (path == replayControlPrefix || strings.HasPrefix(path, replayControlPrefix+"/")) {
		h.handleReplayControl(w, r, path)
		return
	}

	// Interactive sub-resources (exec/portforward/attach/proxy) that can never
	// work against a capture replay, and client-tooling compatibility shims
	// (e.g. k9s's authorization review POSTs) — see compat.go. Both checked
	// before the method check below so they get a specific, actionable
	// response instead of the generic "write operations are not supported"
	// one, and so exec/portforward don't hang waiting for a protocol upgrade.
	if h.tryRejectInteractiveSubresource(w, path) {
		return
	}
	if h.tryClientCompatShim(w, r, path) {
		return
	}

	// Writes: accepted into the overlay in writable replay; otherwise 405.
	// RFC 7231 §6.5.5 requires an Allow header with a 405 response.
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// allowed
	default:
		if h.overlay != nil {
			h.handleWrite(w, r, path)
			return
		}
		w.Header().Set("Allow", "GET, HEAD")
		h.writeStatus(w, http.StatusMethodNotAllowed,
			"k8shark replay server is read-only; write operations are not supported")
		return
	}

	// Watch requests get a synthetic event stream. In replay mode the stream is
	// paced by the clock; otherwise it's the snapshot-burst-then-idle behavior.
	if r.URL.Query().Get("watch") == "1" || r.URL.Query().Get("watch") == "true" {
		if h.clock != nil {
			h.streamReplayWatch(w, r, path)
		} else {
			h.handleWatch(w, r, path, replayAt)
		}
		return
	}

	// Route discovery and resource requests.
	switch {
	case path == "/version":
		h.serveVersion(w)
	case path == "/healthz", path == "/readyz", path == "/livez":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	case path == "/openapi/v2":
		if !h.tryServeFromStore(w, path, replayAt) {
			// Minimal stub so kubectl tolerates missing spec gracefully.
			writeJSON(w, http.StatusOK, map[string]any{"swagger": "2.0", "info": map[string]any{"title": "k8shark", "version": "0.0.0"}, "paths": map[string]any{}})
		}
	case path == "/openapi/v3", strings.HasPrefix(path, "/openapi/v3/"):
		if !h.tryServeFromStore(w, path, replayAt) {
			h.writeStatus(w, http.StatusNotFound, path+" not in capture")
		}
	case path == "/api":
		if !h.tryServeFromStore(w, path, replayAt) {
			h.serveAPIVersions(w)
		}
	case path == "/apis":
		// Discovery documents are the one read path a capture always records
		// unconditionally (see the capture engine's fetchDiscovery), so
		// tryServeFromStore almost always wins here — plain tryServeFromStore
		// would then serve the frozen captured document forever, hiding any
		// group a CRD created via the writable overlay defines after capture
		// (see registerCRDResourceInfo in writes.go). tryServeAPIGroupListFromStore
		// merges those in while still serving the captured bytes verbatim
		// whenever there's nothing new to add.
		if !h.tryServeAPIGroupListFromStore(w, path, replayAt) {
			h.serveAPIGroupList(w)
		}
	case path == "/api/v1":
		if !h.tryServeFromStore(w, path, replayAt) {
			h.serveAPIResourceList(w, "", "v1")
		}
	case strings.HasPrefix(path, "/apis/") && isGroupVersionPath(path):
		// Same captured-document-shadows-runtime-additions problem as /apis
		// above, for a group/version that already existed at capture time —
		// merge in any resource a CRD registered under it afterward.
		if !h.tryServeAPIResourceListFromStore(w, path, replayAt) {
			h.serveGroupResourceList(w, path)
		}
	case strings.HasPrefix(path, "/apis/") && isBareGroupPath(path):
		// The bare, version-less APIGroup discovery document (distinct from
		// /apis/<group>/<version>'s resource list) — client-go's discovery
		// client fetches this to enumerate a group's available versions
		// before making a versioned request. Not synthesized before, so a
		// group whose only captured resource is a version nested deeper (or
		// simply never walked at this exact literal path) 404'd here even
		// though /apis/<group>/<version> worked fine — breaking any client
		// that discovers a group this way first (found via the upstream
		// conformance suite's Ingress/IngressClass API specs).
		group := strings.TrimPrefix(path, "/apis/")
		if !h.tryServeAPIGroupFromStore(w, path, group, replayAt) {
			h.serveAPIGroup(w, group)
		}
	case strings.HasSuffix(path, "/log"):
		// Pod log sub-resource: serve captured content or a helpful stub.
		h.serveLog(w, r, path, replayAt)
	default:
		h.serveResource(w, r, path, replayAt)
	}
}

func (h *handler) serveResource(w http.ResponseWriter, r *http.Request, path string, at time.Time) {
	g, v, res, ns, name, sub := parseWritePath(strings.TrimSuffix(path, "/"))
	// GET .../scale works in both writable and read-only replay (unlike scale
	// writes, which need the overlay) — it's just a read, synthesized from
	// whatever the underlying object's current spec/status are.
	if name != "" && sub == "scale" {
		h.serveScale(w, g, v, res, ns, name, at)
		return
	}

	// Writable replay: a single-object GET is served from the overlay when the
	// overlay owns it (created/updated → the overlay copy; deleted → 404).
	if h.overlay != nil {
		h.syncEpoch()
		// A single-object GET in a namespace deleted in the overlay is gone
		// (cascade), even for captured objects.
		if name != "" && h.overlay.isNamespaceDeleted(ns) {
			writeJSON(w, http.StatusNotFound, notFoundStatus("", "namespaces", ns))
			return
		}
		// Serve overlay-owned objects for a single-object GET (and GET .../status,
		// so clients can observe their status updates).
		if name != "" && (sub == "" || sub == "status") {
			if e, ok := h.overlay.get(g, v, res, ns, name); ok {
				if e.deleted {
					writeJSON(w, http.StatusNotFound, notFoundStatus(g, res, name))
					return
				}
				// Honor Table format (kubectl get <name>) for overlay objects.
				if strings.Contains(r.Header.Get("Accept"), "as=Table") {
					if tb, rok := h.renderResourceTable(path, e.obj, at); rok {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write(tb)
						return
					}
				}
				writeJSON(w, http.StatusOK, json.RawMessage(e.obj))
				return
			}
		}
	}

	body, code, err := h.store.ReconstructAt(path, at)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, statusObj(500, err.Error()))
		return
	}

	if code == 404 {
		// Try single-item GET by looking up the parent list and filtering by name.
		body, code = h.trySingleItemGet(path, at)
	}

	if code == 404 {
		// Try all-namespaces aggregation: kubectl -A issues cluster-wide paths
		// like /api/v1/pods or /apis/apps/v1/deployments. Only fire for paths
		// with no namespace segment; namespace-scoped 404s fall through to the
		// cluster-scoped fallback below, which correctly filters by namespace.
		if _, _, _, reqNS := kstore.ParseAPIPath(path); reqNS == "" {
			body, code, err = h.store.AggregateAcrossNamespaces(path, at)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, statusObj(500, err.Error()))
				return
			}
		}
	}

	if code == 404 {
		// Cluster-scoped fallback: if the resource was captured at the cluster
		// path (e.g. pods with no namespaces: in config, or allNotFound fallback)
		// but the request targets a specific namespace, try the cluster path and
		// filter items by metadata.namespace. This makes kubectl get pods -n <ns>
		// work even when only /api/v1/pods (not per-namespace paths) was captured.
		g, v, resource, ns := kstore.ParseAPIPath(path)
		if ns != "" && resource != "" {
			var clusterPath string
			if g == "" {
				clusterPath = "/api/" + v + "/" + resource
			} else {
				clusterPath = "/apis/" + g + "/" + v + "/" + resource
			}
			clusterBody, clusterCode, cerr := h.store.ReconstructAt(clusterPath, at)
			if cerr != nil {
				writeJSON(w, http.StatusInternalServerError, statusObj(500, cerr.Error()))
				return
			}
			if clusterCode == 200 {
				filtered, ferr := kstore.ApplySelectors(clusterBody, "", kstore.NamespaceScopeSelector(ns))
				if ferr == nil {
					body, code = filtered, 200
				}
			}
		}
	}

	if code == 404 {
		// If the path parses as a list-level resource (not an item GET), return
		// an empty list — a known kind (in a captured discovery document or the
		// index, even with zero captured objects) behaves like a real cluster's
		// empty live collection: no warning, since a client may well create
		// objects of it next (especially in writable mode). Reserve the warning
		// for a genuinely unknown/misconfigured kind (#177).
		g, v, resource, _ := kstore.ParseAPIPath(path)
		if resource != "" {
			av := v
			if g != "" {
				av = g + "/" + v
			}
			// Prefer the authoritative Kind from discovery/index metadata over the
			// kstore.ResourceToKind heuristic, which guesses wrong for resources whose
			// Kind doesn't follow simple depluralization (e.g. endpointslices)
			// and for most CRDs — a client deserializing by GVK would break.
			kind := h.store.ResourceKind(g, v, resource)
			if kind == "" {
				kind = kstore.ResourceToKind(resource)
			}
			emptyList, _ := json.Marshal(map[string]any{
				"apiVersion": av,
				"kind":       kind + "List",
				"metadata":   map[string]string{"resourceVersion": "0"},
				"items":      []any{},
			})
			if !h.store.IsKnownResource(g, v, resource) {
				w.Header().Set("Warning", fmt.Sprintf(`299 k8shark %q`,
					resource+" not found in capture; was it included in the capture config?"))
			}
			body, code = emptyList, 200
		} else {
			// Item-level GET (path has more segments than kstore.ParseAPIPath handles):
			// for a known resource, a standard NotFound Status matching a live
			// cluster, so apierrors.IsNotFound() recognizes it (#177). An unknown
			// resource keeps the k8shark-specific message instead — it signals a
			// capture-config problem (wrong group/resource entirely), which
			// "<resource> \"<name>\" not found" would misleadingly present as "the
			// object is just missing".
			ig, iv, iresource, _, iname, _ := parseWritePath(strings.TrimSuffix(path, "/"))
			if iresource != "" && iname != "" && h.store.IsKnownResource(ig, iv, iresource) {
				writeJSON(w, http.StatusNotFound, notFoundStatus(ig, iresource, iname))
				return
			}
			h.writeStatus(w, http.StatusNotFound, fmt.Sprintf("%q not found in capture", path))
			return
		}
	}

	// Apply label/field selectors if present. The field selector is validated
	// against the kind's field-label contract before anything is filtered: an
	// unsupported label is a 400 on a read exactly as on a deletecollection,
	// which is what a real apiserver does (the conversion runs in the generic
	// handler, shared by list, watch and delete). Silently ignoring it would
	// serve every item as if the selector had matched everything.
	//
	// Skipped for an item-level GET, where ParseAPIPath yields no resource: a
	// single-object GET takes GetOptions, which has no field selector, so
	// upstream ignores the parameter rather than rejecting it.
	labelSel := r.URL.Query().Get("labelSelector")
	selGroup, _, selResource, _ := kstore.ParseAPIPath(strings.TrimSuffix(path, "/"))
	var fieldSel *kstore.FieldSelector
	if selResource != "" {
		var fsErr error
		fieldSel, fsErr = kstore.ParseFieldSelector(selGroup, selResource, r.URL.Query().Get("fieldSelector"))
		if fsErr != nil {
			h.writeStatus(w, http.StatusBadRequest, fsErr.Error())
			return
		}
	}
	body, err = kstore.ApplySelectors(body, labelSel, fieldSel)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, statusObj(500, err.Error()))
		return
	}

	// Writable replay: merge overlay objects into the list (overlay wins) before
	// Table negotiation, then re-apply selectors so overlay items are filtered
	// consistently with replayed items.
	if h.overlay != nil {
		body, _ = h.mergeOverlayList(path, body)
		if filtered, ferr := kstore.ApplySelectors(body, labelSel, fieldSel); ferr == nil {
			body = filtered
		}
	}

	// tableFiltered applies label/field selectors to a stored Table-format body,
	// removing rows whose embedded object does not match. Returns tb unchanged
	// when selectors are empty or if filtering fails (best-effort).
	//
	// ok is false when a stored Table cannot be filtered faithfully, and the
	// caller must fall back to a computed Table rather than serve rows the
	// selector was never really applied to — either because the JSON list did
	// not reconstruct, or because an item's identity would not decode, leaving
	// no way to correlate it with a row. That happens when the selector reads
	// spec or status: a stored Table's rows embed PartialObjectMetadata, so those
	// fields simply are not there, and evaluating against the row would match
	// nothing regardless of the value. A real apiserver filters full objects and
	// then projects to Table, so we intersect with the identities that survived
	// on the JSON list — which was filtered with the full objects above (#339).
	tableFiltered := func(tb []byte) ([]byte, bool) {
		if labelSel == "" && fieldSel == nil {
			return tb, true
		}
		if fieldSel.NeedsFullObject() {
			allow, ok := kstore.ListIdentities(body)
			if !ok {
				return nil, false
			}
			out, ferr := kstore.FilterTableRowsToIdentities(tb, allow)
			if ferr != nil {
				return nil, false
			}
			return out, true
		}
		if out, ferr := kstore.FilterTableRows(tb, labelSel, fieldSel); ferr == nil {
			return out, true
		}
		return tb, true
	}

	// If kubectl requests Table format, try the captured Table response first
	// (real column defs + pre-computed cell values from the actual cluster).
	// Otherwise fall back to a computed Table (see renderResourceTable).
	if strings.Contains(r.Header.Get("Accept"), "as=Table") {
		// Exact-path stored Table (namespace-scoped list). Bypassed in writable
		// mode so the Table reflects overlay writes (built from the merged body
		// below) rather than the captured-only stored Table.
		if tb, tbCode, _ := h.store.Latest(path+"?as=Table", at); tbCode == 200 && h.overlay == nil {
			if out, ok := tableFiltered(tb); ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = w.Write(out)
				return
			}
			// Fall through to the computed Table, built from the filtered list.
		}
		// Single-item GET: extract the matching row from the parent list's Table.
		// Selectors on single-item GETs are resolved by name, not labels — no
		// row filtering needed here.
		if i := strings.LastIndex(path, "/"); i > 0 {
			parentTable, ptCode, _ := h.store.Latest(path[:i]+"?as=Table", at)
			if ptCode == 200 {
				if tb, err2 := extractTableRow(parentTable, path[i+1:]); err2 == nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					_, _ = w.Write(tb)
					return
				}
			}
		}
		// Aggregated Table across namespaces (for -A / cluster-scoped paths only).
		// Also bypassed in writable mode (see above).
		if _, _, _, reqNS := kstore.ParseAPIPath(path); reqNS == "" && h.overlay == nil {
			if tb, tbCode, _ := h.store.AggregateTableAcrossNamespaces(path, at); tbCode == 200 {
				if out, ok := tableFiltered(tb); ok {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					_, _ = w.Write(out)
					return
				}
				// Fall through to the computed Table, built from the filtered list.
			}
		}
		// Compute a Table for objects not covered by a captured Table (writable
		// overlay, or kinds/captures without a stored Table): built-in per-kind
		// printers, else CRD additionalPrinterColumns, else captured
		// columnDefinitions, else a computed generic NAME/(NAMESPACE)/AGE table.
		// body is the merged, selector-filtered list (or a single object).
		if tb, ok := h.renderResourceTable(path, body, at); ok {
			body = tb
		}
	}

	// In replay mode, override a list's resourceVersion with the coherent RV
	// as-of the clock (and, in writable mode, at least the overlay's RV), so a
	// following WATCH(resourceVersion=RV) aligns with the event stream's RVs.
	if h.clock != nil {
		body = rewriteListResourceVersion(body, func() int64 {
			rv := rvAsOf(h.timelineFor(path), at)
			if h.overlay != nil {
				g, v, resource, namespace := kstore.ParseAPIPath(strings.TrimSuffix(path, "/"))
				if orv := h.overlay.scopeRV(g, v, resource, namespace); orv > rv {
					rv = orv
				}
			}
			return rv
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// mergeOverlayList merges overlay objects into a list body for the list's scope.
// Non-list bodies are returned unchanged.
// mergeOverlayList merges overlay objects into a list body for the list's scope
// and returns the highest overlay RV merged in (see applyToList) — the watch
// burst uses it as its overlay-event skip floor. LIST callers ignore the RV.
func (h *handler) mergeOverlayList(path string, body []byte) ([]byte, int64) {
	h.syncEpoch() // reset-on-loop before merging into a LIST
	group, version, resource, namespace := kstore.ParseAPIPath(strings.TrimSuffix(path, "/"))
	if resource == "" {
		return body, 0
	}
	var list struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Metadata   json.RawMessage   `json:"metadata"`
		Items      []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil || list.Items == nil {
		return body, 0 // not a list
	}
	var skipRV int64
	list.Items, skipRV = h.overlay.applyToList(group, version, resource, namespace, list.Items)
	// Cascade: drop items whose namespace was deleted in the overlay (covers both
	// overlay-created and captured items, in namespaced and cluster-wide/-A lists).
	list.Items = dropDeletedNamespaceItems(list.Items, h.overlay.deletedNamespaces())
	out, err := json.Marshal(list)
	if err != nil {
		return body, skipRV
	}
	return out, skipRV
}

// trySingleItemGet handles GET .../resource/{name} by finding the parent list
// and scanning its items for a matching metadata.name.
func (h *handler) trySingleItemGet(path string, at time.Time) ([]byte, int) {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return nil, 404
	}
	name := path[i+1:]
	parentPath := path[:i]

	body, code, err := h.store.ReconstructAt(parentPath, at)
	if err != nil || code != 200 {
		// Namespace-scoped single-item GET whose per-namespace list was not
		// captured: fall back to the cluster-scoped list and filter by namespace.
		g, v, resource, ns := kstore.ParseAPIPath(parentPath)
		if ns != "" && resource != "" {
			var clusterParent string
			if g == "" {
				clusterParent = "/api/" + v + "/" + resource
			} else {
				clusterParent = "/apis/" + g + "/" + v + "/" + resource
			}
			clusterBody, clusterCode, cerr := h.store.ReconstructAt(clusterParent, at)
			if cerr == nil && clusterCode == 200 {
				// Filter to the requested namespace before doing the name lookup.
				filtered, ferr := kstore.ApplySelectors(clusterBody, "", kstore.NamespaceScopeSelector(ns))
				if ferr == nil {
					body, code = filtered, 200
				}
			}
		}
		if code != 200 {
			return nil, 404
		}
	}

	var list struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Items      []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, 404
	}

	// Derive item kind from list kind (e.g. "PodList" → "Pod").
	itemKind := strings.TrimSuffix(list.Kind, "List")

	for _, item := range list.Items {
		var obj struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		if obj.Metadata.Name == name {
			// Inject apiVersion + kind so kubectl can decode the single object.
			var m map[string]json.RawMessage
			if err := json.Unmarshal(item, &m); err != nil {
				return item, 200
			}
			av, _ := json.Marshal(list.APIVersion)
			kd, _ := json.Marshal(itemKind)
			m["apiVersion"] = av
			m["kind"] = kd
			out, err := json.Marshal(m)
			if err != nil {
				return item, 200
			}
			return out, 200
		}
	}
	return nil, 404
}

func (h *handler) handleWatch(w http.ResponseWriter, r *http.Request, path string, at time.Time) {
	watchPath := strings.TrimSuffix(path, "/")
	// Same per-kind field-label validation as the list path — a watch and a list
	// share the conversion upstream, so an unsupported label is a 400 here too
	// rather than a stream of everything (#339).
	selGroup, _, selResource, _ := kstore.ParseAPIPath(watchPath)
	fieldSel, fsErr := kstore.ParseFieldSelector(selGroup, selResource, r.URL.Query().Get("fieldSelector"))
	if fsErr != nil {
		h.writeStatus(w, http.StatusBadRequest, fsErr.Error())
		return
	}
	list, ok, err := h.resolveWatchList(watchPath, at, r.URL.Query().Get("labelSelector"), fieldSel)
	if err != nil {
		h.writeStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		h.writeStatus(w, http.StatusNotFound, fmt.Sprintf("%q not found in capture", path))
		return
	}

	// Honor ?timeoutSeconds: nil channel blocks forever (no timeout).
	timer, stopTimer := watchTimeout(r.URL.Query().Get("timeoutSeconds"))
	defer stopTimer()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)

	for _, item := range list.Items {
		event := map[string]any{"type": "ADDED", "object": json.RawMessage(item)}
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "%s\n", data)
		if canFlush {
			flusher.Flush()
		}
	}

	// Use resourceVersion from the list metadata; fall back to a capture time.
	// Treat "0" as unspecified too — aggregated/synthesized empty lists carry
	// RV "0", but watch clients expect a non-zero BOOKMARK resourceVersion.
	rv := list.ResourceVersion
	if rv == "" || rv == "0" {
		// Lead with the list-as-of time so the BOOKMARK RV aligns with --at / UI
		// time travel; fall back to the capture bounds when `at` is unset.
		rv = bookmarkResourceVersion(at, h.store.Metadata.CapturedAt, h.store.Metadata.CapturedUntil)
	}

	// BOOKMARK signals end of initial list; kubectl -w then waits for new events.
	// The BOOKMARK object must have the same kind as the watched resource
	// (not "Status"), otherwise client-go reflectors log unexpected type errors.
	bookmarkKind := strings.TrimSuffix(list.Kind, "List")
	bookmarkAPIVersion := list.APIVersion
	if bookmarkKind == "" {
		bookmarkKind = "Status"
	}
	if bookmarkAPIVersion == "" {
		bookmarkAPIVersion = "v1"
	}
	// WatchList (client-go 1.32+): terminate the initial-list burst with a
	// BOOKMARK carrying k8s.io/initial-events-end so informers complete sync.
	meta := map[string]any{"resourceVersion": rv}
	if r.URL.Query().Get("sendInitialEvents") == "true" {
		meta["annotations"] = map[string]string{"k8s.io/initial-events-end": "true"}
	}
	bookmark := map[string]any{
		"type": "BOOKMARK",
		"object": map[string]any{
			"apiVersion": bookmarkAPIVersion,
			"kind":       bookmarkKind,
			"metadata":   meta,
		},
	}
	data, _ := json.Marshal(bookmark)
	_, _ = fmt.Fprintf(w, "%s\n", data)
	if canFlush {
		flusher.Flush()
	}

	// Hold until the client disconnects or timeoutSeconds elapses.
	select {
	case <-r.Context().Done():
	case <-timer:
	}
}

// serveLog serves a pod log sub-resource (e.g. /api/v1/namespaces/<ns>/pods/<name>/log).
// Recognized query parameters:
//   - container=<c>  — request a specific container's log
//   - previous=true  — request the previous-container log (kubectl logs --previous)
//
// Lookup order:
//  1. If both container and previous are set, look up the record under
//     path + "?container=<c>&previous=true".
//  2. If only container is set, look up path + "?container=<c>".
//  3. Legacy archives stored a single record at the bare path — try that.
//  4. If no container was specified, fall back to the first per-container
//     record we have for this pod (covers single-container pods, where
//     kubectl sends no ?container= param).
//  5. Return a readable stub explaining how to enable log capture.
func (h *handler) serveLog(w http.ResponseWriter, r *http.Request, path string, at time.Time) {
	q := r.URL.Query()
	container := q.Get("container")
	previous := q.Get("previous") == "true"

	if container != "" {
		key := path + "?container=" + container
		if previous {
			key += "&previous=true"
		}
		if h.tryServeLogRecord(w, key, at) {
			return
		}
	}
	if h.tryServeLogRecord(w, path, at) {
		return
	}
	if container == "" {
		prefix := path + "?container="
		suffix := ""
		if previous {
			suffix = "&previous=true"
		}
		for indexKey := range h.store.Index {
			if !strings.HasPrefix(indexKey, prefix) {
				continue
			}
			if suffix != "" && !strings.HasSuffix(indexKey, suffix) {
				continue
			}
			if suffix == "" && strings.Contains(indexKey, "&previous=true") {
				// When the client didn't ask for previous logs, don't accidentally
				// serve a previous-log record as the default.
				continue
			}
			if h.tryServeLogRecord(w, indexKey, at) {
				return
			}
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w,
		"# k8shark capture replay: logs were not captured for this pod.\n"+
			"# To capture logs, add 'logs: 200' (or another line count) to the\n"+
			"# pods entry in your k8shark capture config and re-run the capture.\n")
}

// tryServeLogRecord writes the captured log for indexKey if one exists.
// Returns true on success so the caller stops trying further fallbacks.
func (h *handler) tryServeLogRecord(w http.ResponseWriter, indexKey string, at time.Time) bool {
	body, code, err := h.store.Latest(indexKey, at)
	if err != nil || code != 200 {
		return false
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// Logs are stored as JSON strings; decode to recover the original plain text.
	var text string
	if json.Unmarshal(body, &text) == nil {
		_, _ = fmt.Fprint(w, text)
		return true
	}
	// Fallback: body is already plain text (should not normally happen).
	_, _ = w.Write(body)
	return true
}
