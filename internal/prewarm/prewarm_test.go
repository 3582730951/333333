package prewarm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParallelRecoversTaskPanic(t *testing.T) {
	err := Parallel(context.Background(), func(context.Context) error {
		panic("warmup exploded")
	})
	if err == nil {
		t.Fatal("Parallel panic task returned nil error")
	}
	if !strings.Contains(err.Error(), "prewarm task panic: warmup exploded") {
		t.Fatalf("Parallel panic error = %q", err)
	}
}

func TestParallelAcceptsNilContext(t *testing.T) {
	called := false
	if err := Parallel(nil, func(ctx context.Context) error {
		called = true
		if ctx == nil {
			t.Fatal("task received nil context")
		}
		return nil
	}); err != nil {
		t.Fatalf("Parallel nil context: %v", err)
	}
	if !called {
		t.Fatal("Parallel did not run task")
	}
}

func TestDatabaseRejectsNilStore(t *testing.T) {
	if err := Database(context.Background(), nil); !errors.Is(err, errNilStore) {
		t.Fatalf("Database(nil) = %v, want %v", err, errNilStore)
	}
}

func TestCacheRejectsNilStore(t *testing.T) {
	if err := Cache(context.Background(), nil); !errors.Is(err, errNilStore) {
		t.Fatalf("Cache(nil) = %v, want %v", err, errNilStore)
	}
}
