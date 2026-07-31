package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/phenixblue/k8shark/internal/k8sbin"
)

// controllerManagerControllers is the curated set of kube-controller-manager
// controllers --with-controller-manager enables: pure API-object reconcilers
// that only need the writable overlay's CRUD+watch surface, not a real
// kubelet, storage provisioner, cloud provider, or node lifecycle. This
// deliberately excludes controllers like node-lifecycle, persistentvolume
// (binding needs a real provisioner), and certificate-signing — see
// docs/kwok.md's non-goals, which apply here too.
var controllerManagerControllers = []string{
	"namespace",
	"serviceaccount",
	"resourcequota",
	"garbagecollector",
	"daemonset",
	"deployment",
	"replicaset",
	"statefulset",
	"job",
	"cronjob",
	"endpoint",
	"endpointslice",
	"endpointslicemirroring",
	"disruption",
}

// controllerManagerFlagHelp is the --with-controller-manager flag description
// shared by `replay` and `ui`, derived from controllerManagerControllers
// rather than duplicated as a hand-written list in each cmd/*.go — the two
// drifted out of sync with the actual set (and each other) when the list was
// last extended.
var controllerManagerFlagHelp = "also run kube-controller-manager (downloaded/built to match the capture's " +
	"Kubernetes version) against the server, reconciling a curated set of controllers (" +
	strings.Join(controllerManagerControllers, ", ") + ") — see docs/kwok.md (implies --writable)"

// controllerLogFlagHelp is the --controller-log description, shared by `replay`
// and `ui` for the same reason controllerManagerFlagHelp is.
var controllerLogFlagHelp = "destination for kube-controller-manager's own output when " +
	"--with-controller-manager is set: a file path, or \"-\" to stream it inline " +
	"(default: a temp file whose path is printed at startup)"

// startControllerManager locates or builds a kube-controller-manager binary
// matching k8sVersion (see internal/k8sbin) and runs it against the mock
// server's kubeconfig with the curated controller set, no leader election (a
// single local process needs none), and no delegated authn/authz (there's no
// real TokenReview/SubjectAccessReview API for it to call — it falls back to
// always-allow, same as any other out-of-cluster test harness). It returns a
// cleanup func that stops the subprocess.
// logDest resolves where kube-controller-manager's own output goes.
//
// It used to be wired straight to kshrk's stdout/stderr, which drowned
// everything else: a one-minute replay produced 3,000 lines, 5 MB, of which 84%
// was one controller retrying — interleaved with replay's progress line. The
// output is diagnostic, so the default is a file whose path is printed once;
// silently discarding it would make a misbehaving controller impossible to
// debug. "-" restores the old inline streaming.
func resolveControllerLog(dest string) (w io.Writer, closeFn func(), shown string, err error) {
	if dest == "-" {
		return os.Stderr, func() {}, "stderr", nil
	}
	if dest == "" {
		f, ferr := os.CreateTemp("", "kshrk-controller-manager-*.log")
		if ferr != nil {
			return nil, nil, "", fmt.Errorf("creating controller log file: %w", ferr)
		}
		return f, func() { _ = f.Close() }, f.Name(), nil
	}
	f, ferr := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if ferr != nil {
		return nil, nil, "", fmt.Errorf("opening controller log %q: %w", dest, ferr)
	}
	return f, func() { _ = f.Close() }, dest, nil
}

func startControllerManager(kubeconfigPath, k8sVersion, controllerLog string) (cleanup func(), err error) {
	binPath, err := k8sbin.EnsureControllerManager(k8sVersion, func(msg string) {
		fmt.Fprintf(os.Stderr, "--with-controller-manager: %s\n", msg)
	})
	if err != nil {
		return nil, fmt.Errorf("--with-controller-manager: %w", err)
	}

	args := []string{
		"--kubeconfig", kubeconfigPath,
		"--leader-elect=false",
		"--use-service-account-credentials=false",
		"--controllers=" + strings.Join(controllerManagerControllers, ","),
	}
	logW, closeLog, logShown, err := resolveControllerLog(controllerLog)
	if err != nil {
		return nil, fmt.Errorf("--with-controller-manager: %w", err)
	}

	c := exec.Command(binPath, args...)
	c.Stdout = logW
	c.Stderr = logW
	if err := c.Start(); err != nil {
		closeLog()
		return nil, fmt.Errorf("--with-controller-manager: starting kube-controller-manager: %w", err)
	}
	fmt.Fprintf(os.Stderr, "--with-controller-manager: controller output -> %s\n", logShown)

	cleanup = func() {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
		_ = c.Wait()
		closeLog()
	}
	return cleanup, nil
}
