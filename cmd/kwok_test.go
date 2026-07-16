package cmd

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestStartKwok_NotInstalled: with no kwok on PATH, startKwok fails with an
// actionable install hint rather than launching anything.
func TestStartKwok_NotInstalled(t *testing.T) {
	t.Setenv("PATH", "") // ensure exec.LookPath("kwok") fails
	KwokStages = []byte("dummy")

	cleanup, err := startKwok("/tmp/does-not-matter.yaml")
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("expected an error when kwok is not on PATH")
	}
	if !strings.Contains(err.Error(), "not found in PATH") {
		t.Errorf("error = %q, want an install hint mentioning PATH", err.Error())
	}
}

// TestWaitForNodesReady_PollsUntilNonEmpty: the fake server reports an empty
// node list for the first two requests (simulating the replay clock not yet
// having caught up to the first captured nodes snapshot) and a populated one
// thereafter. waitForNodesReady must keep polling instead of returning on the
// first (empty) response.
func TestWaitForNodesReady_PollsUntilNonEmpty(t *testing.T) {
	const emptyResponses = 2
	empty := `{"apiVersion":"v1","kind":"NodeList","items":[]}`
	nonEmpty := `{"apiVersion":"v1","kind":"NodeList","items":[{"metadata":{"name":"n1"}}]}`

	var reqCount int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&reqCount, 1) <= emptyResponses {
			fmt.Fprint(w, empty)
			return
		}
		fmt.Fprint(w, nonEmpty)
	}))
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if nodesReady(client, srv.URL) {
		t.Fatal("expected not ready while /api/v1/nodes reports an empty list")
	}

	done := make(chan struct{})
	go func() {
		waitForNodesReady(srv.URL, 2*time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForNodesReady did not return after the node list became non-empty")
	}
	if got := atomic.LoadInt32(&reqCount); got <= emptyResponses {
		t.Fatalf("expected more than %d requests (poll retried until non-empty), got %d", emptyResponses, got)
	}
}

// TestWaitForNodesReady_TimesOutWhenAlwaysEmpty: a capture that genuinely has
// no nodes and no synthesized overlay node (not reachable via --with-kwok in
// practice, since that always implies the scheduling shim, but the timeout
// itself must not hang indefinitely) returns once the deadline passes rather
// than blocking forever.
func TestWaitForNodesReady_TimesOutWhenAlwaysEmpty(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"NodeList","items":[]}`)
	}))
	defer srv.Close()

	const timeout = 150 * time.Millisecond
	start := time.Now()
	waitForNodesReady(srv.URL, timeout)
	if elapsed := time.Since(start); elapsed < timeout {
		t.Fatalf("returned after %s, before the %s timeout elapsed", elapsed, timeout)
	}
}

// TestValidateKwokFlags: --with-kwok with the scheduling shim disabled is
// rejected (it would leave pods unscheduled and never reach Running).
func TestValidateKwokFlags(t *testing.T) {
	cases := []struct {
		withKwok, schedulePods, wantErr bool
	}{
		{false, true, false},  // no kwok, shim on
		{false, false, false}, // no kwok, shim off — fine
		{true, true, false},   // kwok + shim on — the supported combo
		{true, false, true},   // kwok + shim off — contradictory, rejected
	}
	for _, c := range cases {
		err := validateKwokFlags(c.withKwok, c.schedulePods)
		if (err != nil) != c.wantErr {
			t.Errorf("validateKwokFlags(withKwok=%v, schedulePods=%v) err=%v, wantErr=%v",
				c.withKwok, c.schedulePods, err, c.wantErr)
		}
	}
}
