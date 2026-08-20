package api

import "time"

type codexCachePolicySnapshot struct {
	capable    bool
	profitable bool
	expiresAt  time.Time
}
