package capture

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/phenixblue/k8shark/internal/config"
)

// TestNewEngine_NoNilMapOrChanFields guards #238: NewEngine and
// newEngineWith independently initialized the same thirteen struct fields,
// so adding a field to one constructor and forgetting the other shipped a
// nil-map panic (or a zero-value timeout) on the very first real capture,
// with a fully green test suite — every other test goes through
// newEngineWith, never NewEngine. Drive NewEngine itself (0% coverage
// before this) via a temp kubeconfig pointing at an httptest server, and
// assert no map or channel field is left nil.
func TestNewEngine_NoNilMapOrChanFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e, err := NewEngine(&config.Config{Kubeconfig: writeTestKubeconfig(t, srv.URL)}, false)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// reflect.ValueOf(e).Elem(), not reflect.ValueOf(*e) — dereferencing *e
	// would copy the whole struct, including its embedded sync.Mutex, which
	// go vet correctly flags as a lock-copy.
	v := reflect.ValueOf(e).Elem()
	typ := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() { //nolint:exhaustive // only map/chan fields can be nil in a way that matters here
		case reflect.Map, reflect.Chan:
			if f.IsNil() {
				t.Errorf("field %s is nil after NewEngine — newEngineBase must initialize it", typ.Field(i).Name)
			}
		}
	}

	if e.baseURL != srv.URL {
		t.Errorf("baseURL = %q, want %q", e.baseURL, srv.URL)
	}
	if e.httpClient == nil {
		t.Error("httpClient is nil")
	}
	if e.dynClient == nil {
		t.Error("dynClient is nil")
	}
	if e.fetchTimeout != perFetchTimeout {
		t.Errorf("fetchTimeout = %v, want the default %v (zero value would silently disable the per-fetch cap)", e.fetchTimeout, perFetchTimeout)
	}
}

func writeTestKubeconfig(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	contents := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
users:
- name: test
  user: {}
`, server)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}
