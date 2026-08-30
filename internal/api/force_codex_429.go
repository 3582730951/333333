package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
)

// forceCodex429ConfirmWindowSecs is the trailing window (in storage.Now() unix
// seconds) within which two explicit 429s on one account confirm "强制卡429"
// same-account retention. Mirrors sub2api-plus openAIOAuth429ConfirmationWindow.
const forceCodex429ConfirmWindowSecs = 30

// forceCodex429StormWorkers and forceCodex429StormWindow are deliberately
// fixed production defaults.  The variables below keep the behavior
// deterministic in production while allowing the focused gateway tests to use
// a short window without sleeping for fifteen minutes.
const (
	forceCodex429StormWorkers = 100
	forceCodex429StormWindow  = 15 * time.Minute
)

var (
	forceCodex429StormConcurrency = forceCodex429StormWorkers
	forceCodex429StormDuration    = forceCodex429StormWindow
)

// errForceCodex429StormExpired means that the account was held on the original
// lease for the complete force-429 window but no non-429 upstream response was
// received.  The caller must then leave this account out of the current request
// and let the normal failover selector choose another account.
var errForceCodex429StormExpired = errors.New("force codex 429 retry window expired")

// forceCodex429State tracks the trailing-window 429 count for one account. Pure
// in-memory state: an opt-in, niche override, lost on restart without harm.
type forceCodex429State struct {
	count       int
	windowStart int64
}

// confirmForceCodex429 advances the per-account trailing-window 429 counter and
// reports whether the account has now produced two explicit 429s within
// forceCodex429ConfirmWindowSecs. The first 429 returns false so the ordinary
// failover/cooldown path handles it; the second inside the window returns true so
// the caller switches to same-account retention (the analog of sub2api-plus
// pinning the connection after confirming the 429 guard). The window resets
// naturally: a 429 arriving after the window elapsed restarts the count at one.
func (s *Server) confirmForceCodex429(ctx context.Context, accountID string) bool {
	now := storage.Now()
	s.forceCodex429Mu.Lock()
	defer s.forceCodex429Mu.Unlock()
	if s.forceCodex429Counts == nil {
		s.forceCodex429Counts = make(map[string]*forceCodex429State)
	}
	st := s.forceCodex429Counts[accountID]
	if st == nil || now-st.windowStart > forceCodex429ConfirmWindowSecs {
		s.forceCodex429Counts[accountID] = &forceCodex429State{count: 1, windowStart: now}
		return false
	}
	st.count++
	return st.count >= 2
}

// forceCodex429CancelBody keeps the winning response's request context alive
// after the storm helper returns.  The other 99 attempts are cancelled as soon
// as a winner is found, but cancelling a shared parent context here would also
// cancel the winner's response body before codexAttempt can read/stream it.
// Closing the body (which codexAttempt always does) finally releases the
// winner's per-worker context.
type forceCodex429CancelBody struct {
	io.ReadCloser
	cancelOnce sync.Once
	cancel     context.CancelFunc
}

func (b *forceCodex429CancelBody) Close() error {
	if b == nil {
		return nil
	}
	var err error
	if b.ReadCloser != nil {
		err = b.ReadCloser.Close()
	}
	b.cancelOnce.Do(func() {
		if b.cancel != nil {
			b.cancel()
		}
	})
	return err
}

type forceCodex429StormResponse struct {
	resp   *upstream.Response
	egress storage.EgressProfile
}

// retryCodexForce429Storm sends the same, already-shaped logical request from
// exactly 100 independent workers.  It intentionally calls the raw Codex
// transport boundary rather than retryCodexSameAccountAfterRateLimit: there is
// no Retry-After sleep, account cooldown, credential backoff, or scheduler
// reselection while the fifteen-minute window is active.
//
// A 429 is treated as the absence of a usable response and starts the worker's
// next request immediately.  Any other HTTP response (including an explicit
// 4xx/5xx) is a response and wins the race; codexAttempt's ordinary state
// machine then decides whether that response should be surfaced or fail over.
// Transport errors are retried until a worker gets a response, the downstream
// context is cancelled, or the fifteen-minute window expires.
//
// The storm is request-scoped.  There is no account-global mutex, so unrelated
// downstream connections continue to forward normally while one connection is
// in this mode.  WebSocket sessions are deliberately detached for storm probes:
// one persistent WS cannot safely carry 100 concurrent frames, while independent
// HTTP/SSE attempts preserve the exact body and account/egress identity.
func (s *Server) retryCodexForce429Storm(ctx context.Context, lease scheduler.Lease, request func() upstream.Request) (*upstream.Response, storage.EgressProfile, error) {
	if request == nil {
		return nil, lease.Egress, errors.New("force codex 429 request factory is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || s.upstreamDo == nil {
		return nil, lease.Egress, fmt.Errorf("%w: client is unavailable", errInvalidUpstreamResponse)
	}
	workers := forceCodex429StormConcurrency
	if workers < 1 {
		workers = 1
	}
	if workers > forceCodex429StormWorkers {
		workers = forceCodex429StormWorkers
	}
	duration := forceCodex429StormDuration
	if duration <= 0 {
		duration = forceCodex429StormWindow
	}

	s.auditForceCodex429Storm(ctx, lease, "started", fmt.Sprintf("concurrency=%d window_seconds=%d cooldown=false", workers, int(duration/time.Second)))

	// Each worker owns one cancellable context for all of its repeated attempts.
	// Keeping the context per worker (instead of sharing one parent) lets the
	// winner keep reading/streaming its response after the other workers stop.
	workerCancels := make([]context.CancelFunc, workers)
	workerContexts := make([]context.Context, workers)
	for i := 0; i < workers; i++ {
		workerContexts[i], workerCancels[i] = context.WithCancel(ctx)
	}

	stormExpired := make(chan struct{})
	var stormExpiredFlag atomic.Bool
	timerStop := make(chan struct{})
	timerDone := make(chan struct{})
	var timerStopOnce sync.Once
	go func() {
		defer close(timerDone)
		defer supervisor.Recover("codex-force-429-storm-timer")
		timer := time.NewTimer(duration)
		defer timer.Stop()
		select {
		case <-timer.C:
			stormExpiredFlag.Store(true)
			close(stormExpired)
			for _, cancel := range workerCancels {
				cancel()
			}
		case <-timerStop:
		case <-ctx.Done():
		}
	}()
	stopTimer := func() {
		timerStopOnce.Do(func() { close(timerStop) })
		<-timerDone
	}

	winnerDone := make(chan struct{})
	var winnerOnce sync.Once
	var winnerIndex atomic.Int32
	winnerIndex.Store(-1)
	resultCh := make(chan forceCodex429StormResponse, 1)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		idx := i
		workerCtx := workerContexts[idx]
		workerCancel := workerCancels[idx]
		go func() {
			defer wg.Done()
			defer supervisor.Recover("codex-force-429-storm-worker")
			ownsWinnerContext := false
			defer func() {
				if !ownsWinnerContext {
					workerCancel()
				}
			}()

			for {
				select {
				case <-winnerDone:
					return
				case <-stormExpired:
					return
				case <-workerCtx.Done():
					return
				default:
				}

				req := request()
				// Header maps are reference values.  Clone before handing a request to
				// an independent transport so a provider adapter cannot race another
				// worker's header projection.  A persistent WS is never shared by the
				// storm; every probe is an independent HTTP/SSE attempt.
				req.Headers = req.Headers.Clone()
				req.CodexResponsesWebSocket = false
				req.CodexWebSocketSession = nil
				mapping := codexSessionMappingFromContext(workerCtx)
				s.recordCodexUpstreamAttempt(workerCtx, mapping, lease, req.Egress, "force_429_storm_attempted", 0)
				resp, err := s.doCodexUpstream(workerCtx, req)
				if err != nil {
					if resp != nil && resp.Body != nil {
						_ = resp.Body.Close()
					}
					if workerCtx.Err() != nil || ctx.Err() != nil || stormExpiredFlag.Load() {
						return
					}
					s.recordCodexUpstreamAttempt(workerCtx, mapping, lease, req.Egress, "force_429_storm_transport_error", 0)
					continue
				}
				if resp == nil {
					continue
				}
				s.recordCodexUpstreamAttempt(workerCtx, mapping, lease, req.Egress, "force_429_storm_response", resp.StatusCode)
				if resp.StatusCode == http.StatusTooManyRequests {
					// Do not wait for Retry-After.  Closing promptly releases the
					// connection and the worker immediately issues the next probe.
					if resp.Body != nil {
						_ = resp.Body.Close()
					}
					continue
				}

				// The timeout and winner race is resolved by the atomic flag plus the
				// context checks above.  A response that arrived before expiry wins;
				// a response observed after expiry is closed and the caller fails over.
				if stormExpiredFlag.Load() || ctx.Err() != nil {
					if resp.Body != nil {
						_ = resp.Body.Close()
					}
					return
				}
				if winnerIndex.CompareAndSwap(-1, int32(idx)) {
					ownsWinnerContext = true
					winnerOnce.Do(func() { close(winnerDone) })
					for j, cancel := range workerCancels {
						if j != idx {
							cancel()
						}
					}
					if resp.Body != nil {
						resp.Body = &forceCodex429CancelBody{ReadCloser: resp.Body, cancel: workerCancel}
					} else {
						workerCancel()
						return
					}
					resultCh <- forceCodex429StormResponse{resp: resp, egress: req.Egress}
					return
				}
				if resp.Body != nil {
					_ = resp.Body.Close()
				}
				return
			}
		}()
	}

	var result forceCodex429StormResponse
	var stormErr error
	select {
	case result = <-resultCh:
		// Keep the winner context alive; its body wrapper cancels it when the
		// normal Codex response path has consumed/closed the body.
		stopTimer()
	case <-stormExpired:
		stormErr = errForceCodex429StormExpired
		for _, cancel := range workerCancels {
			cancel()
		}
		stopTimer()
	case <-ctx.Done():
		stormErr = ctx.Err()
		for _, cancel := range workerCancels {
			cancel()
		}
		stopTimer()
	}

	// All regular transports honor context cancellation.  Wait for every worker
	// before returning so a timed-out storm cannot continue issuing requests in
	// the background and so the account lease can be released deterministically.
	wg.Wait()
	if result.resp != nil {
		s.auditForceCodex429Storm(ctx, lease, "responded", fmt.Sprintf("status=%d", result.resp.StatusCode))
		return result.resp, result.egress, nil
	}
	if errors.Is(stormErr, context.Canceled) && ctx.Err() == nil {
		stormErr = errForceCodex429StormExpired
	}
	if errors.Is(stormErr, errForceCodex429StormExpired) {
		s.auditForceCodex429Storm(ctx, lease, "expired", "no_non_429_response_within_window")
		return nil, lease.Egress, errForceCodex429StormExpired
	}
	s.auditForceCodex429Storm(ctx, lease, "cancelled", firstNonEmpty(stormErrString(stormErr), "request_context_cancelled"))
	return nil, lease.Egress, stormErr
}

func stormErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) auditForceCodex429Storm(ctx context.Context, lease scheduler.Lease, state, detail string) {
	if s == nil || s.store == nil {
		return
	}
	trimmedDetail := strings.TrimSpace(detail)
	if trimmedDetail != "" && len(trimmedDetail) > 256 {
		detail = trimmedDetail[:256]
	} else {
		detail = trimmedDetail
	}
	_ = s.store.InsertAuditLog(context.WithoutCancel(ctx), storage.AuditLogRow{
		AccountID: lease.Account.ID, AccountLabel: lease.Account.Label,
		Action: "codex_force_429_storm", State: state, Reason: "confirmed_429", Detail: detail,
	})
}
