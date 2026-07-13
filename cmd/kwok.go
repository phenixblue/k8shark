package cmd

import (
	"fmt"
	"os"
	"os/exec"
)

// KwokStages holds the bundled KWOK Stages config (set from main via go:embed).
// It is written to a temp file and passed to `kwok --config` by --with-kwok.
var KwokStages []byte

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
	_ = tmp.Close()

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
