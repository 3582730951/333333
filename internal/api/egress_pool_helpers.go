package api

import (
	"codex-account-pool/internal/storage"
	"context"
	"fmt"
	"strings"
)

func isRegistrationEgressPoolPurpose(purpose string) bool {
	purpose = strings.TrimSpace(purpose)
	return purpose == "" || purpose == "registration"
}

func normalizeRegistrationEgressPool(p storage.EgressPool) storage.EgressPool {
	if strings.TrimSpace(p.Purpose) == "" {
		p.Purpose = "registration"
	}
	return p
}

func getRegistrationEgressPoolFromStore(ctx context.Context, store *storage.Store, poolID string) (storage.EgressPool, error) {
	poolID = strings.TrimSpace(poolID)
	if poolID == "" {
		return storage.EgressPool{}, fmt.Errorf("registration egress pool id required")
	}
	pool, err := store.GetEgressPool(ctx, poolID)
	if err != nil {
		return storage.EgressPool{}, fmt.Errorf("egress pool %q not found", poolID)
	}
	if !isRegistrationEgressPoolPurpose(pool.Purpose) {
		return storage.EgressPool{}, fmt.Errorf("egress pool %q is purpose %q, want registration", poolID, pool.Purpose)
	}
	return normalizeRegistrationEgressPool(pool), nil
}

func (s *Server) getRegistrationEgressPool(ctx context.Context, poolID string) (storage.EgressPool, error) {
	return getRegistrationEgressPoolFromStore(ctx, s.store, poolID)
}

func (h *Handler) getRegistrationEgressPool(ctx context.Context, poolID string) (storage.EgressPool, error) {
	return getRegistrationEgressPoolFromStore(ctx, h.store, poolID)
}
