package scheduler

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestShardedLoadCountersAndEgressEWMA(t *testing.T) {
	s := New(nil, config.Default())
	egress := storage.EgressProfile{ID: "egress-a", TransportSidecarID: "sidecar-a"}
	var wg sync.WaitGroup
	for index := 0; index < 256; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			s.addLocalLoad(fmt.Sprintf("account-%d", index), egress, 1, 100)
		}(index)
	}
	wg.Wait()
	if active := s.loads.active(); active != 256 {
		t.Fatalf("active=%d want=256", active)
	}
	if got := s.currentEgressLoad(egress.ID); got != 256 {
		t.Fatalf("egress load=%d want=256", got)
	}
	if got := s.currentEgressLoad(egress.TransportSidecarID); got != 256 {
		t.Fatalf("sidecar load=%d want=256", got)
	}
	for index := 0; index < 256; index++ {
		s.addLocalLoad(fmt.Sprintf("account-%d", index), egress, -1, -100)
	}
	if active := s.loads.active(); active != 0 {
		t.Fatalf("active after release=%d want=0", active)
	}
	for index := range s.loads.shards {
		shard := &s.loads.shards[index]
		shard.mu.RLock()
		accounts, tokens := len(shard.inflight), len(shard.tokens)
		shard.mu.RUnlock()
		if accounts != 0 || tokens != 0 {
			t.Fatalf("released shard %d retained accounts=%d tokens=%d", index, accounts, tokens)
		}
	}

	s.ObserveEgress(egress.ID, 40*time.Millisecond, true)
	initialSuccess, initialLatency := s.egressEWMAQuality(egress.ID)
	s.ObserveEgress(egress.ID, 800*time.Millisecond, false)
	afterFailure, afterLatency := s.egressEWMAQuality(egress.ID)
	if afterFailure >= initialSuccess || afterLatency <= initialLatency {
		t.Fatalf("EWMA did not penalize slow failure: before=(%d,%d) after=(%d,%d)", initialSuccess, initialLatency, afterFailure, afterLatency)
	}
	for index := 0; index < schedulerLoadShardCount*schedulerEWMAEntriesPerShard*2; index++ {
		s.ObserveEgress(fmt.Sprintf("ephemeral-egress-%d", index), time.Millisecond, true)
	}
	if outlets := s.Metrics().EgressEWMAOutlets; outlets > schedulerLoadShardCount*schedulerEWMAEntriesPerShard {
		t.Fatalf("EWMA outlets=%d exceed bound=%d", outlets, schedulerLoadShardCount*schedulerEWMAEntriesPerShard)
	}
}
