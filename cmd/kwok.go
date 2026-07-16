package cmd

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"
)

// KwokStages holds the bundled KWOK Stages config (set from main via go:embed).
// It is written to a temp file and passed to `kwok --config` by --with-kwok.
var KwokStages []byte

// validateKwokFlags rejects flag combinations that would make --with-kwok fail
// silently. KWOK only advances pods that are bound to a node, so the scheduling
// shim must be on; --schedule-pods=false with --with-kwok would leave pods
// unscheduled and the Pending→Running loop would never fire.
func validateKwokFlags(withKwok, schedulePods bool) error {
	if withKwok && !schedulePods {
		return fmt.Errorf("--with-kwok requires the pod-scheduling shim; remove --schedule-pods=false " +
			"(KWOK only runs pods that are bound to a node)")
	}
	return nil
}

// nodesReadyTimeout bounds how long waitForNodesReady polls before giving up
// and letting kwok start anyway.
const nodesReadyTimeout = 5 * time.Second

// nodesReadyReqTimeout bounds a single readiness request, so one slow/hung
// response can't itself blow past waitForNodesReady's overall timeout.
const nodesReadyReqTimeout = 2 * time.Second

// waitForNodesReady polls addr's /api/v1/nodes until it reports at least one
// node, or timeout elapses. In replay mode the as-of clock starts advancing
// the instant the mock server comes up, but a resource's first captured
// snapshot can land a moment after the clock's nominal window start
// (goroutine scheduling and network round-trip jitter around capture start);
// until the clock catches up, /api/v1/nodes briefly reports an empty list.
// kwok's node informer LISTs exactly once at startup — if that race loses,
// its cache stays empty for the whole session and pods it manages never get
// scheduled to Running. Waiting here (rather than in kwok itself) keeps the
// launched kwok binary unmodified.
//
// certPEM pins the mock server's own self-signed cert (Server.CertPEM())
// rather than disabling certificate verification: this client talks straight
// to an HTTPS listener we just spawned in this process, so the real
// certificate is on hand and there's no reason to skip validating it.
func waitForNodesReady(addr string, certPEM []byte, timeout time.Duration) {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		reqTimeout := remaining
		if reqTimeout > nodesReadyReqTimeout {
			reqTimeout = nodesReadyReqTimeout
		}
		client := &http.Client{Timeout: reqTimeout, Transport: transport}
		if nodesReady(client, addr) {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// nodesReady reports whether a GET of addr's /api/v1/nodes returns at least
// one node. Any error, non-200, or empty items list counts as not ready.
func nodesReady(client *http.Client, addr string) bool {
	resp, err := client.Get(addr + "/api/v1/nodes")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return false
	}
	return len(list.Items) > 0
}

// startKwok launches a detected `kwok` binary against the mock server's
// kubeconfig, managing all nodes and using the bundled Stages. It returns a
// cleanup func that stops kwok and removes the temp stages file. An error is
// returned (with an install hint) when the kwok binary isn't on PATH.
func startKwok(kubeconfigPath string) (cleanup func(), err error) {
	kwokBin, lookErr := exec.LookPath("kwok")
	if lookErr != nil {
		return nil, fmt.Errorf("--with-kwok: 'kwok' not found in PATH; install it from " +
			"https://kwok.sigs.k8s.io/docs/user/install/ (or drop --with-kwok and run kwok yourself)")
	}
	if len(KwokStages) == 0 {
		return nil, fmt.Errorf("--with-kwok: bundled KWOK stages are unavailable in this build")
	}

	tmp, err := os.CreateTemp("", "kshrk-kwok-stages-*.yaml")
	if err != nil {
		return nil, fmt.Errorf("--with-kwok: writing stages: %w", err)
	}
	if _, err := tmp.Write(KwokStages); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("--with-kwok: writing stages: %w", err)
	}
	// Check Close: it flushes the write, so a failure here means the stages file
	// may be incomplete — which would surface later as a confusing kwok parse error.
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("--with-kwok: writing stages: %w", err)
	}

	kc := exec.Command(kwokBin, "--kubeconfig", kubeconfigPath, "--manage-all-nodes", "--config", tmp.Name())
	kc.Stdout = os.Stdout
	kc.Stderr = os.Stderr
	if err := kc.Start(); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("--with-kwok: starting kwok: %w", err)
	}

	cleanup = func() {
		if kc.Process != nil {
			_ = kc.Process.Kill()
		}
		_ = kc.Wait()
		_ = os.Remove(tmp.Name())
	}
	return cleanup, nil
}
