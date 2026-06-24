package cmd

import "testing"

func TestParseRedactField(t *testing.T) {
	t.Run("three parts", func(t *testing.T) {
		rule, err := parseRedactField("metadata.name:Pod:REDACTED")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rule.FieldPath != "metadata.name" || rule.Kind != "Pod" || rule.Replacement != "REDACTED" {
			t.Errorf("got %+v", rule)
		}
		if rule.ValueType != "" {
			t.Errorf("ValueType = %q, want empty", rule.ValueType)
		}
	})

	t.Run("four parts with valueType", func(t *testing.T) {
		rule, err := parseRedactField("data.token:Secret:XXX:base64")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rule.ValueType != "base64" {
			t.Errorf("ValueType = %q, want base64", rule.ValueType)
		}
		if rule.Replacement != "XXX" {
			t.Errorf("Replacement = %q, want XXX", rule.Replacement)
		}
	})

	t.Run("valueType keeps everything after the 3rd colon", func(t *testing.T) {
		// SplitN(…, 4) caps the split at 4 fields, so any further colons stay in
		// the 4th field (valueType). The replacement is still just the 3rd field.
		rule, err := parseRedactField("f:Kind:a:b:c")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rule.Replacement != "a" || rule.ValueType != "b:c" {
			t.Errorf("got replacement=%q valueType=%q, want replacement=%q valueType=%q",
				rule.Replacement, rule.ValueType, "a", "b:c")
		}
	})

	t.Run("too few parts errors", func(t *testing.T) {
		if _, err := parseRedactField("metadata.name:Pod"); err == nil {
			t.Error("expected error for fewer than 3 parts")
		}
		if _, err := parseRedactField("justonefield"); err == nil {
			t.Error("expected error for a single field")
		}
	})
}
