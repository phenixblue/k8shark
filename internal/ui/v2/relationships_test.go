package v2

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

func TestServeObjectRelationships(t *testing.T) {
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	path := "/api/v1/namespaces/default/pods"
	podList := `{"apiVersion":"v1","kind":"PodList","items":[` +
		`{"metadata":{"name":"web","namespace":"default",` +
		`"ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"web-5d4","uid":"u1"}]},` +
		`"spec":{"containers":[{"name":"app"}]}}]}`
	recs := []*capture.Record{
		{ID: "p1", CapturedAt: now, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podList)},
	}
	idx := capture.Index{path: {APIPath: path, Seqs: []int{0}, Times: []time.Time{now}, Counts: []int{1}}}
	meta := &capture.CaptureMetadata{CaptureID: "rel-test", CapturedAt: now.Add(-time.Minute), CapturedUntil: now, RecordCount: len(recs)}
	h := &Handler{Store: buildV2TestStore(t, recs, idx, meta), At: now}

	var resp ObjectRelationships
	q := "?path=" + url.QueryEscape(path) + "&name=web"
	if code := getJSONInto(t, h, h.serveObjectRelationships, "/v2/api/object-relationships", q, &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	var owned *RelatedGroup
	for i := range resp.Groups {
		if resp.Groups[i].Title == "Owned by" {
			owned = &resp.Groups[i]
		}
	}
	if owned == nil {
		t.Fatalf("no 'Owned by' group: %+v", resp.Groups)
	}
	if len(owned.Items) != 1 || owned.Items[0].Kind != "ReplicaSet" || owned.Items[0].Name != "web-5d4" {
		t.Errorf("owner item = %+v", owned.Items)
	}
}

func TestServeObjectRelationships_Errors(t *testing.T) {
	h := newFleetTestHandler(t)
	if code := getJSONInto(t, h, h.serveObjectRelationships, "/v2/api/object-relationships", "?path=/x", nil); code != http.StatusBadRequest {
		t.Errorf("missing name: status = %d, want 400", code)
	}
	nh := &Handler{}
	if code := getJSONInto(t, nh, nh.serveObjectRelationships, "/v2/api/object-relationships", "?path=/x&name=y", nil); code != http.StatusInternalServerError {
		t.Errorf("nil store: status = %d, want 500", code)
	}
}
