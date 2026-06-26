package v2

import (
	"net/http"
	"testing"
)

func TestServeResourceCatalog(t *testing.T) {
	h := newFleetTestHandler(t)
	var rc ResourceCatalog
	if code := getJSONInto(t, h, h.serveResourceCatalog, "/v2/api/resources", "", &rc); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if rc.Total == 0 || len(rc.Resources) == 0 {
		t.Fatalf("expected resources in catalog, got %+v", rc)
	}
	var pods *ResourceCatalogRow
	for i := range rc.Resources {
		if rc.Resources[i].Resource == "pods" {
			pods = &rc.Resources[i]
		}
	}
	if pods == nil {
		t.Fatalf("no pods row in catalog: %+v", rc.Resources)
	}
	if pods.Kind != "Pod" {
		t.Errorf("pods.Kind = %q, want Pod", pods.Kind)
	}
	if pods.Count != 2 {
		t.Errorf("pods.Count = %d, want 2", pods.Count)
	}
}

func TestServeResourceCatalog_NilStore(t *testing.T) {
	h := &Handler{}
	if code := getJSONInto(t, h, h.serveResourceCatalog, "/v2/api/resources", "", nil); code != http.StatusInternalServerError {
		t.Errorf("nil store: status = %d, want 500", code)
	}
}

func TestServeResourceList(t *testing.T) {
	h := newFleetTestHandler(t)
	var rl ResourceList
	if code := getJSONInto(t, h, h.serveResourceList, "/v2/api/resource", "?resource=pods", &rl); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if rl.Resource != "pods" || rl.Kind != "Pod" {
		t.Errorf("Resource/Kind = %q/%q, want pods/Pod", rl.Resource, rl.Kind)
	}
	if rl.Total != 2 || len(rl.Items) != 2 {
		t.Fatalf("Total/items = %d/%d, want 2", rl.Total, len(rl.Items))
	}
	names := map[string]bool{}
	for _, it := range rl.Items {
		names[it.Name] = true
	}
	if !names["web"] || !names["crasher"] {
		t.Errorf("expected web and crasher, got %+v", rl.Items)
	}
}

func TestServeResourceList_Errors(t *testing.T) {
	h := newFleetTestHandler(t)
	if code := getJSONInto(t, h, h.serveResourceList, "/v2/api/resource", "", nil); code != http.StatusBadRequest {
		t.Errorf("missing resource: status = %d, want 400", code)
	}
	nh := &Handler{}
	if code := getJSONInto(t, nh, nh.serveResourceList, "/v2/api/resource", "?resource=pods", nil); code != http.StatusInternalServerError {
		t.Errorf("nil store: status = %d, want 500", code)
	}
}
