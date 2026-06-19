package v2

import "testing"

func TestClassifyPod_HealthyPod(t *testing.T) {
	body := []byte(`{
		"status": {
			"phase": "Running",
			"containerStatuses": [
				{"name": "app", "image": "img", "ready": true, "restartCount": 0,
				 "state": {"running": {}}}
			]
		}
	}`)
	ph := ClassifyPod(body)
	if !ph.IsHealthy() {
		t.Fatalf("expected healthy pod, got issues %v", ph.Issues)
	}
	if ph.Ready != 1 || ph.Total != 1 {
		t.Errorf("ready/total = %d/%d, want 1/1", ph.Ready, ph.Total)
	}
}

func TestClassifyPod_CrashLoopBackOff(t *testing.T) {
	body := []byte(`{
		"status": {
			"phase": "Running",
			"containerStatuses": [
				{"name": "app", "ready": false, "restartCount": 17,
				 "state": {"waiting": {"reason": "CrashLoopBackOff", "message": "back-off 5m0s"}},
				 "lastState": {"terminated": {"reason": "OOMKilled", "exitCode": 137}}}
			]
		}
	}`)
	ph := ClassifyPod(body)
	if ph.IsHealthy() {
		t.Fatalf("expected unhealthy pod")
	}
	found := false
	for _, x := range ph.Issues {
		if x == "CrashLoopBackOff" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected CrashLoopBackOff in issues, got %v", ph.Issues)
	}
	if ph.Restarts != 17 {
		t.Errorf("Restarts = %d, want 17", ph.Restarts)
	}
}

func TestClassifyPod_InitContainerSuccess(t *testing.T) {
	// Init container that exited 0 is not an issue.
	body := []byte(`{
		"status": {
			"phase": "Running",
			"initContainerStatuses": [
				{"name": "init", "ready": true, "restartCount": 0,
				 "state": {"terminated": {"reason": "Completed", "exitCode": 0}}}
			],
			"containerStatuses": [
				{"name": "app", "ready": true, "restartCount": 0,
				 "state": {"running": {}}}
			]
		}
	}`)
	ph := ClassifyPod(body)
	if !ph.IsHealthy() {
		t.Errorf("expected healthy, got %v", ph.Issues)
	}
}

func TestPodNamePrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"kubevirt-migration-controller-6c77fbf565-2rz8h", "kubevirt-migration-controller-*"},
		{"nginx-deployment-abc123-xyz45", "nginx-deployment-*"},
		{"single", "single-*"},
		{"my-pod-static", "my-pod-*"},
	}
	for _, c := range cases {
		got := PodNamePrefix(c.in)
		if got != c.want {
			t.Errorf("PodNamePrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
