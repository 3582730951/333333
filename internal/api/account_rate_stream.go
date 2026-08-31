package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
)

const (
	accountRateMaxIDs        = 100
	accountRateDefaultPeriod = time.Second
	accountRateHeartbeat     = 20 * time.Second
)

type accountRateFrame struct {
	SampledAt     int64                                 `json:"sampled_at"`
	WindowSeconds int64                                 `json:"window_seconds,omitempty"`
	Accounts      map[string]storage.AccountRequestRate `json:"accounts"`
	sequence      uint64
}

type accountRateSubscription struct {
	ids     []string
	updates chan accountRateFrame
}

func accountRateValuesEqual(left, right storage.AccountRequestRate) bool {
	left.SampledAt, right.SampledAt = 0, 0
	return left == right
}

// accountRateHub owns the only recurring account-rate query loop in a process.
// Browser tabs subscribe to subsets; each tick queries the union and fans the same
// sampled snapshot out, preventing per-tab database polling amplification.
type accountRateHub struct {
	meter     *storage.AccountRateMeter
	releaseID string

	mu        sync.Mutex
	subs      map[*accountRateSubscription]struct{}
	wake      chan struct{}
	startOnce sync.Once
	sequence  atomic.Uint64
}

func newAccountRateHub(meter *storage.AccountRateMeter, releaseID string) *accountRateHub {
	releaseID = strings.TrimSpace(releaseID)
	if releaseID == "" {
		releaseID = "development"
	}
	return &accountRateHub{
		meter: meter, releaseID: releaseID,
		subs: make(map[*accountRateSubscription]struct{}),
		wake: make(chan struct{}, 1),
	}
}

func (h *accountRateHub) Start(ctx context.Context) {
	if h == nil || ctx == nil {
		return
	}
	h.startOnce.Do(func() { supervisor.Go(ctx, "account-rate-hub", h.run) })
}

func (h *accountRateHub) Subscribe(ids []string) (*accountRateSubscription, func()) {
	if h == nil {
		return nil, func() {}
	}
	sub := &accountRateSubscription{ids: append([]string(nil), ids...), updates: make(chan accountRateFrame, 1)}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
	var once sync.Once
	return sub, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, sub)
			h.mu.Unlock()
		})
	}
}

func (h *accountRateHub) run(ctx context.Context) {
	ticker := time.NewTicker(accountRateDefaultPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.sample(ctx)
		case <-h.wake:
			h.sample(ctx)
		}
	}
}

func (h *accountRateHub) sample(ctx context.Context) {
	h.mu.Lock()
	if len(h.subs) == 0 {
		h.mu.Unlock()
		return
	}
	subs := make([]*accountRateSubscription, 0, len(h.subs))
	unionSet := make(map[string]struct{})
	for sub := range h.subs {
		subs = append(subs, sub)
		for _, id := range sub.ids {
			unionSet[id] = struct{}{}
		}
	}
	h.mu.Unlock()

	union := make([]string, 0, len(unionSet))
	for id := range unionSet {
		union = append(union, id)
	}
	sort.Strings(union)
	sampledAt := time.Now().Unix()
	rates := make(map[string]storage.AccountRequestRate, len(union))
	// Each storage query is deliberately bounded to 100 ids. A normal account page
	// needs one query; multiple disjoint 100-row pages degrade into bounded chunks.
	for offset := 0; offset < len(union); offset += accountRateMaxIDs {
		end := offset + accountRateMaxIDs
		if end > len(union) {
			end = len(union)
		}
		part, err := h.meter.Rates(ctx, union[offset:end], sampledAt)
		if err != nil {
			log.Printf("account rate sampler degraded: %v", err)
		}
		for id, rate := range part {
			rates[id] = rate
		}
	}
	sequence := h.sequence.Add(1)
	for _, sub := range subs {
		accounts := make(map[string]storage.AccountRequestRate, len(sub.ids))
		for _, id := range sub.ids {
			accounts[id] = rates[id]
		}
		frame := accountRateFrame{
			SampledAt: sampledAt, WindowSeconds: storage.AccountRequestRateWindowSeconds,
			Accounts: accounts, sequence: sequence,
		}
		select {
		case sub.updates <- frame:
		default:
			// Slow clients need only the latest rolling value, never an unbounded queue.
			select {
			case <-sub.updates:
			default:
			}
			select {
			case sub.updates <- frame:
			default:
			}
		}
	}
}

func parseAccountRateIDs(rawValues []string) ([]string, error) {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, raw := range rawValues {
		for _, candidate := range strings.Split(raw, ",") {
			id := strings.TrimSpace(candidate)
			if id == "" {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			if len(ids) >= accountRateMaxIDs {
				return nil, fmt.Errorf("ids supports at most %d unique account ids", accountRateMaxIDs)
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("ids is required")
	}
	return ids, nil
}

func (s *Server) ObserveAccountRequestAttempt(accountID, provider, routeKind string, contexts ...context.Context) {
	if s == nil {
		return
	}
	ctx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	identity := requestClientIdentityFromContext(ctx)
	class := storage.NormalizeAgentClass(string(identity.AgentClass))
	if class == storage.AgentClassUnknown {
		class = accountAgentClassFromContext(ctx)
	}
	clientFamily := storage.NormalizeClientFamily(string(identity.ClientFamily))
	if s.accountRateMeter != nil {
		s.accountRateMeter.ObserveAttemptDimensions(accountID, provider, routeKind, class, clientFamily, time.Time{})
	}
	// The first account transport attempt is the durable logical-arrival
	// boundary. Retries reuse the server-owned event id and therefore leave this
	// shell unchanged while their attempt RPM remains separately observable.
	eventID := usageEventIDFromContext(ctx)
	if eventID == "" {
		eventID = requestIDFromContext(ctx)
	}
	if s.store != nil && eventID != "" && strings.TrimSpace(accountID) != "" {
		arrivalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		err := s.store.RecordAccountRequestRateArrival(arrivalCtx, storage.AccountUsageRateEvent{
			EventID: eventID, AccountID: accountID, AgentClass: class,
			ClientFamily: clientFamily, ClientConfidence: string(identity.Confidence),
			SettlementState: "unsettled",
		})
		cancel()
		if err != nil {
			log.Printf("account rate arrival degraded account=%s: %v", accountID, err)
		}
	}
}

type accountAgentClassContextKey struct{}

func contextWithAccountAgentClass(ctx context.Context, class string) context.Context {
	return context.WithValue(ctx, accountAgentClassContextKey{}, storage.NormalizeAgentClass(class))
}

func accountAgentClassFromContext(ctx context.Context) string {
	if ctx != nil {
		if value, ok := ctx.Value(accountAgentClassContextKey{}).(string); ok {
			return storage.NormalizeAgentClass(value)
		}
	}
	return storage.AgentClassUnknown
}

func requestAccountAgentClass(r *http.Request) string {
	if r == nil {
		return storage.AgentClassUnknown
	}
	explicit := func(value string) (string, bool) {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "1", "yes", "subagent", "sub-agent":
			return storage.AgentClassSubagent, true
		case "false", "0", "no", "root", "main":
			return storage.AgentClassRoot, true
		default:
			if strings.TrimSpace(value) != "" {
				// Codex may send an opaque subagent identifier rather than a bool.
				return storage.AgentClassSubagent, true
			}
			return "", false
		}
	}
	if class, ok := explicit(r.Header.Get("x-openai-subagent")); ok {
		return class
	}
	for _, part := range strings.Split(r.Header.Get("x-anthropic-billing-header"), ";") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if found && strings.EqualFold(strings.TrimSpace(key), "cc_is_subagent") {
			if class, ok := explicit(value); ok {
				return class
			}
		}
	}
	own := strings.TrimSpace(r.Header.Get("thread-id"))
	parent := strings.TrimSpace(r.Header.Get("x-codex-parent-thread-id"))
	forkedFrom := strings.TrimSpace(r.Header.Get("x-codex-forked-from-thread-id"))
	if parent != "" && (own == "" || own != parent) {
		return storage.AgentClassSubagent
	}
	if forkedFrom != "" && (own == "" || own != forkedFrom) {
		return storage.AgentClassSubagent
	}
	if own != "" {
		return storage.AgentClassRoot
	}
	return storage.AgentClassUnknown
}

// doAccountUpstreamAttempt is the business-request transport boundary for
// protocols that use upstream.Request. Probes, OAuth and background quota work keep
// calling the raw client and are therefore deliberately excluded from RPM.
func (s *Server) doAccountUpstreamAttempt(ctx context.Context, req upstream.Request) (*upstream.Response, error) {
	s.ObserveAccountRequestAttempt(req.Account.ID, req.Provider, req.DownstreamPath, ctx)
	return s.upstream.Do(ctx, req)
}

func (s *Server) AccountRequestRates(ctx context.Context, ids []string) (map[string]storage.AccountRequestRate, int64, error) {
	sampledAt := time.Now().Unix()
	if s == nil || s.accountRateMeter == nil {
		unavailable := make(map[string]storage.AccountRequestRate, len(ids))
		for _, id := range ids {
			unavailable[id] = storage.AccountRequestRate{WindowSeconds: storage.AccountRequestRateWindowSeconds, SampledAt: sampledAt, State: "unavailable"}
		}
		return unavailable, sampledAt, nil
	}
	rates := make(map[string]storage.AccountRequestRate, len(ids))
	var queryErr error
	for offset := 0; offset < len(ids); offset += accountRateMaxIDs {
		end := offset + accountRateMaxIDs
		if end > len(ids) {
			end = len(ids)
		}
		part, err := s.accountRateMeter.Rates(ctx, ids[offset:end], sampledAt)
		queryErr = errors.Join(queryErr, err)
		for id, rate := range part {
			rates[id] = rate
		}
	}
	return rates, sampledAt, queryErr
}

func (s *Server) RateMeterStatus() string {
	if s == nil || s.accountRateMeter == nil {
		return "unavailable"
	}
	return s.accountRateMeter.Status()
}

func (s *Server) adminAccountRates(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	ids, err := parseAccountRateIDs(r.URL.Query()["ids"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rates, sampledAt, queryErr := s.AccountRequestRates(r.Context(), ids)
	if queryErr != nil {
		log.Printf("account rates fallback degraded: %v", queryErr)
	}
	writeJSON(w, http.StatusOK, accountRateFrame{
		SampledAt: sampledAt, WindowSeconds: storage.AccountRequestRateWindowSeconds, Accounts: rates,
	})
}

func (s *Server) adminAccountRatesStream(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	ids, err := parseAccountRateIDs(r.URL.Query()["ids"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "retry: 1000\n\n")
	flusher.Flush()

	sub, unsubscribe := s.accountRateHub.Subscribe(ids)
	defer unsubscribe()
	ctx := r.Context()
	handoff, hasHandoff := DeploymentHandoffFromContext(ctx)
	var handoffDone <-chan struct{}
	if hasHandoff {
		handoffDone = handoff.Done
	}
	heartbeat := time.NewTicker(accountRateHeartbeat)
	defer heartbeat.Stop()
	seen := make(map[string]storage.AccountRequestRate, len(ids))
	first := true

	write := func(event string, frame accountRateFrame) bool {
		encoded, marshalErr := json.Marshal(frame)
		if marshalErr != nil {
			return true
		}
		if _, writeErr := fmt.Fprintf(w, "event: %s\nid: %s:%d\ndata: %s\n\n", event, s.accountRateHub.releaseID, frame.sequence, encoded); writeErr != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-handoffDone:
			target := ""
			if handoff.TargetRelease != nil {
				target = handoff.TargetRelease()
			}
			payload, _ := json.Marshal(map[string]any{"release_id": target, "reconnect_after_ms": 0})
			_, _ = fmt.Fprintf(w, "event: handoff\ndata: %s\n\n", payload)
			flusher.Flush()
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case frame := <-sub.updates:
			if first {
				first = false
				for id, rate := range frame.Accounts {
					seen[id] = rate
				}
				if !write("snapshot", frame) {
					return
				}
				continue
			}
			delta := make(map[string]storage.AccountRequestRate)
			for id, rate := range frame.Accounts {
				if previous, exists := seen[id]; !exists || !accountRateValuesEqual(previous, rate) {
					delta[id] = rate
					seen[id] = rate
				}
			}
			if len(delta) == 0 {
				continue
			}
			frame.Accounts = delta
			frame.WindowSeconds = 0
			if !write("delta", frame) {
				return
			}
		}
	}
}
