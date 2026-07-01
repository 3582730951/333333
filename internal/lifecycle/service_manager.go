package lifecycle

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"codex-account-pool/internal/supervisor"
)

// ServiceManager manages lifecycle of external services (Python services)
type ServiceManager struct {
	services     map[string]*ManagedService
	autoStart    bool
	idleTimeout  time.Duration
	restartDelay time.Duration
	mu           sync.RWMutex
}

// ManagedService represents a managed external service
type ManagedService struct {
	Name          string
	Port          int
	Command       string
	WorkDir       string
	Process       *exec.Cmd
	Status        ServiceStatus
	StartedAt     time.Time
	LastUsedAt    time.Time
	ExitedAt      time.Time
	LastError     string
	IdleTimer     *time.Timer
	HealthURL     string
	Done          chan struct{}
	StopRequested bool
	mu            sync.RWMutex
}

// ServiceStatus represents service state
type ServiceStatus string

const (
	ServiceStatusStopped  ServiceStatus = "stopped"
	ServiceStatusStarting ServiceStatus = "starting"
	ServiceStatusRunning  ServiceStatus = "running"
	ServiceStatusStopping ServiceStatus = "stopping"
	ServiceStatusFailed   ServiceStatus = "failed"
)

const defaultServiceRestartDelay = time.Second
const lifecycleServiceModulePrefix = "lifecycle-service"

type ServiceSnapshot struct {
	Name       string        `json:"name"`
	Status     ServiceStatus `json:"status"`
	Port       int           `json:"port"`
	StartedAt  int64         `json:"started_at,omitempty"`
	LastUsedAt int64         `json:"last_used_at,omitempty"`
	ExitedAt   int64         `json:"exited_at,omitempty"`
	LastError  string        `json:"last_error,omitempty"`
}

// Config for service manager
type ServiceManagerConfig struct {
	AutoStart   bool
	IdleTimeout time.Duration // 0 = never stop
}

// NewServiceManager creates a new service manager
func NewServiceManager(config ServiceManagerConfig) *ServiceManager {
	return &ServiceManager{
		services:     make(map[string]*ManagedService),
		autoStart:    config.AutoStart,
		idleTimeout:  config.IdleTimeout,
		restartDelay: defaultServiceRestartDelay,
	}
}

// RegisterService registers a new service
func (sm *ServiceManager) RegisterService(name, command, workDir string, port int, healthURL string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.services[name] = &ManagedService{
		Name:      name,
		Port:      port,
		Command:   command,
		WorkDir:   workDir,
		Status:    ServiceStatusStopped,
		HealthURL: healthURL,
	}
}

func (sm *ServiceManager) HasService(name string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	_, ok := sm.services[name]
	return ok
}

// EnsureRunning ensures a service is running, starting it if necessary
func (sm *ServiceManager) EnsureRunning(ctx context.Context, serviceName string) error {
	sm.mu.RLock()
	svc, exists := sm.services[serviceName]
	sm.mu.RUnlock()

	if !exists {
		return fmt.Errorf("service %s not registered", serviceName)
	}

	svc.mu.Lock()

	// Update last used time
	svc.LastUsedAt = time.Now()

	// If already running, reset idle timer
	if svc.Status == ServiceStatusRunning {
		if sm.idleTimeout > 0 && svc.IdleTimer != nil {
			svc.IdleTimer.Reset(sm.idleTimeout)
		}
		svc.mu.Unlock()
		return nil
	}

	// If starting, wait for it
	if svc.Status == ServiceStatusStarting {
		svc.mu.Unlock()
		return sm.waitForHealthy(ctx, svc)
	}

	// Start the service
	err := sm.startService(ctx, svc)
	svc.mu.Unlock()
	return err
}

// startService starts a service (must be called with svc.mu locked)
func (sm *ServiceManager) startService(ctx context.Context, svc *ManagedService) error {
	now := time.Now()
	svc.Status = ServiceStatusStarting
	svc.StartedAt = now
	svc.LastUsedAt = now
	svc.ExitedAt = time.Time{}
	svc.LastError = ""
	svc.StopRequested = false

	// Create command
	cmd := exec.Command("/bin/bash", "-c", svc.Command)
	if svc.WorkDir != "" {
		cmd.Dir = svc.WorkDir
	}

	// Start process
	if err := cmd.Start(); err != nil {
		svc.Status = ServiceStatusFailed
		svc.LastError = fmt.Sprintf("failed to start: %v", err)
		supervisor.ModuleFailed(serviceModuleName(svc.Name), fmt.Errorf("failed to start: %w", err))
		return fmt.Errorf("failed to start service %s: %w", svc.Name, err)
	}

	svc.Process = cmd
	svc.Done = make(chan struct{})

	// Wait for health check
	svc.mu.Unlock()
	err := sm.waitForHealthy(ctx, svc)
	svc.mu.Lock()

	if err != nil {
		cmd := svc.Process
		done := svc.Done
		stopRequested := svc.StopRequested
		uptime := serviceRunUptime(svc.StartedAt)
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if stopRequested {
			svc.Status = ServiceStatusStopped
			svc.LastError = ""
			supervisor.ModuleStopped(serviceModuleName(svc.Name))
		} else {
			svc.Status = ServiceStatusFailed
			svc.LastError = fmt.Sprintf("health check failed: %v", err)
			supervisor.ModuleFailedWithUptime(serviceModuleName(svc.Name), fmt.Errorf("health check failed: %w", err), uptime)
		}
		svc.ExitedAt = time.Now()
		svc.Process = nil
		svc.Done = nil
		svc.StopRequested = false
		if done != nil {
			close(done)
		}
		return fmt.Errorf("service %s health check failed: %w", svc.Name, err)
	}

	svc.Status = ServiceStatusRunning
	supervisor.ModuleStarted(serviceModuleName(svc.Name))

	// Start idle timer if configured
	if sm.idleTimeout > 0 {
		serviceName := svc.Name
		svc.IdleTimer = time.AfterFunc(sm.idleTimeout, func() {
			defer supervisor.Recover(serviceModuleName(serviceName) + ":idle-timeout")
			sm.stopIdleService(serviceName)
		})
	}

	// Monitor process in background
	go func() {
		defer supervisor.Recover("lifecycle-service-monitor")
		sm.monitorProcess(svc)
	}()

	return nil
}

// waitForHealthy waits for a service to become healthy
func (sm *ServiceManager) waitForHealthy(ctx context.Context, svc *ManagedService) error {
	if svc.HealthURL == "" {
		// No health check, just wait a bit
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		for {
			if serviceStartStopped(svc) {
				return fmt.Errorf("service start stopped")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			case <-ticker.C:
			}
		}
	}

	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	client := &http.Client{Timeout: 2 * time.Second}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-timeout:
			return fmt.Errorf("health check timeout after 30s")

		case <-ticker.C:
			if serviceStartStopped(svc) {
				return fmt.Errorf("service start stopped")
			}
			resp, err := client.Get(svc.HealthURL)
			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				return nil
			}
			if resp != nil {
				resp.Body.Close()
			}
		}
	}
}

func serviceStartStopped(svc *ManagedService) bool {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	return svc.StopRequested || svc.Process == nil
}

func serviceModuleName(name string) string {
	if name == "" {
		return lifecycleServiceModulePrefix
	}
	return fmt.Sprintf("%s:%s", lifecycleServiceModulePrefix, name)
}

// monitorProcess monitors a service process
func (sm *ServiceManager) monitorProcess(svc *ManagedService) {
	svc.mu.RLock()
	cmd := svc.Process
	done := svc.Done
	svc.mu.RUnlock()

	if cmd == nil {
		return
	}

	// Wait for process to exit
	err := cmd.Wait()

	var (
		shouldRestart bool
		serviceName   string
		moduleName    string
		failure       error
		uptime        time.Duration
		stopped       bool
	)

	svc.mu.Lock()

	if svc.Process != cmd {
		svc.mu.Unlock()
		if done != nil {
			close(done)
		}
		return
	}

	// Stop idle timer
	if svc.IdleTimer != nil {
		svc.IdleTimer.Stop()
	}

	// Update status
	svc.ExitedAt = time.Now()
	uptime = serviceRunUptime(svc.StartedAt)
	if svc.StopRequested {
		svc.Status = ServiceStatusStopped
		svc.LastError = ""
		stopped = true
	} else if err != nil {
		svc.Status = ServiceStatusFailed
		svc.LastError = fmt.Sprintf("process exited: %v", err)
		failure = fmt.Errorf("service process exited: %w", err)
		shouldRestart = sm.autoStart
	} else {
		svc.Status = ServiceStatusFailed
		svc.LastError = "process exited unexpectedly"
		failure = fmt.Errorf("service process exited unexpectedly")
		shouldRestart = sm.autoStart
	}
	serviceName = svc.Name
	moduleName = serviceModuleName(svc.Name)
	svc.Process = nil
	svc.Done = nil
	svc.StopRequested = false
	svc.mu.Unlock()

	if done != nil {
		close(done)
	}
	switch {
	case stopped:
		supervisor.ModuleStopped(moduleName)
	case shouldRestart:
		supervisor.ModuleRestartingWithUptime(moduleName, failure.Error(), uptime, sm.effectiveRestartDelay())
		sm.scheduleRestart(serviceName)
	case failure != nil:
		supervisor.ModuleFailedWithUptime(moduleName, failure, uptime)
	}
}

func serviceRunUptime(startedAt time.Time) time.Duration {
	if startedAt.IsZero() {
		return 0
	}
	return time.Since(startedAt).Round(time.Millisecond)
}

func (sm *ServiceManager) effectiveRestartDelay() time.Duration {
	delay := sm.restartDelay
	if delay <= 0 {
		return defaultServiceRestartDelay
	}
	return delay
}

func (sm *ServiceManager) scheduleRestart(serviceName string) {
	go func() {
		defer supervisor.Recover("lifecycle-service-restart")

		delay := sm.effectiveRestartDelay()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C

		if !sm.shouldRestart(serviceName) {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sm.EnsureRunning(ctx, serviceName); err != nil {
			sm.recordRestartError(serviceName, err)
		}
	}()
}

func (sm *ServiceManager) shouldRestart(serviceName string) bool {
	if !sm.autoStart {
		return false
	}

	sm.mu.RLock()
	svc, exists := sm.services[serviceName]
	sm.mu.RUnlock()
	if !exists {
		return false
	}

	svc.mu.RLock()
	defer svc.mu.RUnlock()
	return !svc.StopRequested &&
		svc.Status != ServiceStatusRunning &&
		svc.Status != ServiceStatusStarting &&
		svc.Status != ServiceStatusStopping
}

func (sm *ServiceManager) recordRestartError(serviceName string, err error) {
	sm.mu.RLock()
	svc, exists := sm.services[serviceName]
	sm.mu.RUnlock()
	if !exists {
		return
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.Status != ServiceStatusRunning && svc.Status != ServiceStatusStarting {
		svc.Status = ServiceStatusFailed
	}
	failure := fmt.Errorf("restart failed: %w", err)
	svc.LastError = failure.Error()
	svc.ExitedAt = time.Now()
	supervisor.ModuleFailed(serviceModuleName(serviceName), failure)
}

// StopService stops a service
func (sm *ServiceManager) StopService(serviceName string) error {
	sm.mu.RLock()
	svc, exists := sm.services[serviceName]
	sm.mu.RUnlock()

	if !exists {
		return fmt.Errorf("service %s not registered", serviceName)
	}

	svc.mu.Lock()

	if svc.Status != ServiceStatusRunning && svc.Status != ServiceStatusStarting && svc.Status != ServiceStatusStopping {
		svc.StopRequested = true
		svc.Status = ServiceStatusStopped
		svc.LastError = ""
		svc.mu.Unlock()
		supervisor.ModuleStopped(serviceModuleName(serviceName))
		return nil
	}

	svc.Status = ServiceStatusStopping
	svc.StopRequested = true

	// Stop idle timer
	if svc.IdleTimer != nil {
		svc.IdleTimer.Stop()
		svc.IdleTimer = nil
	}

	if svc.Process == nil || svc.Process.Process == nil {
		svc.Status = ServiceStatusStopped
		svc.LastError = ""
		svc.mu.Unlock()
		supervisor.ModuleStopped(serviceModuleName(serviceName))
		return nil
	}

	cmd := svc.Process
	done := svc.Done

	// Send SIGTERM
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// Process might be already dead
		svc.LastError = fmt.Sprintf("signal failed: %v", err)
	}
	svc.mu.Unlock()

	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				return fmt.Errorf("service %s did not stop after kill", serviceName)
			}
		}
	}
	return nil
}

// stopIdleService stops an idle service
func (sm *ServiceManager) stopIdleService(serviceName string) {
	sm.mu.RLock()
	svc, exists := sm.services[serviceName]
	sm.mu.RUnlock()

	if !exists {
		return
	}

	svc.mu.RLock()
	// Check if really idle
	if time.Since(svc.LastUsedAt) < sm.idleTimeout {
		svc.mu.RUnlock()
		return
	}
	svc.mu.RUnlock()

	// Stop the service
	sm.StopService(serviceName)
}

// GetStatus returns the status of a service
func (sm *ServiceManager) GetStatus(serviceName string) (ServiceStatus, error) {
	sm.mu.RLock()
	svc, exists := sm.services[serviceName]
	sm.mu.RUnlock()

	if !exists {
		return ServiceStatusStopped, fmt.Errorf("service %s not registered", serviceName)
	}

	svc.mu.RLock()
	defer svc.mu.RUnlock()

	return svc.Status, nil
}

func (sm *ServiceManager) GetSnapshot(serviceName string) (ServiceSnapshot, error) {
	sm.mu.RLock()
	svc, exists := sm.services[serviceName]
	sm.mu.RUnlock()

	if !exists {
		return ServiceSnapshot{}, fmt.Errorf("service %s not registered", serviceName)
	}

	return serviceSnapshot(svc), nil
}

// ListServices returns all registered services and their status
func (sm *ServiceManager) ListServices() map[string]ServiceStatus {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make(map[string]ServiceStatus)
	for name, svc := range sm.services {
		svc.mu.RLock()
		result[name] = svc.Status
		svc.mu.RUnlock()
	}

	return result
}

func (sm *ServiceManager) ListServiceSnapshots() map[string]ServiceSnapshot {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make(map[string]ServiceSnapshot)
	for name, svc := range sm.services {
		result[name] = serviceSnapshot(svc)
	}

	return result
}

func serviceSnapshot(svc *ManagedService) ServiceSnapshot {
	svc.mu.RLock()
	defer svc.mu.RUnlock()

	out := ServiceSnapshot{
		Name:      svc.Name,
		Status:    svc.Status,
		Port:      svc.Port,
		LastError: svc.LastError,
	}
	if !svc.StartedAt.IsZero() {
		out.StartedAt = svc.StartedAt.Unix()
	}
	if !svc.LastUsedAt.IsZero() {
		out.LastUsedAt = svc.LastUsedAt.Unix()
	}
	if !svc.ExitedAt.IsZero() {
		out.ExitedAt = svc.ExitedAt.Unix()
	}
	return out
}

// StopAll stops all running services
func (sm *ServiceManager) StopAll() error {
	sm.mu.RLock()
	names := make([]string, 0, len(sm.services))
	for name := range sm.services {
		names = append(names, name)
	}
	sm.mu.RUnlock()

	var lastErr error
	for _, name := range names {
		if err := sm.StopService(name); err != nil {
			lastErr = err
		}
	}

	return lastErr
}
