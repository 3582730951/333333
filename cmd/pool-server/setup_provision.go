package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/datadir"
	"codex-account-pool/internal/storage"
)

const setupProvisionTokenMaxBytes = 4096

// runProvisionAdminSetup is deliberately stdin-only: plaintext cannot appear in
// argv, process listings, config, the database or logs. Successful output contains
// status metadata only; the installer remains the sole process allowed to display
// its generated plaintext once.
func runProvisionAdminSetup(configPath string) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("admin setup load config: %v", err)
		return 1
	}
	explicitIdentityKey := strings.TrimSpace(cfg.IdentityKeyFile) != ""
	layout, err := datadir.Prepare(cfg.DataDir, cfg.BodySpoolDir, cfg.UsageJournalDir)
	if err != nil {
		log.Printf("admin setup data preflight: %v", err)
		return 1
	}
	if !explicitIdentityKey {
		cfg.IdentityKeyFile = filepath.Join(layout.Keys, "identity.key")
	}
	identityKey, err := loadRuntimeKey(cfg.IdentityKeyFile, explicitIdentityKey)
	if err != nil {
		log.Printf("admin setup identity key: %v", err)
		return 1
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, setupProvisionTokenMaxBytes+1))
	if err != nil {
		log.Printf("admin setup read stdin: %v", err)
		return 1
	}
	if len(raw) > setupProvisionTokenMaxBytes {
		log.Printf("admin setup token exceeds maximum length")
		return 2
	}
	token := strings.TrimSpace(string(raw))
	// A base64url 256-bit token is 43 characters; hex is 64. Reject short human
	// passwords without ever echoing them.
	if len(token) < 43 || strings.ContainsAny(token, " \t\r\n") {
		log.Printf("admin setup token must contain at least 256 bits from a CSPRNG")
		return 2
	}
	store, err := storage.OpenWithConfig(cfg)
	if err != nil {
		log.Printf("admin setup open storage: %v", err)
		return 1
	}
	defer store.Close()
	// The active worker may still be draining requests while the installer stages a
	// replacement.  Use the same bounded lock-retry policy as normal worker startup
	// for both schema validation and the one-time verifier write; a single 5-second
	// SQLite busy timeout is not sufficient when the active writer is between batches.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err = initStorageWithLockRetry(ctx, store.InitExpandOnlyWithProgress, nil, func(format string, args ...any) {
		log.Printf("admin setup: "+format, args...)
	}); err != nil {
		log.Printf("admin setup initialize storage: %v", err)
		return 1
	}
	now := storage.Now()
	// ProvisionAdminSetup is a short write transaction, but it can still collide
	// with the live worker immediately after InitExpandOnly returns.  Retrying only
	// SQLite lock errors keeps the operation bounded while preserving all other
	// failures (invalid schema, permissions, corruption) for the caller.
	err = initStorageWithLockRetry(ctx, func(provisionCtx context.Context, _ func(string)) error {
		return store.ProvisionAdminSetup(provisionCtx, storage.AdminSetupTokenMAC(identityKey, token), now, now+int64((10*time.Minute)/time.Second))
	}, nil, func(format string, args ...any) {
		log.Printf("admin setup: "+format, args...)
	})
	if errors.Is(err, storage.ErrAdminSetupCompleted) {
		fmt.Println(`{"admin_setup":"already_completed"}`)
		return 0
	}
	if err != nil {
		log.Printf("admin setup provision verifier: %v", err)
		return 1
	}
	fmt.Printf("{\"admin_setup\":\"provisioned\",\"expires_at\":%d,\"loopback_only\":true}\n", now+600)
	return 0
}
