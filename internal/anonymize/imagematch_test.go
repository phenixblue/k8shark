package anonymize

import (
	"encoding/json"
	"testing"

	"github.com/phenixblue/k8shark/internal/capture"
)

func TestRewriteImageRegistryHost(t *testing.T) {
	cases := []struct {
		name   string
		image  string
		want   string
		wantOK bool
	}{
		{
			name:   "explicit registry with dot",
			image:  "registry.internal.corp/team/app:v1.2.3",
			want:   "registry.internal.corp-ALIASED/team/app:v1.2.3",
			wantOK: true,
		},
		{
			name:   "explicit registry with host:port",
			image:  "registry.internal.corp:5000/team/app:v1.2.3",
			want:   "registry.internal.corp:5000-ALIASED/team/app:v1.2.3",
			wantOK: true,
		},
		{
			name:   "localhost registry",
			image:  "localhost/myapp:dev",
			want:   "localhost-ALIASED/myapp:dev",
			wantOK: true,
		},
		{
			name:   "port-only registry (no dot, has colon)",
			image:  "myregistry:5000/team/app",
			want:   "myregistry:5000-ALIASED/team/app",
			wantOK: true,
		},
		{
			name:   "no slash at all — implicit Docker Hub, single component",
			image:  "nginx:1.21",
			want:   "nginx:1.21",
			wantOK: false,
		},
		{
			name:   "no slash, digest form",
			image:  "nginx@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			want:   "nginx@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			wantOK: false,
		},
		{
			name:   "org/repo with no dot or colon — Docker Hub namespace, not a registry",
			image:  "myorg/myapp:v1",
			want:   "myorg/myapp:v1",
			wantOK: false,
		},
		{
			name:   "registry with a path and a digest",
			image:  "gcr.io/my-project/my-app@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			want:   "gcr.io-ALIASED/my-project/my-app@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			wantOK: true,
		},
		// CRI-O's containerStatuses[*].imageID commonly carries a
		// "docker-pullable://" scheme prefix ahead of the actual reference
		// — without accounting for it, the first "/" lands inside the
		// scheme separator's own "://", and "docker-pullable:" (which
		// contains a ':') would be misidentified as the registry host.
		{
			name:   "CRI-prefixed imageID (docker-pullable://)",
			image:  "docker-pullable://docker.io/library/nginx@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			want:   "docker-pullable://docker.io-ALIASED/library/nginx@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			wantOK: true,
		},
		{
			name:   "CRI-prefixed imageID, implicit Docker Hub (no explicit registry)",
			image:  "docker://nginx:alpine",
			want:   "docker://nginx:alpine",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rewriteImageRegistryHost(tc.image, upper)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("rewriteImageRegistryHost(%q) = (%q, %v), want (%q, %v)", tc.image, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestRewriteImageRegistryInRecord(t *testing.T) {
	t.Run("Pod spec.containers and status.containerStatuses", func(t *testing.T) {
		body := `{"kind":"Pod","spec":{"containers":[{"name":"app","image":"registry.internal.corp/team/app:v1"}]},
			"status":{"containerStatuses":[{"name":"app","image":"registry.internal.corp/team/app:v1","imageID":"registry.internal.corp/team/app@sha256:abc"}]}}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		changed, err := rewriteImageRegistryInRecord(rec, noExclusions, upper)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("want changed=true")
		}
		var out map[string]interface{}
		if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
			t.Fatal(err)
		}
		specImage := out["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["image"]
		if got, want := specImage, "registry.internal.corp-ALIASED/team/app:v1"; got != want {
			t.Errorf("spec.containers[0].image = %v, want %v", got, want)
		}
		cs := out["status"].(map[string]interface{})["containerStatuses"].([]interface{})[0].(map[string]interface{})
		if got, want := cs["image"], "registry.internal.corp-ALIASED/team/app:v1"; got != want {
			t.Errorf("status.containerStatuses[0].image = %v, want %v", got, want)
		}
		if got, want := cs["imageID"], "registry.internal.corp-ALIASED/team/app@sha256:abc"; got != want {
			t.Errorf("status.containerStatuses[0].imageID = %v, want %v", got, want)
		}
	})

	// Deployment nests its PodTemplateSpec at spec.template.spec — this is
	// exactly what proves the generic recursive walk, not a hardcoded
	// per-Kind field path.
	t.Run("Deployment nested at spec.template.spec", func(t *testing.T) {
		body := `{"kind":"Deployment","spec":{"template":{"spec":{"containers":[{"name":"app","image":"registry.internal.corp/team/app:v1"}]}}}}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		changed, err := rewriteImageRegistryInRecord(rec, noExclusions, upper)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("want changed=true")
		}
		var out map[string]interface{}
		if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
			t.Fatal(err)
		}
		image := out["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["image"]
		if got, want := image, "registry.internal.corp-ALIASED/team/app:v1"; got != want {
			t.Errorf("nested image = %v, want %v", got, want)
		}
	})

	// CronJob nests one level deeper still: spec.jobTemplate.spec.template.spec.
	t.Run("CronJob nested at spec.jobTemplate.spec.template.spec", func(t *testing.T) {
		body := `{"kind":"CronJob","spec":{"jobTemplate":{"spec":{"template":{"spec":{
			"containers":[{"name":"app","image":"registry.internal.corp/team/app:v1"}]
		}}}}}}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		changed, err := rewriteImageRegistryInRecord(rec, noExclusions, upper)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("want changed=true")
		}
		var out map[string]interface{}
		if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
			t.Fatal(err)
		}
		spec := out["spec"].(map[string]interface{})
		podSpec := spec["jobTemplate"].(map[string]interface{})["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
		image := podSpec["containers"].([]interface{})[0].(map[string]interface{})["image"]
		if got, want := image, "registry.internal.corp-ALIASED/team/app:v1"; got != want {
			t.Errorf("deeply nested image = %v, want %v", got, want)
		}
	})

	t.Run("an implicit Docker Hub image with no registry is left untouched", func(t *testing.T) {
		body := `{"kind":"Pod","spec":{"containers":[{"name":"app","image":"nginx:1.21"}]}}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		orig := string(rec.ResponseBody)
		changed, err := rewriteImageRegistryInRecord(rec, noExclusions, upper)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Error("want changed=false — no explicit registry to anonymize")
		}
		if string(rec.ResponseBody) != orig {
			t.Error("body must be byte-identical when nothing was rewritten")
		}
	})

	t.Run("a record with no container list at all is left untouched", func(t *testing.T) {
		body := `{"kind":"Namespace","metadata":{"name":"prod"}}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		orig := string(rec.ResponseBody)
		changed, err := rewriteImageRegistryInRecord(rec, noExclusions, upper)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Error("want changed=false")
		}
		if string(rec.ResponseBody) != orig {
			t.Error("body must be byte-identical when nothing was rewritten")
		}
	})
}
