package server

import (
	"encoding/json"
	"sync"
)

// overlay is the in-memory write layer for --writable replay. Client writes
// (create/update/patch/delete) land here and are merged over the replayed
// capture state on read, so a controller can observe its own writes (closed-loop
// dev). It is nil for read-only replay.
//
// Conflict policy is "overlay wins (owned)": on read, an object present in the
// overlay shadows the replayed copy (LIST/GET use the overlay). Suppressing
// replay *watch* events for owned objects lands with watch feedback in a later
// PR. Reset policy is "on loop wrap only" — syncEpoch clears the overlay when the
// replay clock's loop epoch advances; a manual reset is also exposed. The overlay
// persists across seeks.
type overlay struct {
	mu      sync.RWMutex
	items   map[string]*overlayEntry // key: overlayKey(g,v,resource,namespace,name)
	counter int64                    // monotonic RV source (never decreases)
	epoch   int                      // last-seen clock loop epoch (reset-on-loop)

	// subs are active watch subscriptions. A write (store/del/cascade) publishes a
	// typed event to every subscriber whose scope matches, so an active watcher
	// observes overlay writes live (see streamReplayWatch's overlay pump). Guarded
	// by mu; publishLocked runs while a write already holds the lock.
	subs   map[int64]*overlaySub
	nextID int64
}

type overlayEntry struct {
	group, version, resource string
	namespace, name          string
	obj                      json.RawMessage // last-known body (kept even when deleted, for the DELETED event)
	deleted                  bool            // tombstone
	rv                       int64
}

// overlayWatchEvent is a single live overlay change delivered to subscribers.
type overlayWatchEvent struct {
	typ                                       string // ADDED | MODIFIED | DELETED
	rv                                        int64
	obj                                       json.RawMessage
	group, version, resource, namespace, name string
}

// overlaySub is one active watch subscription: a scope filter and a buffered
// delivery channel. namespace == "" matches every namespace (cluster-wide or -A
// watches). overflowCh is closed (once) when the channel fills, so the stream can
// select on it and drop the connection immediately — without waiting to receive a
// further event — mirroring an apiserver watch-cache overflow; the client relists.
type overlaySub struct {
	group, version, resource, namespace string
	ch                                  chan overlayWatchEvent
	overflowCh                          chan struct{}
	overflowed                          bool // guarded by overlay.mu (set in publishLocked)
}

func newOverlay() *overlay {
	return &overlay{items: map[string]*overlayEntry{}, subs: map[int64]*overlaySub{}}
}

// overlaySubBuffer is the per-subscription event backlog. Overlay write volume in
// closed-loop dev is low and the stream pump drains continuously, so this is
// generous headroom rather than a tight bound.
const overlaySubBuffer = 256

// subscribe registers a watch subscription for a list scope and returns its id
// and receive channel. The channel buffers events until the stream's pump drains
// them; unsubscribe removes it.
func (o *overlay) subscribe(group, version, resource, namespace string) (int64, *overlaySub) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.nextID++
	id := o.nextID
	s := &overlaySub{
		group: group, version: version, resource: resource, namespace: namespace,
		ch:         make(chan overlayWatchEvent, overlaySubBuffer),
		overflowCh: make(chan struct{}),
	}
	o.subs[id] = s
	return id, s
}

// unsubscribe removes a subscription. The channel is intentionally not closed:
// the pump exits on the request context instead, so a concurrent publishLocked
// can never send on a closed channel.
func (o *overlay) unsubscribe(id int64) {
	o.mu.Lock()
	delete(o.subs, id)
	o.mu.Unlock()
}

// publishLocked delivers ev to every subscriber whose scope matches. The caller
// holds o.mu. The send is non-blocking: a subscriber whose buffer is full has its
// overflowCh closed (once) so the stream tears down and the client relists, which
// keeps a slow watcher from stalling writes under the lock.
func (o *overlay) publishLocked(ev overlayWatchEvent) {
	for _, s := range o.subs {
		if s.group != ev.group || s.version != ev.version || s.resource != ev.resource {
			continue
		}
		if s.namespace != "" && s.namespace != ev.namespace {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			if !s.overflowed {
				s.overflowed = true
				close(s.overflowCh)
			}
		}
	}
}

// overlayKey identifies an object by GVR + namespace + name.
func overlayKey(group, version, resource, namespace, name string) string {
	return group + "/" + version + "/" + resource + "/" + namespace + "/" + name
}

// nsName is the namespace-qualified name used to match list items ("ns/name",
// or "/name" for cluster-scoped objects).
func nsName(namespace, name string) string { return namespace + "/" + name }

// itemNSName extracts the namespace-qualified name from a raw list item.
func itemNSName(raw json.RawMessage) string {
	var m struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return nsName(m.Metadata.Namespace, m.Metadata.Name)
}

// syncEpoch clears the overlay when the clock's loop epoch has advanced since the
// last call, implementing reset-on-loop. The RV counter stays monotonic so RVs
// are never reused. Safe to call on every read/write.
func (o *overlay) syncEpoch(clock *ReplayClock) {
	if o == nil || clock == nil {
		return
	}
	_, epoch, _ := clock.Sample()
	o.mu.Lock()
	if epoch != o.epoch {
		o.items = map[string]*overlayEntry{}
		o.epoch = epoch
	}
	o.mu.Unlock()
}

// reset clears all overlay entries (manual reset). The RV counter is preserved.
func (o *overlay) reset() {
	o.mu.Lock()
	o.items = map[string]*overlayEntry{}
	o.mu.Unlock()
}

// scopeRV returns the highest overlay RV among entries in a list scope
// (group/version/resource, and namespace — empty matches all namespaces). Replay
// RVs are per watch/list path, so list/watch RV coherence must use the scoped
// overlay RV, not the global counter (which mixes writes across resources).
func (o *overlay) scopeRV(group, version, resource, namespace string) int64 {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var maxRV int64
	for _, e := range o.items {
		if e.group == group && e.version == version && e.resource == resource &&
			(namespace == "" || e.namespace == namespace) {
			if e.rv > maxRV {
				maxRV = e.rv
			}
		}
	}
	return maxRV
}

// bumpRV advances the monotonic counter to at least floorRV+1 and returns it.
// Callers hold o.mu.
func (o *overlay) bumpRVLocked(floorRV int64) int64 {
	rv := o.counter
	if floorRV > rv {
		rv = floorRV
	}
	rv++
	o.counter = rv
	return rv
}

// nextRV advances the monotonic counter above floorRV and returns the new RV,
// without storing anything — callers stamp the RV into the object, then store it.
func (o *overlay) nextRV(floorRV int64) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.bumpRVLocked(floorRV)
}

// store upserts a finalized object under a pre-assigned RV and publishes a watch
// event: ADDED for a new (or previously deleted) identity, MODIFIED for an
// update to a live one.
func (o *overlay) store(group, version, resource, namespace, name string, obj json.RawMessage, rv int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	key := overlayKey(group, version, resource, namespace, name)
	typ := "ADDED"
	if prior, ok := o.items[key]; ok && !prior.deleted {
		typ = "MODIFIED"
	}
	o.items[key] = &overlayEntry{
		group: group, version: version, resource: resource,
		namespace: namespace, name: name, obj: obj, rv: rv,
	}
	o.publishLocked(overlayWatchEvent{
		typ: typ, rv: rv, obj: obj,
		group: group, version: version, resource: resource, namespace: namespace, name: name,
	})
}

// del marks an object deleted (tombstone) and returns its new RV. last is the
// object body to carry on the DELETED event (may be nil).
func (o *overlay) del(group, version, resource, namespace, name string, last json.RawMessage, floorRV int64) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	rv := o.bumpRVLocked(floorRV)
	o.items[overlayKey(group, version, resource, namespace, name)] = &overlayEntry{
		group: group, version: version, resource: resource,
		namespace: namespace, name: name, obj: last, deleted: true, rv: rv,
	}
	o.publishLocked(overlayWatchEvent{
		typ: "DELETED", rv: rv, obj: last,
		group: group, version: version, resource: resource, namespace: namespace, name: name,
	})
	return rv
}

// isCoreNamespace matches the core cluster-scoped "namespaces" resource.
func isCoreNamespace(group, version, resource string) bool {
	return group == "" && version == "v1" && resource == "namespaces"
}

// cascadeDeleteNamespace tombstones every live overlay object in a namespace,
// mimicking the namespace controller's cascade so a deleted namespace's
// overlay-created contents don't linger. Captured objects in the namespace are
// handled lazily on read (see isNamespaceDeleted / read filtering).
func (o *overlay) cascadeDeleteNamespace(namespace string) {
	if namespace == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for key, e := range o.items {
		if e.namespace == namespace && !e.deleted {
			rv := o.bumpRVLocked(0)
			o.items[key] = &overlayEntry{
				group: e.group, version: e.version, resource: e.resource,
				namespace: e.namespace, name: e.name, obj: e.obj, deleted: true, rv: rv,
			}
			o.publishLocked(overlayWatchEvent{
				typ: "DELETED", rv: rv, obj: e.obj,
				group: e.group, version: e.version, resource: e.resource,
				namespace: e.namespace, name: e.name,
			})
		}
	}
}

// isNamespaceDeleted reports whether the given namespace has been deleted in the
// overlay (a tombstoned core namespaces object).
func (o *overlay) isNamespaceDeleted(namespace string) bool {
	if namespace == "" {
		return false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	e, ok := o.items[overlayKey("", "v1", "namespaces", "", namespace)]
	return ok && e.deleted
}

// deletedNamespaces returns the set of namespaces deleted in the overlay, used to
// filter their (possibly captured) contents out of read responses.
func (o *overlay) deletedNamespaces() map[string]struct{} {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var out map[string]struct{}
	for _, e := range o.items {
		if e.deleted && isCoreNamespace(e.group, e.version, e.resource) {
			if out == nil {
				out = map[string]struct{}{}
			}
			out[e.name] = struct{}{}
		}
	}
	return out
}

// ownsObject reports whether the overlay holds an entry for the identity of a
// raw object (by GVR + its metadata name/namespace), live or tombstoned. Replay
// watch events for an owned identity are suppressed so the overlay copy wins and
// the client never sees a stale captured event after taking ownership.
func (o *overlay) ownsObject(group, version, resource string, raw json.RawMessage) bool {
	var m struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Metadata.Name == "" {
		return false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	_, ok := o.items[overlayKey(group, version, resource, m.Metadata.Namespace, m.Metadata.Name)]
	return ok
}

// get returns the overlay entry for an object identity, if present.
func (o *overlay) get(group, version, resource, namespace, name string) (*overlayEntry, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	e, ok := o.items[overlayKey(group, version, resource, namespace, name)]
	return e, ok
}

// applyToList merges the overlay over a base list's items for a given list scope
// (group/version/resource, and namespace — empty for a cluster-wide/-A list).
// Overlay entries replace matching base items (overlay wins), tombstones remove
// them, and overlay-created items not in the base are appended. It also returns
// the highest overlay RV merged into this scope, captured under the same lock as
// the snapshot: a watch uses it as its overlay-event skip floor so events already
// reflected in the burst are suppressed while any write after the snapshot (which
// the monotonic counter guarantees has a higher RV) is still delivered — no
// duplicate and no gap.
func (o *overlay) applyToList(group, version, resource, namespace string, base []json.RawMessage) ([]json.RawMessage, int64) {
	// Snapshot the relevant entries under the lock, then release it before walking
	// (and JSON-unmarshalling) the base list, so a large LIST doesn't block writes.
	// Stored entries are immutable (store/del replace the pointer), so the
	// snapshot is safe to read after unlock.
	o.mu.RLock()
	matched := map[string]*overlayEntry{}
	var maxRV int64
	for _, e := range o.items {
		if e.group == group && e.version == version && e.resource == resource &&
			(namespace == "" || e.namespace == namespace) {
			matched[nsName(e.namespace, e.name)] = e
			if e.rv > maxRV {
				maxRV = e.rv
			}
		}
	}
	o.mu.RUnlock()

	if len(matched) == 0 {
		return base, maxRV
	}

	// Pre-size to the base length; append grows for any overlay-created items.
	// (Avoids len(base)+len(matched), which CodeQL flags as a possible overflow.)
	out := make([]json.RawMessage, 0, len(base))
	seen := map[string]bool{}
	for _, item := range base {
		k := itemNSName(item)
		if e, ok := matched[k]; ok {
			seen[k] = true
			if e.deleted {
				continue // removed by overlay
			}
			out = append(out, e.obj) // overlay wins
		} else {
			out = append(out, item)
		}
	}
	for k, e := range matched {
		if seen[k] || e.deleted {
			continue
		}
		out = append(out, e.obj)
	}
	return out, maxRV
}
