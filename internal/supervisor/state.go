package supervisor

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	StatusRunning    = "running"
	StatusRestarting = "restarting"
	StatusPanic      = "panic"
	StatusFailed     = "failed"
	StatusStopped    = "stopped"
)

// ModuleState is the latest known health state for a supervised module.
type ModuleState struct {
	Name                 string `json:"name"`
	Status               string `json:"status"`
	LastStartUnix        int64  `json:"last_start_unix,omitempty"`
	LastEventUnix        int64  `json:"last_event_unix,omitempty"`
	NextRestartUnix      int64  `json:"next_restart_unix,omitempty"`
	RestartCount         int64  `json:"restart_count"`
	PanicCount           int64  `json:"panic_count"`
	UnexpectedExitCount  int64  `json:"unexpected_exit_count"`
	UptimeMillis         int64  `json:"uptime_millis,omitempty"`
	LastUptimeMillis     int64  `json:"last_uptime_millis,omitempty"`
	RestartBackoffMillis int64  `json:"restart_backoff_millis,omitempty"`
	LastMessage          string `json:"last_message,omitempty"`
	LastPanic            string `json:"last_panic,omitempty"`
}

var moduleStates = newModuleStateStore()

type moduleStateStore struct {
	mu      sync.Mutex
	modules map[string]ModuleState
}

func newModuleStateStore() *moduleStateStore {
	return &moduleStateStore{modules: make(map[string]ModuleState)}
}

// ModuleStates returns module states sorted by health priority and then name.
func ModuleStates() []ModuleState {
	return moduleStates.snapshot()
}

// ModuleStarted records a manually-managed module as running. Use this for process
// roots that should be visible in /admin/system but are not restart loops.
func ModuleStarted(name string) {
	markModuleStarted(name)
}

// ModuleStopped records a manually-managed module as stopped.
func ModuleStopped(name string) {
	markModuleStopped(name)
}

// ModuleFailed records a manually-managed module failure without implying an in-process
// restart. Long-running restart loops should use Go/GoWithOptions instead.
func ModuleFailed(name string, err error) {
	ModuleFailedWithUptime(name, err, 0)
}

// ModuleFailedWithUptime records a manually-managed module failure with the
// duration of the failed run when the caller can measure it.
func ModuleFailedWithUptime(name string, err error, uptime time.Duration) {
	message := "module failed"
	if err != nil {
		message = fmt.Sprintf("module failed: %v", err)
	}
	if uptime < 0 {
		uptime = 0
	}
	event := Event{
		Type:         "failed",
		Module:       normalizeOptions(Options{Name: name}).Name,
		Message:      message,
		UptimeMillis: uptime.Milliseconds(),
	}
	recordEvent(event)
	markModuleFailed(name, event)
}

// ModuleRestarting records a manually-managed module that failed and is scheduled
// for an external restart. Use this for child processes whose restart is driven by
// a process manager instead of supervisor.Go.
func ModuleRestarting(name, message string, backoff time.Duration) {
	ModuleRestartingWithUptime(name, message, 0, backoff)
}

// ModuleRestartingWithUptime records a manually-managed module restart with the
// duration of the failed run when the caller can measure it.
func ModuleRestartingWithUptime(name, message string, uptime, backoff time.Duration) {
	opts := normalizeOptions(Options{Name: name})
	if message == "" {
		message = "module exited unexpectedly; restarting"
	}
	if uptime < 0 {
		uptime = 0
	}
	if backoff < 0 {
		backoff = 0
	}
	event := Event{
		Type:          "unexpected_exit",
		Module:        opts.Name,
		Message:       message,
		UptimeMillis:  uptime.Milliseconds(),
		BackoffMillis: backoff.Milliseconds(),
	}
	recordEvent(event)
	markModuleRestarting(opts.Name, event)
}

func markModuleStarted(name string) {
	now := time.Now().Unix()
	moduleStates.update(name, func(state ModuleState) ModuleState {
		state.Status = StatusRunning
		state.LastStartUnix = now
		state.LastEventUnix = now
		state.NextRestartUnix = 0
		state.RestartBackoffMillis = 0
		state.LastMessage = "module running"
		return state
	})
}

func markModuleRestarting(name string, event Event) {
	now := event.TimeUnix
	if now == 0 {
		now = time.Now().Unix()
	}
	moduleStates.update(name, func(state ModuleState) ModuleState {
		state.Status = StatusRestarting
		state.LastEventUnix = now
		state.NextRestartUnix = now + event.BackoffMillis/1000
		state.LastUptimeMillis = event.UptimeMillis
		state.RestartBackoffMillis = event.BackoffMillis
		state.RestartCount++
		state.LastMessage = event.Message
		state.LastPanic = event.Panic
		if event.Type == "panic_restart" {
			state.PanicCount++
		}
		if event.Type == "unexpected_exit" {
			state.UnexpectedExitCount++
		}
		return state
	})
}

func markModuleFailed(name string, event Event) {
	now := event.TimeUnix
	if now == 0 {
		now = time.Now().Unix()
	}
	moduleStates.update(name, func(state ModuleState) ModuleState {
		state.Status = StatusFailed
		state.LastEventUnix = now
		state.NextRestartUnix = 0
		state.RestartBackoffMillis = 0
		if event.UptimeMillis > 0 {
			state.LastUptimeMillis = event.UptimeMillis
		}
		state.UnexpectedExitCount++
		state.LastMessage = event.Message
		return state
	})
}

func markModulePanic(name string, panicVal any) {
	now := time.Now().Unix()
	moduleStates.update(name, func(state ModuleState) ModuleState {
		state.Status = StatusPanic
		state.LastEventUnix = now
		state.PanicCount++
		state.LastPanic = fmt.Sprint(panicVal)
		state.LastMessage = fmt.Sprintf("module panic: %v", panicVal)
		return state
	})
}

func markModuleStopped(name string) {
	now := time.Now().Unix()
	moduleStates.update(name, func(state ModuleState) ModuleState {
		state.Status = StatusStopped
		state.LastEventUnix = now
		state.NextRestartUnix = 0
		state.RestartBackoffMillis = 0
		state.LastMessage = "module stopped"
		return state
	})
}

func clearModuleStatesForTest() {
	moduleStates.clear()
}

func (s *moduleStateStore) update(name string, mutate func(ModuleState) ModuleState) {
	name = normalizeOptions(Options{Name: name}).Name
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.modules[name]
	state.Name = name
	if state.Status == "" {
		state.Status = StatusStopped
	}
	s.modules[name] = mutate(state)
}

func (s *moduleStateStore) snapshot() []ModuleState {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	out := make([]ModuleState, 0, len(s.modules))
	for _, state := range s.modules {
		if state.Status == StatusRunning && state.LastStartUnix > 0 {
			state.UptimeMillis = (now - state.LastStartUnix) * 1000
		}
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := stateRank(out[i].Status), stateRank(out[j].Status)
		if left != right {
			return left < right
		}
		if out[i].LastEventUnix != out[j].LastEventUnix {
			return out[i].LastEventUnix > out[j].LastEventUnix
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *moduleStateStore) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modules = make(map[string]ModuleState)
}

func stateRank(status string) int {
	switch status {
	case StatusRestarting, StatusPanic, StatusFailed:
		return 0
	case StatusStopped:
		return 1
	case StatusRunning:
		return 2
	default:
		return 3
	}
}
