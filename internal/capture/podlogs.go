package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/phenixblue/k8shark/internal/config"
)

// Pod log fetch tuning.
const (
	// maxConcurrentLogFetches bounds the number of in-flight pod-log GETs.
	// Pod logs are fetched once at the end of the capture; on large clusters
	// (thousands of containers) parallelism is the difference between the
	// pass finishing in seconds vs. hitting a global timeout.
	maxConcurrentLogFetches = 16
	// perPodLogTimeout caps any single pod-log fetch so one hung container
	// can't stall the whole pass. Replaces the previous global 30s budget.
	perPodLogTimeout = 15 * time.Second
)

// formatHTTPFailure builds a concise human-readable reason for a non-OK HTTP
// response. When the body is a Kubernetes Status JSON envelope (the typical
// API server error format), only the `message` field is shown — much more
// readable than the raw JSON, which is what users see for pod-log failures
// like "container X is terminated" or "container Y is waiting to start".
// Falls back to a truncated raw body when the response is not a Status object.
func formatHTTPFailure(statusCode int, body []byte) string {
	reason := fmt.Sprintf("HTTP %d", statusCode)
	if len(body) == 0 {
		return reason
	}
	if msg := extractStatusMessage(body); msg != "" {
		const maxMessageLen = 300
		if len(msg) > maxMessageLen {
			msg = msg[:maxMessageLen] + "..."
		}
		return reason + ": " + msg
	}
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) > 160 {
		trimmed = trimmed[:160] + "..."
	}
	return reason + ": " + trimmed
}

// extractStatusMessage returns the `message` field of a Kubernetes Status
// JSON object, or "" if the body isn't a Status envelope.
func extractStatusMessage(body []byte) string {
	var s struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return ""
	}
	if s.Kind != "Status" {
		return ""
	}
	return s.Message
}

// fetchPodsLogs fetches the tail log for each container of each pod found in
// res across all configured namespaces. Each log is stored under
// /api/v1/namespaces/<ns>/pods/<name>/log?container=<c> so the replay server
// can route both bare `kubectl logs <pod>` (single-container) and
// `kubectl logs <pod> -c <c>` requests to the right record.
//
// Fetches run concurrently bounded by maxConcurrentLogFetches, and each fetch
// has its own perPodLogTimeout so one hung container does not stall the pass.
// Per-fetch failures (HTTP errors, RBAC, timeouts, terminated containers) are
// recorded in the returned PodLogSummary so the CLI can show users what was
// skipped without re-running in verbose mode.
func (e *Engine) fetchPodsLogs(ctx context.Context, res config.Resource) PodLogSummary {
	type job struct {
		namespace string
		pod       string
		container string
		previous  bool
	}

	namespaces := res.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}

	var jobs []job
	for _, ns := range namespaces {
		listPath := buildAPIPath(res.Group, res.Version, res.Resource, ns)
		listBody, code := e.doFetch(ctx, listPath, "", res.DedupEnabled())
		if code != 200 || listBody == nil {
			continue
		}
		var list struct {
			Items []struct {
				Metadata struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"metadata"`
				Spec struct {
					Containers []struct {
						Name string `json:"name"`
					} `json:"containers"`
					InitContainers []struct {
						Name string `json:"name"`
					} `json:"initContainers"`
				} `json:"spec"`
			} `json:"items"`
		}
		if err := json.Unmarshal(listBody, &list); err != nil {
			continue
		}
		for _, item := range list.Items {
			podNS := item.Metadata.Namespace
			if podNS == "" {
				podNS = ns
			}
			queue := func(container string) {
				jobs = append(jobs, job{namespace: podNS, pod: item.Metadata.Name, container: container})
				if res.PreviousLogs {
					jobs = append(jobs, job{namespace: podNS, pod: item.Metadata.Name, container: container, previous: true})
				}
			}
			// Init containers first — they ran before main containers and
			// often carry the most actionable diagnostic output on Pending
			// or CrashLoopBackOff pods.
			for _, c := range item.Spec.InitContainers {
				queue(c.Name)
			}
			for _, c := range item.Spec.Containers {
				queue(c.Name)
			}
		}
	}

	if len(jobs) == 0 {
		return PodLogSummary{}
	}

	// Only current-log fetches count toward the user-facing summary so it
	// stays focused on the headline number ("X/Y captured"). Previous-log
	// fetches are best-effort: successes grow CapturedPrevious, but failures
	// (the very common "container has not been previously terminated" 400)
	// are dropped silently to avoid swamping the failure sample with noise.
	attempted := 0
	for _, j := range jobs {
		if !j.previous {
			attempted++
		}
	}

	var (
		mu      sync.Mutex
		summary = PodLogSummary{Attempted: attempted}
		wg      sync.WaitGroup
	)
	sem := make(chan struct{}, maxConcurrentLogFetches)

	for _, j := range jobs {
		sem <- struct{}{}
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			result := e.fetchOnePodLog(ctx, j.namespace, j.pod, j.container, res.Logs, j.previous)

			mu.Lock()
			defer mu.Unlock()
			if j.previous {
				if result.captured {
					summary.CapturedPrevious++
				}
				return
			}
			if result.captured {
				summary.Captured++
				return
			}
			summary.Skipped++
			if result.failure != nil && len(summary.Failures) < podLogFailureSampleLimit {
				summary.Failures = append(summary.Failures, *result.failure)
			}
		}(j)
	}
	wg.Wait()
	return summary
}

type logFetchResult struct {
	captured bool
	failure  *PodLogFailure // nil when captured is true
}

// fetchOnePodLog fetches the last tailLines of one container's log and stores
// the plain-text content as a JSON-encoded string under
// /api/v1/namespaces/<ns>/pods/<name>/log?container=<c> (or, when previous is
// true, …&previous=true). The query-suffix index key lets the replay server
// route `kubectl logs <pod> -c <c> [--previous]` to the matching record.
func (e *Engine) fetchOnePodLog(ctx context.Context, namespace, podName, containerName string, tailLines int, previous bool) logFetchResult {
	fetchCtx, cancel := context.WithTimeout(ctx, perPodLogTimeout)
	defer cancel()

	logPath := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", namespace, podName)
	indexKey := logPath + "?container=" + containerName
	fetchURL := fmt.Sprintf("%s%s?container=%s&tailLines=%d", e.baseURL, logPath, url.QueryEscape(containerName), tailLines)
	if previous {
		indexKey += "&previous=true"
		fetchURL += "&previous=true"
	}

	failure := func(reason string) logFetchResult {
		if e.verbose {
			marker := ""
			if previous {
				marker = ",previous=true"
			}
			fmt.Fprintf(os.Stderr, "  [warn] log %s/%s [container=%s%s]: %s\n", namespace, podName, containerName, marker, reason)
		}
		return logFetchResult{failure: &PodLogFailure{
			Namespace: namespace,
			Pod:       podName,
			Container: containerName,
			Reason:    reason,
		}}
	}

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return failure(fmt.Sprintf("build request: %v", err))
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		if fetchCtx.Err() != nil {
			return failure("timeout after " + perPodLogTimeout.String())
		}
		return failure(fmt.Sprintf("request error: %v", err))
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return failure(formatHTTPFailure(resp.StatusCode, body))
	}
	if readErr != nil {
		return failure(fmt.Sprintf("reading body: %v", readErr))
	}

	jsonBody, err := json.Marshal(string(body))
	if err != nil {
		return failure(fmt.Sprintf("encoding body: %v", err))
	}

	if e.verbose {
		fmt.Fprintf(os.Stdout, "  [capture] %s -> %d (%d bytes)\n", indexKey, resp.StatusCode, len(body))
	}

	rec := &Record{
		ID:           uuid.New().String(),
		CapturedAt:   time.Now().UTC(),
		APIPath:      indexKey,
		HTTPMethod:   http.MethodGet,
		ResponseCode: http.StatusOK,
		ResponseBody: json.RawMessage(jsonBody),
	}
	if e.sink == nil {
		return failure("no output sink configured")
	}
	seq, err := e.sink.WriteRecord(rec)
	if err != nil {
		return failure(fmt.Sprintf("writing record: %v", err))
	}

	e.mu.Lock()
	if _, ok := e.index[indexKey]; !ok {
		e.index[indexKey] = &IndexEntry{APIPath: indexKey}
	}
	e.index[indexKey].Seqs = append(e.index[indexKey].Seqs, seq)
	e.index[indexKey].Times = append(e.index[indexKey].Times, rec.CapturedAt)
	e.mu.Unlock()

	return logFetchResult{captured: true}
}
