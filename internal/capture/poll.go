package capture

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/phenixblue/k8shark/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// pollResource polls a single resource kind at its configured interval until
// ctx is done, unless e.pollPasses overrides this with a fixed fetch count
// (see its doc comment on Engine).
func (e *Engine) pollResource(ctx context.Context, res config.Resource) {
	if e.pollPasses > 0 {
		for i := 0; i < e.pollPasses && ctx.Err() == nil; i++ {
			e.fetchResource(ctx, res)
		}
		return
	}

	ticker := time.NewTicker(res.Interval)
	defer ticker.Stop()

	// Poll immediately, then on each tick.
	e.fetchResource(ctx, res)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.fetchResource(ctx, res)
		}
	}
}

// watchResource starts watch loops for the given resource. By default one
// watch loop runs per configured namespace, but when the resource was
// originally configured with a wildcard namespace ("*") a single cluster-wide
// watch stream is used instead. This matches how in-cluster controllers
// watch resources and avoids opening N streams against the API server on
// clusters with many namespaces. For cluster-scoped resources a single watch
// loop is used.
func (e *Engine) watchResource(ctx context.Context, res config.Resource) {
	if res.WildcardNamespaces {
		// One cluster-wide stream; streamWatch demuxes events back to per-
		// namespace API paths via metadata.namespace on each event object.
		e.watchResourcePath(ctx, res, "")
		return
	}

	namespaces := res.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}

	var wg sync.WaitGroup
	for _, ns := range namespaces {
		wg.Add(1)
		go func(namespace string) {
			defer wg.Done()
			e.watchResourcePath(ctx, res, namespace)
		}(ns)
	}
	wg.Wait()
}

func (e *Engine) watchResourcePath(ctx context.Context, res config.Resource, namespace string) {
	apiPath := buildAPIPath(res.Group, res.Version, res.Resource, namespace)

	for {
		if ctx.Err() != nil {
			return
		}

		if err := e.streamWatch(ctx, res, apiPath, namespace); err != nil && ctx.Err() == nil && e.verbose {
			fmt.Fprintf(os.Stderr, "  [watch] %s: %v\n", apiPath, err)
		}

		if ctx.Err() != nil {
			return
		}

		// Brief backoff before reconnecting after a disconnect/error.
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// streamWatch opens a single watch stream via client-go's dynamic client and
// records each ADDED/MODIFIED/DELETED event. Using the dynamic client (rather
// than a hand-rolled HTTP request + json.Decoder) means watch events are
// decoded by client-go's StreamWatcher, which negotiates the correct content
// type and reliably streams events over HTTP/2 — a raw request would connect
// but never receive any frames against some API servers.
//
// namespace is "" for a cluster-wide stream (cluster-scoped resources and the
// single stream used for wildcard-namespaced resources); otherwise it is the
// specific namespace to watch. Returning nil/err drives watchResourcePath's
// reconnect loop, which re-bootstraps a fresh resourceVersion before retrying.
func (e *Engine) streamWatch(ctx context.Context, res config.Resource, apiPath, namespace string) error {
	// Wildcard-namespace watches open a single cluster-wide stream but rewrite
	// each event's stored APIPath to /api/.../namespaces/<ns>/<resource> so the
	// replay server's per-namespace reconstruction logic continues to work
	// without changes.
	demuxPerNamespace := res.WildcardNamespaces && apiPath == buildAPIPath(res.Group, res.Version, res.Resource, "")

	gvr := schema.GroupVersionResource{Group: res.Group, Version: res.Version, Resource: res.Resource}

	var ri dynamic.ResourceInterface = e.dynClient.Resource(gvr)
	if namespace != "" {
		ri = e.dynClient.Resource(gvr).Namespace(namespace)
	}

	// Bootstrap a starting resourceVersion with a cheap one-item list so the
	// watch streams only changes from this point forward rather than replaying
	// every existing object as an ADDED event. The list metadata carries the
	// collection resourceVersion regardless of how many items are returned.
	opts := metav1.ListOptions{}
	if l, err := ri.List(ctx, metav1.ListOptions{Limit: 1}); err == nil {
		opts.ResourceVersion = l.GetResourceVersion()
	} else if ctx.Err() != nil {
		return nil
	}

	w, err := ri.Watch(ctx, opts)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer w.Stop()

	if e.verbose {
		fmt.Fprintf(os.Stdout, "  [watch] %s connected\n", apiPath)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-w.ResultChan():
			if !ok {
				// Server closed the stream; the caller re-lists and reconnects.
				return nil
			}

			var eventType string
			switch event.Type {
			case watch.Added:
				eventType = "ADDED"
			case watch.Modified:
				eventType = "MODIFIED"
			case watch.Deleted:
				eventType = "DELETED"
			case watch.Error:
				// Typically a 410 Expired Status. Log and let the channel close
				// so watchResourcePath re-lists for a fresh resourceVersion.
				if e.verbose {
					msg := "unknown watch error"
					if st, ok := event.Object.(*metav1.Status); ok && st.Message != "" {
						msg = st.Message
					}
					fmt.Fprintf(os.Stderr, "  [watch] %s: error event: %s\n", apiPath, msg)
				}
				continue
			default:
				// Bookmark and any other types are not transitions.
				continue
			}

			u, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			body, err := u.MarshalJSON()
			if err != nil {
				continue
			}

			recordPath := apiPath
			if demuxPerNamespace {
				ns := u.GetNamespace()
				if ns == "" {
					// Namespaced resources should always have metadata.namespace
					// set on watch events. Skip rather than store an unattributable
					// event under the cluster-wide path where the replay server
					// won't find it.
					continue
				}
				recordPath = buildAPIPath(res.Group, res.Version, res.Resource, ns)
			}

			rec := &Record{
				ID:           uuid.New().String(),
				CapturedAt:   time.Now().UTC(),
				APIPath:      recordPath,
				EventType:    eventType,
				HTTPMethod:   http.MethodGet,
				ResponseCode: http.StatusOK,
				ResponseBody: body,
			}

			if e.sink == nil {
				continue
			}
			seq, err := e.sink.WriteRecord(rec)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  [warn] writing watch record %s: %v\n", recordPath, err)
				e.mu.Lock()
				if e.captureErr == nil {
					e.captureErr = fmt.Errorf("writing watch record %s: %w", recordPath, err)
				}
				e.mu.Unlock()
				continue
			}

			e.mu.Lock()
			if _, ok := e.watchIndex[recordPath]; !ok {
				e.watchIndex[recordPath] = &WatchIndexEntry{APIPath: recordPath}
			}
			e.watchIndex[recordPath].Seqs = append(e.watchIndex[recordPath].Seqs, seq)
			e.watchIndex[recordPath].Times = append(e.watchIndex[recordPath].Times, rec.CapturedAt)
			e.watchIndex[recordPath].EventTypes = append(e.watchIndex[recordPath].EventTypes, rec.EventType)
			e.mu.Unlock()
		}
	}
}

// fetchResource issues one GET for res and stores the record. It also fetches
// the Table-format response so the mock server can replay rich column definitions.
//
// For wildcard-namespaced resources (namespaces: ["*"]), polling is performed
// against the cluster-wide endpoint and the response is demuxed into
// per-namespace records — this catches "zombie" resources whose namespace has
// been deleted but which still exist in etcd. Such items are invisible to
// per-namespace polling because the deleted namespace isn't in
// /api/v1/namespaces. See fetchResourceClusterWide for details.
func (e *Engine) fetchResource(ctx context.Context, res config.Resource) {
	if res.WildcardNamespaces {
		e.fetchResourceClusterWide(ctx, res)
		return
	}

	namespaces := res.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}

	// Track whether every explicitly-namespaced fetch returned 404. If so, the
	// resource is likely cluster-scoped and the config has 'namespaces:' set by
	// mistake — warn and also capture the cluster-scoped path as a fallback.
	// Only a genuine 404 counts as evidence of cluster scope: a code of 0 means
	// the fetch failed transiently (timeout/transport/cancellation), which must
	// not be mistaken for "this resource isn't served per-namespace" or every
	// flaky fetch would trigger a spurious cluster-wide fallback.
	allNotFound := len(res.Namespaces) > 0
	dedupEnabled := res.DedupEnabled()
	for _, ns := range namespaces {
		apiPath := buildAPIPath(res.Group, res.Version, res.Resource, ns)
		_, code := e.doFetch(ctx, apiPath, "", dedupEnabled)
		if code != http.StatusNotFound {
			allNotFound = false
		}
		e.doFetch(ctx, apiPath, tableIndexKeySuffix, dedupEnabled)
	}

	if allNotFound {
		clusterPath := buildAPIPath(res.Group, res.Version, res.Resource, "")
		// For auto-discovered resources the namespace assignment came from the
		// Kubernetes discovery API, not from user config. Some resources
		// (especially OpenShift CRDs) report "namespaced" in discovery but only
		// serve data at the cluster-scoped path. Silently fall back rather than
		// printing a misleading "remove 'namespaces:'" hint the user can't act on.
		if !res.AutoDiscovered && e.verbose {
			// Deduplicate: only warn once per unique cluster-scoped path per run.
			e.mu.Lock()
			alreadyWarned := e.warnedFallback[clusterPath]
			if !alreadyWarned {
				e.warnedFallback[clusterPath] = true
			}
			e.mu.Unlock()
			if !alreadyWarned {
				fmt.Fprintf(os.Stderr,
					"  [warn] %s: all namespace-scoped fetches returned 404 — "+
						"this is likely a cluster-scoped resource; remove 'namespaces:' "+
						"from its config entry. Fetching cluster-scoped path %s as fallback.\n",
					res.Resource, clusterPath)
			}
		} else if res.AutoDiscovered && e.verbose {
			e.mu.Lock()
			alreadyWarned := e.warnedFallback[clusterPath]
			if !alreadyWarned {
				e.warnedFallback[clusterPath] = true
			}
			e.mu.Unlock()
			if !alreadyWarned {
				fmt.Fprintf(os.Stderr,
					"  [debug] %s: all namespace-scoped fetches returned 404; falling back to cluster-scoped path %s\n",
					res.Resource, clusterPath)
			}
		}
		e.doFetch(ctx, clusterPath, "", dedupEnabled)
		e.doFetch(ctx, clusterPath, tableIndexKeySuffix, dedupEnabled)
	}
}

// fetchResourceClusterWide polls a wildcard-namespaced resource via the
// cluster-wide endpoint /apis/<g>/<v>/<r> (or /api/v1/<r> for core) and
// demuxes the response into per-namespace records keyed by metadata.namespace
// on each item. Catches "zombie" resources whose namespace has been deleted
// — invisible to per-namespace polling because the deleted namespace isn't
// in /api/v1/namespaces, the source of wildcard expansion.
//
// A side effect of demuxing is one API call per cycle instead of N, which
// also reduces API server load on clusters with many namespaces.
func (e *Engine) fetchResourceClusterWide(ctx context.Context, res config.Resource) {
	clusterPath := buildAPIPath(res.Group, res.Version, res.Resource, "")
	dedupEnabled := res.DedupEnabled()

	if body, code := e.rawFetch(ctx, clusterPath, ""); body != nil {
		e.demuxClusterPlainList(res, clusterPath, body, code, dedupEnabled)
	}
	if body, code := e.rawFetch(ctx, clusterPath, tableIndexKeySuffix); body != nil {
		e.demuxClusterTableList(res, clusterPath, body, code, dedupEnabled)
	}
}
