package cmd

import (
	"fmt"
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

// startControllerManager locates or builds a kube-controller-manager binary
// matching k8sVersion (see internal/k8sbin) and runs it against the mock
// server's kubeconfig with the curated controller set, no leader election (a
// single local process needs none), and no delegated authn/authz (there's no
// real TokenReview/SubjectAccessReview API for it to call — it falls back to
// always-allow, same as any other out-of-cluster test harness). It returns a
// cleanup func that stops the subprocess.
func startControllerManager(kubeconfigPath, k8sVersion string) (cleanup func(), err error) {
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
	c := exec.Command(binPath, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("--with-controller-manager: starting kube-controller-manager: %w", err)
	}

	cleanup = func() {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
		_ = c.Wait()
	}
	return cleanup, nil
}
