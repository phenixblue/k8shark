package cmd

import (
	"reflect"
	"strings"
	"testing"

	"github.com/phenixblue/k8shark/internal/config"
	"github.com/spf13/cobra"
)

// newResolveRedactionCmd builds a bare *cobra.Command carrying exactly the
// three flags resolveRedaction reads, mirroring captureCmd's registration
// (cmd/capture.go's init) without sharing captureCmd's global flag state
// across test cases.
func newResolveRedactionCmd(t *testing.T, redactSecrets bool, allowSecret, redactField []string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().Bool("redact-secrets", false, "")
	cmd.Flags().StringArray("allow-secret", nil, "")
	cmd.Flags().StringArray("redact-field", nil, "")
	if redactSecrets {
		if err := cmd.Flags().Set("redact-secrets", "true"); err != nil {
			t.Fatalf("setting redact-secrets: %v", err)
		}
	}
	for _, a := range allowSecret {
		if err := cmd.Flags().Set("allow-secret", a); err != nil {
			t.Fatalf("setting allow-secret=%q: %v", a, err)
		}
	}
	for _, f := range redactField {
		if err := cmd.Flags().Set("redact-field", f); err != nil {
			t.Fatalf("setting redact-field=%q: %v", f, err)
		}
	}
	return cmd
}

func TestResolveRedaction(t *testing.T) {
	cases := []struct {
		name              string
		flagRedactSecrets bool
		flagAllowSecret   []string
		flagRedactField   []string
		cfgRedaction      config.RedactionConfig
		wantDoRedact      bool
		wantAllowList     map[string]bool
		wantFieldRules    []config.RedactionRule
	}{
		{
			name:          "neither flag nor config set",
			wantDoRedact:  false,
			wantAllowList: map[string]bool{},
		},
		{
			name:              "flag alone enables redaction",
			flagRedactSecrets: true,
			wantDoRedact:      true,
			wantAllowList:     map[string]bool{},
		},
		{
			name:          "config alone enables redaction",
			cfgRedaction:  config.RedactionConfig{RedactSecrets: true},
			wantDoRedact:  true,
			wantAllowList: map[string]bool{},
		},
		{
			// Documents the asymmetry called out in #253: cfg.RedactSecrets
			// can only turn redaction ON, never off. An explicit
			// --redact-secrets=false against a config that enables it does
			// nothing — this is the fail-safe direction (you can't
			// accidentally disable redaction the config owner intended),
			// so it's asserted here as intentional, not a bug.
			name:              "config wins over an explicit flag=false (fail-safe direction)",
			flagRedactSecrets: false,
			cfgRedaction:      config.RedactionConfig{RedactSecrets: true},
			wantDoRedact:      true,
			wantAllowList:     map[string]bool{},
		},
		{
			name:            "allowlist from flag only",
			flagAllowSecret: []string{"default/a", "default/b"},
			wantDoRedact:    false,
			wantAllowList:   map[string]bool{"default/a": true, "default/b": true},
		},
		{
			name:          "allowlist from config only",
			cfgRedaction:  config.RedactionConfig{AllowSecrets: []string{"kube-system/c"}},
			wantDoRedact:  false,
			wantAllowList: map[string]bool{"kube-system/c": true},
		},
		{
			name:            "allowlist merges flag and config",
			flagAllowSecret: []string{"default/a"},
			cfgRedaction:    config.RedactionConfig{AllowSecrets: []string{"kube-system/c"}},
			wantDoRedact:    false,
			wantAllowList:   map[string]bool{"default/a": true, "kube-system/c": true},
		},
		{
			name: "field rules from config only",
			cfgRedaction: config.RedactionConfig{
				Rules: []config.RedactionRule{{FieldPath: "data.token", Kind: "Secret", Replacement: "REDACTED"}},
			},
			wantDoRedact:  false,
			wantAllowList: map[string]bool{},
			wantFieldRules: []config.RedactionRule{
				{FieldPath: "data.token", Kind: "Secret", Replacement: "REDACTED"},
			},
		},
		{
			name:            "field rules from --redact-field flag only",
			flagRedactField: []string{"data.api-key:ConfigMap:REDACTED"},
			wantDoRedact:    false,
			wantAllowList:   map[string]bool{},
			wantFieldRules: []config.RedactionRule{
				{FieldPath: "data.api-key", Kind: "ConfigMap", Replacement: "REDACTED"},
			},
		},
		{
			name:            "field rules from --redact-field flag with valueType",
			flagRedactField: []string{"spec.replicas:Deployment:0:integer"},
			wantDoRedact:    false,
			wantAllowList:   map[string]bool{},
			wantFieldRules: []config.RedactionRule{
				{FieldPath: "spec.replicas", Kind: "Deployment", Replacement: "0", ValueType: "integer"},
			},
		},
		{
			name:            "field rules from config and flag both — config rules first, then flag rules",
			flagRedactField: []string{"data.api-key:ConfigMap:REDACTED"},
			cfgRedaction: config.RedactionConfig{
				Rules: []config.RedactionRule{{FieldPath: "data.token", Kind: "Secret", Replacement: "REDACTED"}},
			},
			wantDoRedact:  false,
			wantAllowList: map[string]bool{},
			wantFieldRules: []config.RedactionRule{
				{FieldPath: "data.token", Kind: "Secret", Replacement: "REDACTED"},
				{FieldPath: "data.api-key", Kind: "ConfigMap", Replacement: "REDACTED"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newResolveRedactionCmd(t, tc.flagRedactSecrets, tc.flagAllowSecret, tc.flagRedactField)
			cfg := &config.Config{Redaction: tc.cfgRedaction}

			doRedact, allowList, fieldRules, err := resolveRedaction(cmd, cfg)
			if err != nil {
				t.Fatalf("resolveRedaction: unexpected error: %v", err)
			}
			if doRedact != tc.wantDoRedact {
				t.Errorf("doRedactSecrets = %v, want %v", doRedact, tc.wantDoRedact)
			}
			if !reflect.DeepEqual(allowList, tc.wantAllowList) {
				t.Errorf("allowList = %v, want %v", allowList, tc.wantAllowList)
			}
			if len(fieldRules) == 0 && len(tc.wantFieldRules) == 0 {
				return // both nil/empty; reflect.DeepEqual(nil, []T{}) is false, so skip
			}
			if !reflect.DeepEqual(fieldRules, tc.wantFieldRules) {
				t.Errorf("fieldRules = %+v, want %+v", fieldRules, tc.wantFieldRules)
			}
		})
	}
}

// TestResolveRedaction_MalformedRedactFieldErrors guards the case #253 calls
// out: a malformed --redact-field must return an error naming the flag, not
// silently drop the rule and continue.
func TestResolveRedaction_MalformedRedactFieldErrors(t *testing.T) {
	cmd := newResolveRedactionCmd(t, false, nil, []string{"not-enough-parts"})
	cfg := &config.Config{}

	doRedact, allowList, fieldRules, err := resolveRedaction(cmd, cfg)
	if err == nil {
		t.Fatal("expected an error for a malformed --redact-field value")
	}
	if !strings.Contains(err.Error(), "--redact-field") {
		t.Errorf("error = %q, want it to name --redact-field", err.Error())
	}
	if doRedact || allowList != nil || fieldRules != nil {
		t.Errorf("expected zero-valued returns alongside the error, got doRedact=%v allowList=%v fieldRules=%v",
			doRedact, allowList, fieldRules)
	}
}
