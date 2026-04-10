Output docs/demo/demo.gif

Require kshrk
Require kubectl

Set Shell "zsh"
Set FontSize 13
Set Width 1200
Set Height 700
Set Theme "Catppuccin Mocha"
Set Padding 20
Set TypingSpeed 40ms

# ── Capture cluster state ─────────────────────────────────────────────────────
Type "kshrk capture --config docs/demo/demo-capture.yaml"
Sleep 500ms
Enter
Sleep 7500ms

# ── Inspect the capture ────────────────────────────────────────────────────────
Type "kshrk inspect demo-capture.tar.gz"
Sleep 300ms
Enter
Sleep 3s

# ── Open the archive in the background ────────────────────────────────────────
Type "kshrk open demo-capture.tar.gz &"
Sleep 500ms
Enter
Sleep 3750ms

# ── Point kubectl at the mock server ──────────────────────────────────────────
Type "export KUBECONFIG=$(ls ~/.kube/k8shark-*.yaml | tail -1)"
Sleep 300ms
Enter
Sleep 750ms

# ── kubectl commands ──────────────────────────────────────────────────────────
Type "kubectl get nodes"
Sleep 300ms
Enter
Sleep 2250ms

Type "kubectl get pods -A"
Sleep 300ms
Enter
Sleep 2250ms

Type "kubectl get pods -n k8shark-test --show-labels"
Sleep 300ms
Enter
Sleep 2250ms

Type "kubectl get pods -n k8shark-test -o wide"
Sleep 300ms
Enter
Sleep 2250ms

Type "kubectl get deploy -n k8shark-test nginx -o json"
Sleep 300ms
Enter
Sleep 3s

Type "kubectl explain sts.spec.updateStrategy.type"
Sleep 300ms
Enter
Sleep 3s

Type "kubectl api-resources"
Sleep 300ms
Enter
Sleep 3s

Type "kubectl api-versions"
Sleep 300ms
Enter
Sleep 3s

# ── Cleanup ───────────────────────────────────────────────────────────────────
Type "kill %1"
Sleep 300ms
Enter
Sleep 1500ms
