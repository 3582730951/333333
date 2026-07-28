package api

import (
	"bytes"
	"context"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

type claudeAliasCapture struct{ bytes.Buffer }

func (c *claudeAliasCapture) Write(p []byte) (int, error) {
	n := len(p)
	if c.Len() < 256<<10 {
		remain := (256 << 10) - c.Len()
		if len(p) > remain {
			p = p[:remain]
		}
		_, _ = c.Buffer.Write(p)
	}
	return n, nil
}

func (s *Server) persistClaudeItemAliases(ctx context.Context, requestBody, responseBody []byte, lease scheduler.Lease, model string) {
	keys := append(routing.ClaudeItemAffinityKeys(requestBody), routing.ClaudeItemAffinityKeys(responseBody)...)
	seen := map[string]bool{}
	for _, key := range keys {
		if key.Hash == "" || seen[key.Hash] {
			continue
		}
		seen[key.Hash] = true
		_ = s.scheduler.UpsertAffinityBinding(ctx, storage.AffinityBinding{
			RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source,
			AccountID: lease.Account.ID, Provider: "claude", Model: model, EgressID: lease.Egress.ID,
		})
	}
}
