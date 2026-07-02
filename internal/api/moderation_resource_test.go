package api

import (
	"strings"
	"testing"
)

func TestAdminModerationHandlesReloadErrors(t *testing.T) {
	source := readAPISource(t, "moderation.go")
	body := functionBody(t, source, "adminModeration")
	if strings.Contains(body, "out, _ := s.store.GetModerationConfig") {
		t.Fatal("adminModeration should handle moderation config reload errors")
	}
	if !strings.Contains(body, "out, err := s.store.GetModerationConfig") {
		t.Fatal("adminModeration should reload the persisted moderation config with error handling")
	}
}

func TestAdminModerationTranslateHandlesDefaultModelReadErrors(t *testing.T) {
	source := readAPISource(t, "moderation.go")
	body := functionBody(t, source, "adminModerationTranslate")
	if strings.Contains(body, "cfg, _ := s.store.GetModerationConfig") {
		t.Fatal("adminModerationTranslate should handle default model read errors")
	}
	if !strings.Contains(body, "cfg, err := s.store.GetModerationConfig") {
		t.Fatal("adminModerationTranslate should read the default moderation model with error handling")
	}
}
