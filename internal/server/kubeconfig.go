package server

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

var kubeconfigTmpl = template.Must(template.New("kc").Parse(`apiVersion: v1
kind: Config
preferences: {}
clusters:
- cluster:
    server: {{.ServerAddr}}
    certificate-authority-data: {{.CACertB64}}
  name: k8shark
contexts:
- context:
    cluster: k8shark
    user: k8shark
  name: k8shark
current-context: k8shark
users:
- name: k8shark
  user: {}
`))

type kubeconfigData struct {
	ServerAddr string
	CACertB64  string
}

// writeKubeconfig writes a kubeconfig that points kubectl at the mock server.
// certPEM is embedded as the certificate-authority-data so kubectl validates
// the self-signed TLS cert without --insecure-skip-tls-verify.
func writeKubeconfig(serverAddr string, certPEM []byte, outputPath string) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o750); err != nil {
		return fmt.Errorf("creating kubeconfig dir: %w", err)
	}
	f, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("creating kubeconfig: %w", err)
	}
	defer f.Close()
	return kubeconfigTmpl.Execute(f, kubeconfigData{
		ServerAddr: serverAddr,
		CACertB64:  base64.StdEncoding.EncodeToString(certPEM),
	})
}
