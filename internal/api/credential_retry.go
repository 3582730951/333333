package api

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

const credentialRetryDrainLimit = 1 << 20

func credentialAttemptLimit(account storage.Account) int {
	attempts := account.RetryMaxAttempts
	if attempts <= 1 {
		return 1
	}
	if attempts > 3 {
		return 3
	}
	return attempts
}

func credentialRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	default:
		return false
	}
}

func credentialRetryDelay(header http.Header, attempt int) time.Duration {
	if header != nil {
		if value := strings.TrimSpace(header.Get("Retry-After")); value != "" {
			if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
				delay := time.Duration(seconds) * time.Second
				if delay > time.Second {
					delay = time.Second
				}
				return delay
			}
			if when, err := http.ParseTime(value); err == nil {
				delay := time.Until(when)
				if delay > 0 {
					if delay > time.Second {
						delay = time.Second
					}
					return delay
				}
			}
		}
	}
	delay := time.Duration(100*(1<<maxInt(0, attempt-1))) * time.Millisecond
	if delay > time.Second {
		delay = time.Second
	}
	return delay
}

func waitCredentialRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// doAccountCredentialRetry repeats only a fully replayable request on the exact
// leased credential and egress. It runs before any downstream bytes are written,
// never retries auth/rate/model errors, and is opt-in per account. Kiro callers do
// not use this helper because generateAssistantResponse is not idempotent.
func (s *Server) doAccountCredentialRetry(
	ctx context.Context,
	account storage.Account,
	replaySafe bool,
	do func() (*upstream.Response, error),
) (*upstream.Response, error, int) {
	limit := credentialAttemptLimit(account)
	if !replaySafe {
		limit = 1
	}
	for attempt := 1; attempt <= limit; attempt++ {
		resp, err := do()
		retryable := ctx.Err() == nil && (err != nil || (resp != nil && credentialRetryableStatus(resp.StatusCode)))
		if !retryable || attempt == limit {
			return resp, err, attempt
		}
		header := http.Header(nil)
		if resp != nil {
			header = resp.Header
			if resp.Body != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, credentialRetryDrainLimit))
				_ = resp.Body.Close()
			}
		}
		if waitErr := waitCredentialRetry(ctx, credentialRetryDelay(header, attempt)); waitErr != nil {
			return nil, waitErr, attempt
		}
	}
	return nil, context.Canceled, limit
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
