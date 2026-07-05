package api

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"codex-account-pool/internal/storage"
)

func requestedImportEgressID(egressID, primaryEgressID string) string {
	if id := strings.TrimSpace(egressID); id != "" {
		return id
	}
	return strings.TrimSpace(primaryEgressID)
}

func (s *Server) resolveImportPrimaryEgress(ctx context.Context, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return storage.DefaultDirectEgressID, nil
	}
	if _, err := s.store.GetEgressProfile(ctx, requested); err == nil {
		return requested, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return storage.DefaultDirectEgressID, nil
}

func (s *Server) bindImportedAccountPrimaryEgress(ctx context.Context, accountID, requested string) error {
	primary, err := s.resolveImportPrimaryEgress(ctx, requested)
	if err != nil {
		return err
	}
	return s.store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{
		AccountID:       accountID,
		PrimaryEgressID: primary,
		CookieJarKey:    accountID + ":" + primary,
	})
}
