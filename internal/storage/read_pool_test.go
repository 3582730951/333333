package storage

import "testing"

func TestReadPoolSizeMemoryTiers(t *testing.T) {
	const gib = uint64(1024 * 1024 * 1024)
	for _, tc := range []struct {
		memory uint64
		want   int
	}{{512 * 1024 * 1024, 2}, {2 * gib, 4}, {7 * gib, 4}, {8 * gib, 8}, {0, 8}} {
		if got := readPoolSizeForMemory(tc.memory); got != tc.want {
			t.Fatalf("memory=%d size=%d, want %d", tc.memory, got, tc.want)
		}
	}
}

func TestReadPoolSizeEnvironmentOverride(t *testing.T) {
	t.Setenv("CODEX_POOL_DB_MAX_READ_CONNS", "3")
	if got := readPoolSize(); got != 3 {
		t.Fatalf("override size=%d", got)
	}
}
