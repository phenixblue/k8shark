package anonymize

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// scaleFixture writes a synthetic capture shaped like a real cluster of
// `pods` pods: one namespace per 25 pods, one node per 50, a Deployment (with
// its ReplicaSet) per 5, one Event per pod, and a distinct pod IP each. It
// returns the archive path and the number of records written.
func scaleFixture(t testing.TB, pods int) (string, int) {
	t.Helper()
	nsCount := max(1, pods/25)
	nodeCount := max(1, pods/50)
	podsPerDeploy := 5
	deployCount := max(1, pods/podsPerDeploy)

	path := filepath.Join(t.TempDir(), "scale.kshrk")
	sw, err := archive.NewStreamWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := capture.Index{}
	records := 0
	write := func(apiPath string, body any) {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := &capture.Record{ID: fmt.Sprintf("r%d", records), CapturedAt: fixedNow, APIPath: apiPath, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: b}
		seq, err := sw.WriteRecord(rec)
		if err != nil {
			t.Fatal(err)
		}
		e := idx[apiPath]
		if e == nil {
			e = &capture.IndexEntry{APIPath: apiPath}
			idx[apiPath] = e
		}
		e.Seqs = append(e.Seqs, seq)
		e.Times = append(e.Times, fixedNow)
		records++
	}

	// A real name has a random-looking suffix; make each one distinct and
	// not derivable from its index, like the real thing.
	rnd := uint64(0x9e3779b97f4a7c15)
	next := func() uint64 { rnd ^= rnd << 13; rnd ^= rnd >> 7; rnd ^= rnd << 17; return rnd }
	suffix := func(n int) string {
		const a = "bcdfghjklmnpqrstvwxz2456789"
		var sb strings.Builder
		for i := 0; i < n; i++ {
			sb.WriteByte(a[next()%uint64(len(a))])
		}
		return sb.String()
	}

	nodes := make([]string, nodeCount)
	nodeItems := make([]map[string]any, nodeCount)
	for i := range nodes {
		nodes[i] = fmt.Sprintf("ip-10-%d-%d-%d.ec2.internal", 20+i/65025, (i/255)%255, i%255)
		nodeItems[i] = map[string]any{"kind": "Node", "metadata": map[string]any{"name": nodes[i]},
			"status": map[string]any{"addresses": []any{map[string]any{"type": "Hostname", "address": nodes[i]}}}}
	}
	write("/api/v1/nodes", map[string]any{"kind": "NodeList", "apiVersion": "v1", "items": nodeItems})

	type nsData struct{ pods, deploys, rss, events []map[string]any }
	data := make([]nsData, nsCount)
	podN := 0
	nsNames := make([]string, nsCount)
	for i := range nsNames {
		nsNames[i] = fmt.Sprintf("team-%s-%s", suffix(4), suffix(3))
	}
	for d := 0; d < deployCount && podN < pods; d++ {
		n := d % nsCount
		ns := nsNames[n]
		dep := fmt.Sprintf("%s-%s", suffix(5), suffix(4))
		rs := fmt.Sprintf("%s-%s", dep, suffix(10))
		data[n].deploys = append(data[n].deploys, map[string]any{"kind": "Deployment",
			"metadata": map[string]any{"name": dep, "namespace": ns, "labels": map[string]any{"app": dep}}})
		data[n].rss = append(data[n].rss, map[string]any{"kind": "ReplicaSet",
			"metadata": map[string]any{"name": rs, "namespace": ns, "ownerReferences": []any{map[string]any{"kind": "Deployment", "name": dep}}}})
		for p := 0; p < podsPerDeploy && podN < pods; p++ {
			pod := fmt.Sprintf("%s-%s", rs, suffix(5))
			ip := fmt.Sprintf("10.%d.%d.%d", 128+podN/65025, (podN/255)%255, 1+podN%254)
			data[n].pods = append(data[n].pods, map[string]any{"kind": "Pod",
				"metadata": map[string]any{"name": pod, "namespace": ns, "labels": map[string]any{"app": dep},
					"ownerReferences": []any{map[string]any{"kind": "ReplicaSet", "name": rs}}},
				"spec":   map[string]any{"nodeName": nodes[podN%nodeCount], "containers": []any{map[string]any{"name": "app", "image": "registry.example.com/team/app:v1"}}},
				"status": map[string]any{"podIP": ip, "podIPs": []any{map[string]any{"ip": ip}}}})
			data[n].events = append(data[n].events, map[string]any{"kind": "Event",
				"metadata":       map[string]any{"name": pod + "." + suffix(16), "namespace": ns},
				"involvedObject": map[string]any{"kind": "Pod", "name": pod, "namespace": ns},
				"message":        "Successfully assigned " + ns + "/" + pod + " to " + nodes[podN%nodeCount]})
			podN++
		}
	}
	for n, ns := range nsNames {
		write("/api/v1/namespaces/"+ns, map[string]any{"kind": "Namespace", "apiVersion": "v1", "metadata": map[string]any{"name": ns}})
		write("/api/v1/namespaces/"+ns+"/pods", map[string]any{"kind": "PodList", "apiVersion": "v1", "items": data[n].pods})
		write("/apis/apps/v1/namespaces/"+ns+"/deployments", map[string]any{"kind": "DeploymentList", "apiVersion": "apps/v1", "items": data[n].deploys})
		write("/apis/apps/v1/namespaces/"+ns+"/replicasets", map[string]any{"kind": "ReplicaSetList", "apiVersion": "apps/v1", "items": data[n].rss})
		write("/api/v1/namespaces/"+ns+"/events", map[string]any{"kind": "EventList", "apiVersion": "v1", "items": data[n].events})
	}

	meta := &capture.CaptureMetadata{CaptureID: "scale", CapturedAt: fixedNow, CapturedUntil: fixedNow, RecordCount: records, ServerAddress: "https://10.0.0.1:6443"}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatal(err)
	}
	return path, records
}

// TestScale measures the whole pipeline (every category, --full-sweep,
// --verify) against a synthetic capture of KSHRK_ANON_SCALE_PODS pods, over
// KSHRK_ANON_SCALE_SALTS different salts (default 5), and logs, per salt,
// whether it succeeded, how long it took, and peak heap. It is skipped unless
// KSHRK_ANON_SCALE_PODS is set, since a large run takes a while:
//
//	KSHRK_ANON_SCALE_PODS=5000 go test ./internal/anonymize -run TestScale -v -timeout 30m
//
// It reports rather than asserts: its job is to answer "does this work on a
// cluster my size, and how often?". TestAliasSpaceScales holds the guarantee.
func TestScale(t *testing.T) {
	podsEnv := os.Getenv("KSHRK_ANON_SCALE_PODS")
	if podsEnv == "" {
		t.Skip("set KSHRK_ANON_SCALE_PODS=<n> to run the scale measurement")
	}
	pods, err := strconv.Atoi(podsEnv)
	if err != nil || pods <= 0 {
		t.Fatalf("KSHRK_ANON_SCALE_PODS=%q is not a positive integer", podsEnv)
	}
	salts := 5
	if s := os.Getenv("KSHRK_ANON_SCALE_SALTS"); s != "" {
		if salts, err = strconv.Atoi(s); err != nil || salts <= 0 {
			t.Fatalf("KSHRK_ANON_SCALE_SALTS=%q is not a positive integer", s)
		}
	}

	genStart := time.Now()
	src, records := scaleFixture(t, pods)
	fi, _ := os.Stat(src)
	t.Logf("fixture: %d pods, %d records, %.1f MiB on disk, generated in %s", pods, records, float64(fi.Size())/(1<<20), time.Since(genStart).Round(time.Millisecond))

	ok := 0
	for i := 0; i < salts; i++ {
		var peak atomic.Uint64
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			var ms runtime.MemStats
			for {
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > peak.Load() {
					peak.Store(ms.HeapAlloc)
				}
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}()
		runtime.GC()
		start := time.Now()
		res, err := Archive(src, filepath.Join(t.TempDir(), "out.kshrk"), Options{
			Categories: allCategories, Salt: []byte(fmt.Sprintf("scale-salt-%d", i)), FullSweep: true, Verify: true,
		})
		elapsed := time.Since(start).Round(time.Millisecond)
		close(stop)
		<-done
		if err != nil {
			msg := err.Error()
			if len(msg) > 150 {
				msg = msg[:150] + "…"
			}
			t.Logf("salt %d: FAILED after %s (peak heap %.0f MiB): %s", i, elapsed, float64(peak.Load())/(1<<20), msg)
			continue
		}
		ok++
		t.Logf("salt %d: ok in %s (peak heap %.0f MiB); ns=%d nodes=%d pods=%d workloads=%d ips=%d; sweep=%d residual=%d",
			i, elapsed, float64(peak.Load())/(1<<20), res.NamespacesRenamed, res.NodesRenamed, res.PodsRenamed, res.WorkloadsRenamed, res.IPsRenamed, res.SweepOccurrencesFound, res.ResidualOccurrences)
	}
	t.Logf("RESULT: %d/%d salts succeeded at %d pods", ok, salts, pods)
}
