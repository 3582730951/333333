package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
)

// The scheduler retains its configured egress and sidecar hard limits. This
// controller is a second, transport-local safety valve: it learns a safe level
// of parallelism for one sidecar process + real exit pair without allowing one
// unhealthy exit to throttle every account using that sidecar.
const (
	sidecarAdaptiveInitialLimit = 4
	sidecarAdaptiveMinLimit     = 1
	sidecarAdaptiveMaxLimit     = 16
	sidecarAdaptiveQueueWait    = 2500 * time.Millisecond
	sidecarFailureWindow        = 30 * time.Second
	sidecarCircuitOpenFor       = 10 * time.Second
	sidecarBypassFor            = 60 * time.Second
)

var errSidecarAdaptiveQueueTimeout = errors.New("sidecar adaptive concurrency queue timeout")

type sidecarCircuitOpenError struct {
	RetryAt     time.Time
	BypassUntil time.Time
	BypassReady bool
}

func (e *sidecarCircuitOpenError) Error() string {
	return "sidecar adaptive circuit open"
}

type sidecarOutcome uint8

const (
	sidecarOutcomeNeutral sidecarOutcome = iota
	sidecarOutcomeSuccess
	sidecarOutcomeFailure
)

type sidecarAdaptiveWaiter struct{}

type sidecarAdaptiveState struct {
	sidecarID string
	egressID  string

	limit        int
	inflight     int
	waiters      []*sidecarAdaptiveWaiter
	changed      chan struct{}
	failures     []time.Time
	successes    int
	openUntil    time.Time
	bypassUntil  time.Time
	bypassActive bool
	halfOpen     bool
	halfProbe    bool
	updatedAt    time.Time
}

type sidecarAdaptiveController struct {
	mu     sync.Mutex
	states map[string]*sidecarAdaptiveState
}

type sidecarAdaptiveLease struct {
	controller *sidecarAdaptiveController
	key        string
	hardCap    int
	once       sync.Once
}

// adaptiveSidecarBody holds a sidecar admission slot until the upstream body
// actually completes. Releasing at response headers would let a large number of
// long SSE streams bypass the learned concurrency limit. Trailer failures are
// transport failures even though their HTTP status has already been committed.
type adaptiveSidecarBody struct {
	io.ReadCloser
	trailer http.Header
	lease   *sidecarAdaptiveLease
}

func (b *adaptiveSidecarBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		if strings.TrimSpace(b.trailer.Get("X-Sidecar-Stream-Error-Code")) != "" {
			b.lease.release(sidecarOutcomeFailure)
		} else {
			b.lease.release(sidecarOutcomeSuccess)
		}
	} else if err != nil {
		b.lease.release(sidecarOutcomeFailure)
	}
	return n, err
}

func (b *adaptiveSidecarBody) Close() error {
	err := b.ReadCloser.Close()
	// A caller may close after cancelling its own downstream connection. That is
	// not evidence that the sidecar or exit failed, so it releases capacity without
	// teaching the controller a spurious failure.
	b.lease.release(sidecarOutcomeNeutral)
	return err
}

// SidecarAdaptiveStatus contains only transport configuration identifiers and
// counters. It deliberately excludes sidecar endpoints, chain URLs, cookies,
// request bodies, response identifiers, and account identities so it is safe to
// include in the administrator diagnostics bundle.
type SidecarAdaptiveStatus struct {
	SidecarEgressID string
	RealEgressID    string
	Limit           int
	Inflight        int
	QueueDepth      int
	RecentFailures  int
	CircuitState    string
	CircuitUntil    int64
	BypassUntil     int64
	UpdatedAt       int64
}

func newSidecarAdaptiveController() *sidecarAdaptiveController {
	return &sidecarAdaptiveController{states: map[string]*sidecarAdaptiveState{}}
}

func sidecarAdaptiveIdentity(egress storage.EgressProfile) (key, sidecarID, realEgressID string) {
	realEgressID = strings.TrimSpace(egress.ID)
	sidecarID = strings.TrimSpace(egress.TransportSidecarID)
	if sidecarID == "" {
		sidecarID = realEgressID
	}
	if realEgressID == "" {
		realEgressID = "unknown-egress"
	}
	if sidecarID == "" {
		sidecarID = "unknown-sidecar"
	}
	return sidecarID + "\x00" + realEgressID, sidecarID, realEgressID
}

func sidecarAdaptiveHardCap(egress storage.EgressProfile) int {
	if egress.TransportSidecarMaxConcurrency > 0 {
		return egress.TransportSidecarMaxConcurrency
	}
	return egress.MaxConcurrency
}

func sidecarAdaptiveLimit(hardCap int) int {
	limit := sidecarAdaptiveMaxLimit
	if hardCap > 0 && hardCap < limit {
		limit = hardCap
	}
	if limit < sidecarAdaptiveMinLimit {
		return sidecarAdaptiveMinLimit
	}
	return limit
}

func (c *sidecarAdaptiveController) stateLocked(key, sidecarID, egressID string, hardCap int, now time.Time) *sidecarAdaptiveState {
	state := c.states[key]
	if state == nil {
		initial := sidecarAdaptiveInitialLimit
		if max := sidecarAdaptiveLimit(hardCap); initial > max {
			initial = max
		}
		state = &sidecarAdaptiveState{
			sidecarID: sidecarID,
			egressID:  egressID,
			limit:     initial,
			changed:   make(chan struct{}),
			updatedAt: now,
		}
		c.states[key] = state
	}
	max := sidecarAdaptiveLimit(hardCap)
	if state.limit > max {
		state.limit = max
	}
	if state.limit < sidecarAdaptiveMinLimit {
		state.limit = sidecarAdaptiveMinLimit
	}
	return state
}

func (c *sidecarAdaptiveController) signalLocked(state *sidecarAdaptiveState) {
	close(state.changed)
	state.changed = make(chan struct{})
	state.updatedAt = time.Now()
}

func (c *sidecarAdaptiveController) trimFailuresLocked(state *sidecarAdaptiveState, now time.Time) {
	cutoff := now.Add(-sidecarFailureWindow)
	kept := state.failures[:0]
	for _, failure := range state.failures {
		if failure.After(cutoff) {
			kept = append(kept, failure)
		}
	}
	state.failures = kept
}

func removeSidecarWaiter(waiters []*sidecarAdaptiveWaiter, wanted *sidecarAdaptiveWaiter) []*sidecarAdaptiveWaiter {
	for index, waiter := range waiters {
		if waiter == wanted {
			copy(waiters[index:], waiters[index+1:])
			return waiters[:len(waiters)-1]
		}
	}
	return waiters
}

func (c *sidecarAdaptiveController) acquire(ctx context.Context, egress storage.EgressProfile) (*sidecarAdaptiveLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(sidecarAdaptiveQueueWait)
	key, sidecarID, egressID := sidecarAdaptiveIdentity(egress)
	hardCap := sidecarAdaptiveHardCap(egress)
	var waiter *sidecarAdaptiveWaiter
	for {
		now := time.Now()
		c.mu.Lock()
		state := c.stateLocked(key, sidecarID, egressID, hardCap, now)
		c.trimFailuresLocked(state, now)
		if !state.openUntil.IsZero() && !now.Before(state.openUntil) {
			state.openUntil = time.Time{}
			state.halfOpen = true
			c.signalLocked(state)
		}
		if state.bypassActive && now.Before(state.bypassUntil) {
			if waiter != nil {
				state.waiters = removeSidecarWaiter(state.waiters, waiter)
				c.signalLocked(state)
			}
			err := &sidecarCircuitOpenError{RetryAt: state.openUntil, BypassUntil: state.bypassUntil, BypassReady: true}
			c.mu.Unlock()
			return nil, err
		}
		if state.bypassActive && !now.Before(state.bypassUntil) {
			state.bypassActive = false
		}
		if now.Before(state.openUntil) {
			if waiter != nil {
				state.waiters = removeSidecarWaiter(state.waiters, waiter)
				c.signalLocked(state)
			}
			err := &sidecarCircuitOpenError{RetryAt: state.openUntil, BypassUntil: state.bypassUntil}
			c.mu.Unlock()
			return nil, err
		}
		max := sidecarAdaptiveLimit(hardCap)
		isHead := waiter == nil && len(state.waiters) == 0 || waiter != nil && len(state.waiters) > 0 && state.waiters[0] == waiter
		canProbe := !state.halfOpen || !state.halfProbe
		if isHead && canProbe && state.inflight < max && state.inflight < state.limit {
			if waiter != nil {
				state.waiters = removeSidecarWaiter(state.waiters, waiter)
			}
			state.inflight++
			if state.halfOpen {
				state.halfProbe = true
			}
			state.updatedAt = now
			c.signalLocked(state)
			c.mu.Unlock()
			return &sidecarAdaptiveLease{controller: c, key: key, hardCap: hardCap}, nil
		}
		if waiter == nil {
			waiter = &sidecarAdaptiveWaiter{}
			state.waiters = append(state.waiters, waiter)
			c.signalLocked(state)
		}
		changed := state.changed
		c.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			c.mu.Lock()
			state := c.states[key]
			if state != nil {
				state.waiters = removeSidecarWaiter(state.waiters, waiter)
				c.signalLocked(state)
			}
			c.mu.Unlock()
			return nil, errSidecarAdaptiveQueueTimeout
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			c.mu.Lock()
			state := c.states[key]
			if state != nil {
				state.waiters = removeSidecarWaiter(state.waiters, waiter)
				c.signalLocked(state)
			}
			c.mu.Unlock()
			return nil, ctx.Err()
		case <-timer.C:
			c.mu.Lock()
			state := c.states[key]
			if state != nil {
				state.waiters = removeSidecarWaiter(state.waiters, waiter)
				c.signalLocked(state)
			}
			c.mu.Unlock()
			return nil, errSidecarAdaptiveQueueTimeout
		case <-changed:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
}

func (l *sidecarAdaptiveLease) release(outcome sidecarOutcome) {
	if l == nil || l.controller == nil {
		return
	}
	l.once.Do(func() {
		c := l.controller
		c.mu.Lock()
		defer c.mu.Unlock()
		state := c.states[l.key]
		if state == nil {
			return
		}
		if state.inflight > 0 {
			state.inflight--
		}
		now := time.Now()
		max := sidecarAdaptiveLimit(l.hardCap)
		switch outcome {
		case sidecarOutcomeSuccess:
			if state.halfOpen {
				state.halfOpen = false
				state.halfProbe = false
				state.failures = nil
				state.bypassUntil = time.Time{}
				state.bypassActive = false
				if state.limit < max {
					state.limit++
				}
			} else {
				state.successes++
				if state.successes >= state.limit && state.limit < max {
					state.limit++
					state.successes = 0
				}
			}
		case sidecarOutcomeFailure:
			state.successes = 0
			state.halfProbe = false
			c.trimFailuresLocked(state, now)
			state.failures = append(state.failures, now)
			if state.limit > sidecarAdaptiveMinLimit {
				state.limit = maxInt(sidecarAdaptiveMinLimit, state.limit/2)
			}
			if state.halfOpen || len(state.failures) >= 3 {
				state.halfOpen = false
				state.openUntil = now.Add(sidecarCircuitOpenFor)
				state.bypassUntil = now.Add(sidecarBypassFor)
				state.bypassActive = false
			}
		case sidecarOutcomeNeutral:
			if state.halfOpen {
				state.halfProbe = false
			}
		}
		state.updatedAt = now
		c.signalLocked(state)
	})
}

// enableBypass is called only after the caller has spent its bounded recovery
// window on structured pre-header failures and has selected the same real proxy
// chain. Subsequent callers may use that safe bypass for up to one minute instead
// of each queueing another ten-second recovery window.
func (c *sidecarAdaptiveController) enableBypass(egress storage.EgressProfile) {
	if c == nil {
		return
	}
	key, sidecarID, egressID := sidecarAdaptiveIdentity(egress)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.stateLocked(key, sidecarID, egressID, sidecarAdaptiveHardCap(egress), now)
	c.trimFailuresLocked(state, now)
	// The caller may take a one-request same-chain fallback as soon as its
	// bounded recovery window expires, but the shared 60-second bypass is a
	// circuit-breaker action. Do not let a short caller deadline turn one
	// certified preflight failure into a minute of bypass for every tenant.
	if len(state.failures) < 3 && !now.Before(state.openUntil) {
		return
	}
	state.bypassActive = true
	state.bypassUntil = now.Add(sidecarBypassFor)
	c.signalLocked(state)
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func unixOrZero(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.Unix()
}

func (c *sidecarAdaptiveController) statuses() []SidecarAdaptiveStatus {
	if c == nil {
		return nil
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	rows := make([]SidecarAdaptiveStatus, 0, len(c.states))
	for _, state := range c.states {
		c.trimFailuresLocked(state, now)
		circuitState := "closed"
		if state.bypassActive && now.Before(state.bypassUntil) {
			circuitState = "bypass"
		} else if now.Before(state.openUntil) {
			circuitState = "open"
		} else if state.halfOpen {
			circuitState = "half_open"
		}
		rows = append(rows, SidecarAdaptiveStatus{
			SidecarEgressID: state.sidecarID,
			RealEgressID:    state.egressID,
			Limit:           state.limit,
			Inflight:        state.inflight,
			QueueDepth:      len(state.waiters),
			RecentFailures:  len(state.failures),
			CircuitState:    circuitState,
			CircuitUntil:    unixOrZero(state.openUntil),
			BypassUntil:     unixOrZero(state.bypassUntil),
			UpdatedAt:       unixOrZero(state.updatedAt),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SidecarEgressID == rows[j].SidecarEgressID {
			return rows[i].RealEgressID < rows[j].RealEgressID
		}
		return rows[i].SidecarEgressID < rows[j].SidecarEgressID
	})
	return rows
}
