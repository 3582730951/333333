package storage

import (
	"fmt"
	"testing"
)

func TestAccountBackupIDBatchesBoundFiftyThousandIDs(t *testing.T) {
	ids := make([]string, 0, 50_003)
	for index := 0; index < 50_000; index++ {
		ids = append(ids, fmt.Sprintf("account-%05d", index))
	}
	ids = append(ids, "", " account-00000 ", "account-49999")

	batches := accountBackupIDBatches(ids)
	if len(batches) != 100 {
		t.Fatalf("batch count = %d, want 100", len(batches))
	}
	total := 0
	for index, batch := range batches {
		if len(batch) == 0 || len(batch) > accountBackupQueryBatchSize {
			t.Fatalf("batch %d size = %d, want 1..%d", index, len(batch), accountBackupQueryBatchSize)
		}
		total += len(batch)
	}
	if total != 50_000 {
		t.Fatalf("unique batched IDs = %d, want 50000", total)
	}
}
