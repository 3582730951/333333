package scheduler

import (
	"fmt"
	"testing"

	"codex-account-pool/internal/storage"
)

func BenchmarkCandidateSelectionScale(b *testing.B) {
	for _, size := range []int{10, 1000, 10000} {
		pool := make([]candidate, size)
		for i := range pool {
			pool[i].account = storage.Account{ID: fmt.Sprintf("account-%05d", i)}
			pool[i].score = float64(i % 7)
		}
		b.Run(fmt.Sprintf("power_two/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				var sample []candidate
				rng := uint64(n + 1)
				for i := range pool {
					sample = addPowerTwoCandidate(sample, pool[i], uint64(i+1), &rng)
				}
				if len(sample) != 2 {
					b.Fatal("invalid sample")
				}
			}
		})
		b.Run(fmt.Sprintf("affinity_top3/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				var top []rankedCandidate
				for i := range pool {
					top = insertRendezvousTop3(top, rankedCandidate{candidate: pool[i], rank: rendezvous("benchmark-affinity", pool[i].account.ID)})
				}
				if len(top) != 3 {
					b.Fatal("invalid top three")
				}
			}
		})
	}
}
