package server

import (
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

// kubeconfigTmpl generates a kubeconfig for the local mock server.
//
// insecure-skip-tls-verify is appropriate here: the server is a short-lived
// process on 127.0.0.1 whose TLS cert is generated fresh each run. There is
// no meaningful security boundary to protect.
//
// A static bearer token is set so kubectl has an explicit credential mechanism
// and never falls back to prompting for a username/password.
// The mock server ignores all Authorization headers.
var kubeconfigTmpl = template.Must(template.New("kc").Parse(`apiVersion: v1
kind: Config
preferences: {}
clusters:
- cluster:
    server: {{.ServerAddr}}
    insecure-skip-tls-verify: true
  name: k8shark
contexts:
- context:
    cluster: k8shark
    user: k8shark
  name: k8shark
current-context: k8shark
users:
- name: k8shark
  user:
    token: k8shark-replay
`))

type kubeconfigData struct {
	ServerAddr string
}

// writeKubeconfig writes a kubeconfig pointing kubectl at the mock server.
func writeKubeconfig(serverAddr string, outputPath string) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o750); err != nil {
		return fmt.Errorf("creating kubeconfig dir: %w", err)
	}
	f, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("creating kubeconfig: %w", err)
	}
	defer f.Close()
	return kubeconfigTmpl.Execute(f, kubeconfigData{ServerAddr: serverAddr})
}
