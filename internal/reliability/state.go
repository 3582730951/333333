package reliability

import (
	"strings"
	"sync"
	"time"
)

// WorkingState is the server-side, per-conversation continuity record. It is the
// spec's working_state verbatim: the durable summary the gateway re-injects on every
// turn so a long conversation keeps its objective, confirmed facts, file/command
// state, risks and open items even when the downstream client (using
// previous_response_id, or aggressive its own truncation) stops echoing full history.
//
// It is populated deterministically from the request's ground-truth Facts plus the
// task/risk Classification — never from the model's prose — so it cannot itself drift
// or be poisoned by a hallucinated prior turn.
type WorkingState struct {
	Objective          string   `json:"objective"`
	TaskType           string   `json:"task_type"`
	RiskLevel          string   `json:"risk_level"`
	ConfirmedFacts     []string `json:"confirmed_facts"`
	Assumptions        []string `json:"assumptions"`
	Decisions          []string `json:"decisions"`
	FilesSeen          []string `json:"files_seen"`
	FilesChanged       []string `json:"files_changed"`
	CommandsRun        []string `json:"commands_run"`
	ToolResults        []string `json:"tool_results"`
	OpenQuestions      []string `json:"open_questions"`
	Risks              []string `json:"risks"`
	NextStep           string   `json:"next_step"`
	VerificationStatus string   `json:"verification_status"`
}

// maxStateList bounds each accumulating slice so a very long conversation cannot grow
// the injected state without limit (keeping the most recent entries).
const maxStateList = 40

// MergeFacts folds one turn's ground-truth Facts and Classification into the state,
// keeping it bounded and de-duplicated. It is the only writer of confirmed,
// evidence-backed fields; subjective fields (assumptions/decisions/risks/questions)
// are left for explicit callers so the gateway never fabricates them.
func (s *WorkingState) MergeFacts(f Facts, c Classification) {
	if s.Objective == "" {
		s.Objective = excerpt(f.FirstUserText, 280)
	}
	if c.Task != "" {
		s.TaskType = string(c.Task)
	}
	if c.Risk != "" {
		s.RiskLevel = string(c.Risk)
	}
	s.CommandsRun = mergeList(s.CommandsRun, f.Commands)
	s.FilesSeen = mergeList(s.FilesSeen, f.FilesSeen)
	for _, t := range f.Tools {
		s.ConfirmedFacts = mergeList(s.ConfirmedFacts, []string{"tool invoked: " + t})
	}
	// Verification status reflects only what real tool evidence supports.
	switch {
	case f.HasTestEvidence:
		s.VerificationStatus = "tests_run"
	case f.HasBuildEvidence || f.HasLintEvidence:
		if s.VerificationStatus != "tests_run" {
			s.VerificationStatus = "build_or_lint_run"
		}
	case s.VerificationStatus == "":
		s.VerificationStatus = "unknown"
	}
}

func mergeList(dst, src []string) []string {
	for _, v := range src {
		dst = appendUnique(dst, v)
	}
	if len(dst) > maxStateList {
		dst = append([]string(nil), dst[len(dst)-maxStateList:]...)
	}
	return dst
}

func excerpt(s string, n int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Store is an in-memory, TTL'd, size-bounded map of conversation key -> WorkingState.
// It mirrors the existing in-memory stores in the api package (oauthStore /
// loginThrottle): server-side, process-local, lost on restart. That is an accepted
// trade-off for a single-instance relay — the state is also re-derivable from each
// request's own tool history, so a cold cache degrades to "rebuild from this turn"
// rather than losing correctness. A multi-instance deployment would back this with
// the shared store; the api layer depends only on this small surface.
type Store struct {
	mu  sync.Mutex
	ttl time.Duration
	max int
	m   map[string]*stateEntry
}

type stateEntry struct {
	state   WorkingState
	updated time.Time
}

// NewStore builds a working-state store. ttl<=0 disables expiry; max<=0 disables the
// size bound.
func NewStore(ttl time.Duration, max int) *Store {
	return &Store{ttl: ttl, max: max, m: make(map[string]*stateEntry)}
}

// Get returns a COPY of the state for key (so callers cannot mutate the stored value
// without Update). ok=false when absent or expired.
func (s *Store) Get(key string) (WorkingState, bool) {
	if s == nil || key == "" {
		return WorkingState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok {
		return WorkingState{}, false
	}
	if s.expired(e) {
		delete(s.m, key)
		return WorkingState{}, false
	}
	return e.state.clone(), true
}

// Update applies fn to the stored state for key (creating it if absent) under the
// lock, then persists the result and refreshes its TTL. A no-op for an empty key.
func (s *Store) Update(key string, fn func(*WorkingState)) WorkingState {
	if s == nil || key == "" {
		var ws WorkingState
		if fn != nil {
			fn(&ws)
		}
		return ws
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	e, ok := s.m[key]
	if !ok || s.expired(e) {
		e = &stateEntry{}
		s.m[key] = e
	}
	if fn != nil {
		fn(&e.state)
	}
	e.updated = time.Now()
	s.evictLocked()
	return e.state.clone()
}

func (s *Store) expired(e *stateEntry) bool {
	return s.ttl > 0 && time.Since(e.updated) > s.ttl
}

// gcLocked drops expired entries (cheap; called on the write path).
func (s *Store) gcLocked() {
	if s.ttl <= 0 {
		return
	}
	for k, e := range s.m {
		if s.expired(e) {
			delete(s.m, k)
		}
	}
}

// evictLocked enforces the size bound by removing the least-recently-updated entries.
func (s *Store) evictLocked() {
	if s.max <= 0 || len(s.m) <= s.max {
		return
	}
	for len(s.m) > s.max {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, e := range s.m {
			if first || e.updated.Before(oldest) {
				oldestKey, oldest, first = k, e.updated, false
			}
		}
		delete(s.m, oldestKey)
	}
}

func (w WorkingState) clone() WorkingState {
	w.ConfirmedFacts = cloneSlice(w.ConfirmedFacts)
	w.Assumptions = cloneSlice(w.Assumptions)
	w.Decisions = cloneSlice(w.Decisions)
	w.FilesSeen = cloneSlice(w.FilesSeen)
	w.FilesChanged = cloneSlice(w.FilesChanged)
	w.CommandsRun = cloneSlice(w.CommandsRun)
	w.ToolResults = cloneSlice(w.ToolResults)
	w.OpenQuestions = cloneSlice(w.OpenQuestions)
	w.Risks = cloneSlice(w.Risks)
	return w
}

func cloneSlice(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return append([]string(nil), s...)
}
