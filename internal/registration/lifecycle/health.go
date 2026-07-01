// Package lifecycle manages registered account health and renewal
package lifecycle

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

// HealthChecker tests account validity
type HealthChecker struct {
	httpClient *http.Client
	store      *storage.Store
}

// NewHealthChecker creates a health checker
func NewHealthChecker(store *storage.Store) *HealthChecker {
	return &HealthChecker{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		store: store,
	}
}

// HealthStatus represents account health state
type HealthStatus struct {
	Alive       bool
	StatusCode  int
	ErrorReason string
	CheckedAt   int64
	ResponseMS  int64
}

// CheckAccount verifies if an account is still valid
func (h *HealthChecker) CheckAccount(ctx context.Context, account storage.Account) HealthStatus {
	start := time.Now()
	status := HealthStatus{
		CheckedAt: time.Now().Unix(),
	}

	// Fetch token from store
	token, err := h.store.GetToken(ctx, account.ID)
	if err != nil {
		status.ErrorReason = "no token found"
		return status
	}

	// Determine platform and check method
	switch account.Provider {
	case "codex", "openai", "":
		status = h.checkOpenAIAccount(ctx, token)
	case "claude":
		status = h.checkClaudeAccount(ctx, account.ID)
	default:
		status.ErrorReason = "unsupported platform: " + account.Provider
		status.StatusCode = 0
	}

	status.ResponseMS = time.Since(start).Milliseconds()
	return status
}

// checkOpenAIAccount verifies ChatGPT/OpenAI account
func (h *HealthChecker) checkOpenAIAccount(ctx context.Context, token storage.AccountToken) HealthStatus {
	status := HealthStatus{}

	if token.AccessToken == "" {
		status.ErrorReason = "no session token"
		return status
	}

	// Test with /backend-api/models endpoint (lightweight)
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://chatgpt.com/backend-api/models", nil)
	if err != nil {
		status.ErrorReason = err.Error()
		return status
	}

	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		status.ErrorReason = err.Error()
		return status
	}
	defer resp.Body.Close()

	status.StatusCode = resp.StatusCode

	switch resp.StatusCode {
	case 200:
		status.Alive = true
	case 401:
		status.ErrorReason = "unauthorized (session expired)"
	case 403:
		status.ErrorReason = "forbidden (banned)"
	case 429:
		status.ErrorReason = "rate limited"
	default:
		status.ErrorReason = fmt.Sprintf("http %d", resp.StatusCode)
	}

	return status
}

// checkClaudeAccount verifies Claude account
func (h *HealthChecker) checkClaudeAccount(ctx context.Context, accountID string) HealthStatus {
	status := HealthStatus{}

	cookie, err := h.store.GetSessionCookie(ctx, accountID)
	if err != nil || cookie == "" {
		status.ErrorReason = "no session cookie"
		return status
	}

	// Test with /api/organizations endpoint
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://api.claude.ai/api/organizations", nil)
	if err != nil {
		status.ErrorReason = err.Error()
		return status
	}

	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		status.ErrorReason = err.Error()
		return status
	}
	defer resp.Body.Close()

	status.StatusCode = resp.StatusCode

	switch resp.StatusCode {
	case 200:
		status.Alive = true
	case 401, 403:
		status.ErrorReason = "unauthorized"
	case 429:
		status.ErrorReason = "rate limited"
	default:
		status.ErrorReason = fmt.Sprintf("http %d", resp.StatusCode)
	}

	return status
}

// BatchCheckAccounts checks multiple accounts concurrently with a bounded worker
// pool. The concurrency limit defaults to 10 and can be overridden via limit (a
// value <= 0 keeps the default). Bounding the pool prevents a large account pool
// from spawning thousands of in-flight HTTP requests on a low-RAM VPS.
func (h *HealthChecker) BatchCheckAccounts(ctx context.Context, accounts []storage.Account) []HealthStatus {
	return h.BatchCheckAccountsN(ctx, accounts, 0)
}

// BatchCheckAccountsN is the concurrency-parameterized variant.
func (h *HealthChecker) BatchCheckAccountsN(ctx context.Context, accounts []storage.Account, limit int) []HealthStatus {
	if limit <= 0 {
		limit = 10
	}
	results := make([]HealthStatus, len(accounts))
	sem := make(chan struct{}, limit)

	var wg sync.WaitGroup
	for i, acc := range accounts {
		select {
		case <-ctx.Done():
			// Context cancelled — stop launching new checks; already-running ones finish.
			break
		case sem <- struct{}{}:
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(idx int, account storage.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if v := recover(); v != nil {
					supervisor.LogPanic("registration-health-check", v)
					results[idx] = HealthStatus{
						Alive:       false,
						ErrorReason: fmt.Sprintf("health check panic: %v", v),
						CheckedAt:   time.Now().Unix(),
					}
				}
			}()
			results[idx] = h.CheckAccount(ctx, account)
		}(i, acc)
	}
	wg.Wait()
	return results
}
