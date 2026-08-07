package store

import (
	"encoding/json"
	"fmt"
	"testing"
)

// These guard the decode-avoidance in FieldSelector.Matches. Every metadata,
// spec and status path resolves from the K8sObject the caller already decoded;
// only fields outside those (Events, Secrets' type) need the object unmarshaled a
// second time. When that held for spec paths too, filtering a 1000-pod list on
// spec.nodeName cost about half again the wall time and better than twice the
// allocations of the same list filtered on metadata.namespace. The two should now
// track each other closely — a gap reopening means the classification regressed.
//
// SpecPath and MetadataOnly are the comparison; EventRawPath measures the case
// that genuinely cannot avoid the second decode.

// benchPods builds a list of realistically sized pods. The saved decode scales
// with the whole object, not with the fields the selector reads, so a toy object
// would understate it.
func benchPods(n int) []json.RawMessage {
	out := make([]json.RawMessage, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, json.RawMessage(fmt.Sprintf(`{
		  "apiVersion":"v1","kind":"Pod",
		  "metadata":{"name":"web-%d","namespace":"demo","uid":"u-%d",
		    "labels":{"app":"web","pod-template-hash":"5bff58cf7","tier":"frontend"},
		    "annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"a\":1,\"b\":2,\"c\":3}"},
		    "ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"web","uid":"r-1"}]},
		  "spec":{"nodeName":"node-a","restartPolicy":"Always","schedulerName":"default-scheduler",
		    "serviceAccountName":"default","hostNetwork":false,
		    "containers":[{"name":"web","image":"nginx:1.27","ports":[{"containerPort":80}],
		      "resources":{"requests":{"cpu":"10m","memory":"16Mi"},"limits":{"memory":"64Mi"}},
		      "volumeMounts":[{"name":"kube-api-access","mountPath":"/var/run/secrets/k8s.io"}]}],
		    "volumes":[{"name":"kube-api-access","projected":{"sources":[{"serviceAccountToken":{"path":"token"}}]}}]},
		  "status":{"phase":"Running","podIP":"10.244.0.%d","hostIP":"172.18.0.2",
		    "conditions":[{"type":"Ready","status":"True"},{"type":"Initialized","status":"True"}],
		    "containerStatuses":[{"name":"web","ready":true,"restartCount":0,"image":"nginx:1.27"}]}}`,
			i, i, i%250)))
	}
	return out
}

func benchmarkFilterItems(b *testing.B, group, resource, selector string, items []json.RawMessage) {
	b.Helper()
	fs, err := ParseFieldSelector(group, resource, selector)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// FilterItems filters in place, so hand it a fresh slice each round.
		in := make([]json.RawMessage, len(items))
		copy(in, items)
		FilterItems(in, "", fs)
	}
}

func BenchmarkFilterItems_MetadataOnly(b *testing.B) {
	benchmarkFilterItems(b, "", "pods", "metadata.namespace=demo", benchPods(1000))
}

func BenchmarkFilterItems_SpecPath(b *testing.B) {
	benchmarkFilterItems(b, "", "pods", "spec.nodeName=node-a", benchPods(1000))
}

func BenchmarkFilterItems_StatusPath(b *testing.B) {
	benchmarkFilterItems(b, "", "pods", "status.phase=Running", benchPods(1000))
}

func BenchmarkFilterItems_EventRawPath(b *testing.B) {
	items := make([]json.RawMessage, 0, 1000)
	for i := 0; i < 1000; i++ {
		items = append(items, json.RawMessage(fmt.Sprintf(
			`{"metadata":{"name":"e-%d","namespace":"demo"},`+
				`"involvedObject":{"kind":"Pod","name":"web-%d","namespace":"demo"},`+
				`"reason":"Scheduled","type":"Normal","source":{"component":"default-scheduler"},`+
				`"message":"Successfully assigned demo/web-%d to node-a","count":1}`, i, i, i)))
	}
	benchmarkFilterItems(b, "", "events", "involvedObject.kind=Pod", items)
}
