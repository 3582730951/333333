package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-account-pool/internal/api"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/datadir"
	"codex-account-pool/internal/storage"
)

const setupProvisionTokenMaxBytes = 4096
const bootstrapAdminMaxBytes = 16 * 1024

type bootstrapAdminInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

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

// runBootstrapAdmin is the non-browser counterpart of /setup/claim-admin. It
// keeps credentials on stdin, performs the same one-admin atomic claim, and
// creates a normal login session so an interactive installer can finish with a
// usable account without exposing a setup token or password in the process list.
func runBootstrapAdmin(configPath string) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("admin bootstrap load config: %v", err)
		return 1
	}
	explicitIdentityKey := strings.TrimSpace(cfg.IdentityKeyFile) != ""
	layout, err := datadir.Prepare(cfg.DataDir, cfg.BodySpoolDir, cfg.UsageJournalDir)
	if err != nil {
		log.Printf("admin bootstrap data preflight: %v", err)
		return 1
	}
	if !explicitIdentityKey {
		cfg.IdentityKeyFile = filepath.Join(layout.Keys, "identity.key")
	}
	identityKey, err := loadRuntimeKey(cfg.IdentityKeyFile, explicitIdentityKey)
	if err != nil {
		log.Printf("admin bootstrap identity key: %v", err)
		return 1
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, bootstrapAdminMaxBytes+1))
	if err != nil || len(raw) > bootstrapAdminMaxBytes {
		log.Printf("admin bootstrap input is unavailable or too large")
		return 2
	}
	var input bootstrapAdminInput
	if err := json.Unmarshal(raw, &input); err != nil {
		// The installer uses a line-oriented form so it never needs a JSON
		// encoder that could copy the password into argv or shell history.
		lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		if len(lines) < 2 {
			log.Printf("admin bootstrap input must be JSON or email/password lines")
			return 2
		}
		input.Email, input.Password = strings.TrimSpace(lines[0]), lines[1]
		if len(lines) > 2 {
			input.Name = lines[2]
		}
	}
	input.Email = strings.TrimSpace(strings.ToLower(input.Email))
	if !strings.Contains(input.Email, "@") || len(input.Password) < 8 {
		log.Printf("admin bootstrap requires a valid email and password of at least 8 characters")
		return 2
	}
	passwordHash, err := api.HashPassword(input.Password)
	if err != nil {
		log.Printf("admin bootstrap password verifier unavailable")
		return 1
	}
	store, err := storage.OpenWithConfig(cfg)
	if err != nil {
		log.Printf("admin bootstrap open storage: %v", err)
		return 1
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err = initStorageWithLockRetry(ctx, store.InitExpandOnlyWithProgress, nil, func(format string, args ...any) {
		log.Printf("admin bootstrap: "+format, args...)
	}); err != nil {
		log.Printf("admin bootstrap initialize storage: %v", err)
		return 1
	}
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		log.Printf("admin bootstrap token unavailable")
		return 1
	}
	setupToken := hex.EncodeToString(tokenBytes[:])
	now := storage.Now()
	if err = initStorageWithLockRetry(ctx, func(provisionCtx context.Context, _ func(string)) error {
		return store.ProvisionAdminSetup(provisionCtx, storage.AdminSetupTokenMAC(identityKey, setupToken), now, now+600)
	}, nil, func(format string, args ...any) {
		log.Printf("admin bootstrap: "+format, args...)
	}); errors.Is(err, storage.ErrAdminSetupCompleted) {
		fmt.Println(`{"admin_setup":"already_completed"}`)
		return 0
	} else if err != nil {
		log.Printf("admin bootstrap provision verifier: %v", err)
		return 1
	}
	var sessionBytes [32]byte
	if _, err := rand.Read(sessionBytes[:]); err != nil {
		log.Printf("admin bootstrap session unavailable")
		return 1
	}
	adminID := "usr_" + hex.EncodeToString(sessionBytes[:8])
	admin, err := store.ClaimAdminSetup(ctx, storage.AdminSetupTokenMAC(identityKey, setupToken), storage.User{
		ID: adminID, Email: input.Email, Name: strings.TrimSpace(input.Name), PasswordHash: passwordHash,
	}, storage.UserSession{
		TokenHash: api.HashAPIKey(hex.EncodeToString(sessionBytes[:])), UserID: adminID,
		UserAgent: "codex-pool-installer", CreatedAt: now, ExpiresAt: now + 30*24*60*60,
	}, now)
	if errors.Is(err, storage.ErrAdminSetupCompleted) {
		fmt.Println(`{"admin_setup":"already_completed"}`)
		return 0
	}
	if err != nil {
		log.Printf("admin bootstrap claim failed: %v", err)
		return 1
	}
	fmt.Printf("{\"admin_setup\":\"created\",\"email\":%q,\"user_id\":%q}\n", admin.Email, admin.ID)
	return 0
}
