package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRequiresExplicitTargetReplacement(t *testing.T) {
	t.Setenv("CODEX_POOL_POSTGRES_DSN", "postgres://secret@example.invalid/db")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-sqlite", "missing.sqlite3"}, &stdout, &stderr); code != 2 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "-replace-target is required") || strings.Contains(stderr.String(), "secret") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunRejectsNonPositiveTimeout(t *testing.T) {
	t.Setenv("CODEX_POOL_POSTGRES_DSN", "postgres://example.invalid/db")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-sqlite", "missing.sqlite3", "-replace-target", "-timeout=0"}, &stdout, &stderr); code != 2 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "-timeout must be positive") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}
