package api

import (
	"context"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
)

func (s *Server) enterKiroCacheSingleflight(
	_ context.Context,
	_ []byte,
	_ routing.AffinityKey,
	_ scheduler.Lease,
	_ string,
	_ int,
) (func(), bool) {
	// Kiro cache points remain enabled, but they must never become proxy-side
	// pacing. In particular, two independent downstream calls with the same
	// stable prefix are both dispatched immediately instead of serializing for up
	// to cacheSingleflightMaxWait. This preserves downstream concurrency and
	// leaves upstream admission entirely to Kiro.
	return func() {}, false
}
