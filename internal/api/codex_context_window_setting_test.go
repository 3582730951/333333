package api

import "testing"

func TestCodexInstallContextWindowValidation(t *testing.T) {
	var field configField
	for _, candidate := range configFields() {
		if candidate.Key == "codex_install_context_window" {
			field = candidate
			break
		}
	}
	if field.Key == "" {
		t.Fatal("codex_install_context_window field is not registered")
	}
	for _, valid := range []any{float64(0), float64(16384), float64(262144), float64(4194304)} {
		if _, err := validateSettingValue(field, valid); err != nil {
			t.Fatalf("valid value %v rejected: %v", valid, err)
		}
	}
	for _, invalid := range []any{float64(-1), float64(16383), float64(4194305), 1.5, "huge"} {
		if _, err := validateSettingValue(field, invalid); err == nil {
			t.Fatalf("invalid value %v accepted", invalid)
		}
	}
}
