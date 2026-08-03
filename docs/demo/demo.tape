Output docs/demo/demo.gif

Require kshrk
Require kubectl

Set Shell "zsh"
# Geometry trades off against legibility, so both are chosen deliberately.
#
# The gif is displayed scaled to the page's container width, which means apparent
# text size is FontSize x (container / Width) — more columns makes the text
# *smaller* on the page. So the goal is the narrowest canvas that fits the widest
# line without wrapping, not the widest canvas.
#
# At FontSize 16 a character is ~10.1px wide, plus 40px of padding. Measured
# widest line of every command below: `kubectl get pods -o wide` at 134 chars,
# then `kubectl api-resources` at 131 and the longest typed command at 107. So
# 1440px (~138 columns) fits them all with margin, and renders ~13.3px on a
# 1200px-wide container — still above the 13px/1200px this replaced.
#
# `kubectl get deploy -o json` reaches 449 characters and wraps at any sane
# width; that's inherent to JSON in a terminal and is what a user really sees.
#
# `--show-labels` was dropped from the pods command deliberately: its
# statefulset labels reach 191 characters, needing ~1970px, which would have cut
# apparent text to ~9.6px — worse than before. `-L app` shows the label that
# matters as its own column and stays on one line.
Set FontSize 16
Set Width 1440
Set Height 860
Set Theme "Catppuccin Mocha"
Set Padding 20
Set TypingSpeed 40ms

# ── Capture cluster state ─────────────────────────────────────────────────────
Type "kshrk capture --config docs/demo/demo-capture.yaml"
Sleep 500ms
Enter
Sleep 7500ms

# ── Inspect the capture ────────────────────────────────────────────────────────
Type "kshrk inspect demo-capture.kshrk"
Sleep 300ms
Enter
Sleep 3s

# ── Open the archive in the background ────────────────────────────────────────
Type "kshrk open demo-capture.kshrk --kubeconfig-out /tmp/kshrk-demo.kubeconfig &"
Sleep 500ms
Enter
Sleep 3750ms

# ── Point kubectl at the mock server ──────────────────────────────────────────
# Wait for the backgrounded `kshrk open` to write its kubeconfig before
# exporting it, so kubectl never races the server on slower machines.
Type "until [ -s /tmp/kshrk-demo.kubeconfig ]; do sleep 0.1; done; export KUBECONFIG=/tmp/kshrk-demo.kubeconfig"
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

Type "kubectl get pods -n k8shark-test -L app"
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
