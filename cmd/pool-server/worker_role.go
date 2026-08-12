package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
)

const activeWorkerLeaseName = "active-worker"

type workerRoleController struct {
	store        *storage.Store
	deployment   *deploymentHandler
	mode         string
	workerSocket string
	activeLink   string
	ownerID      string
	leaseTTL     time.Duration
	pollInterval time.Duration
	startActive  func(context.Context, int64) error

	mu           sync.Mutex
	active       bool
	lease        storage.MaintenanceLease
	activeCancel context.CancelFunc
	nextRenew    time.Time
}

func newWorkerRoleController(
	store *storage.Store,
	deployment *deploymentHandler,
	mode, workerSocket, ownerID string,
	startActive func(context.Context, int64) error,
) (*workerRoleController, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "active" && mode != "standby" {
		return nil, fmt.Errorf("deployment role must be auto, active, or standby")
	}
	if store == nil || deployment == nil {
		return nil, fmt.Errorf("worker role controller requires storage and deployment handler")
	}
	workerSocket = filepath.Clean(strings.TrimSpace(workerSocket))
	activeLink := ""
	if workerSocket != "." && workerSocket != "" {
		activeLink = filepath.Join(filepath.Dir(workerSocket), "active-worker.sock")
	}
	if mode == "auto" && activeLink == "" {
		mode = "active"
	}
	if strings.TrimSpace(ownerID) == "" {
		return nil, fmt.Errorf("worker role owner is empty")
	}
	if startActive == nil {
		return nil, fmt.Errorf("worker role activation callback is nil")
	}
	return &workerRoleController{
		store:        store,
		deployment:   deployment,
		mode:         mode,
		workerSocket: workerSocket,
		activeLink:   activeLink,
		ownerID:      strings.TrimSpace(ownerID),
		leaseTTL:     15 * time.Second,
		pollInterval: 250 * time.Millisecond,
		startActive:  startActive,
	}, nil
}

func (c *workerRoleController) Run(ctx context.Context) {
	c.reconcile(ctx)
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	defer c.deactivate()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.reconcile(ctx)
		}
	}
}

func (c *workerRoleController) reconcile(ctx context.Context) {
	desired := c.desiredActive()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !desired {
		if c.active {
			c.deactivateLocked()
		}
		c.deployment.markStandby()
		return
	}
	if c.active {
		if time.Now().Before(c.nextRenew) {
			return
		}
		// SQLite serializes writes. A bounded historical-migration batch can own the
		// write connection for longer than the old fixed two-second budget even though
		// the process is healthy. Renew early and allow half a lease period so storage
		// maintenance cannot repeatedly demote the only active worker.
		renewCtx, cancel := context.WithTimeout(ctx, leaseRenewTimeout(c.leaseTTL))
		renewed, err := c.store.RenewMaintenanceLease(renewCtx, c.lease, c.leaseTTL)
		cancel()
		if err != nil {
			log.Printf("worker role: active lease renewal failed owner=%s fencing_token=%d: %v", c.ownerID, c.lease.FencingToken, err)
			c.deactivateLocked()
			c.deployment.markStandby()
			return
		}
		c.lease = renewed
		c.nextRenew = time.Now().Add(c.leaseTTL / 4)
		c.deployment.markActive(renewed.FencingToken)
		return
	}

	acquireCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	lease, acquired, err := c.store.AcquireMaintenanceLease(acquireCtx, activeWorkerLeaseName, c.ownerID, c.leaseTTL)
	cancel()
	if err != nil || !acquired {
		c.deployment.markStandby()
		return
	}
	activeCtx, activeCancel := context.WithCancel(ctx)
	if err := c.startActive(activeCtx, lease.FencingToken); err != nil {
		log.Printf("worker role: active runtime start failed owner=%s fencing_token=%d: %v", c.ownerID, lease.FencingToken, err)
		activeCancel()
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.store.ReleaseMaintenanceLease(releaseCtx, lease)
		releaseCancel()
		c.deployment.markStandby()
		return
	}
	c.active = true
	c.lease = lease
	c.activeCancel = activeCancel
	c.nextRenew = time.Now().Add(c.leaseTTL / 4)
	c.deployment.markActive(lease.FencingToken)
}

func leaseRenewTimeout(ttl time.Duration) time.Duration {
	timeout := ttl / 2
	if timeout > 10*time.Second {
		timeout = 10 * time.Second
	}
	return timeout
}

func (c *workerRoleController) desiredActive() bool {
	switch c.mode {
	case "active":
		return true
	case "standby":
		return false
	default:
		return workerLinkTargets(c.activeLink, c.workerSocket)
	}
}

func workerLinkTargets(activeLink, workerSocket string) bool {
	activeLink = strings.TrimSpace(activeLink)
	workerSocket = strings.TrimSpace(workerSocket)
	if activeLink == "" || workerSocket == "" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(activeLink)
	if err != nil {
		return false
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return false
	}
	workerSocket, err = filepath.Abs(workerSocket)
	if err != nil {
		return false
	}
	return filepath.Clean(resolved) == filepath.Clean(workerSocket)
}

func (c *workerRoleController) deactivate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deactivateLocked()
}

func (c *workerRoleController) deactivateLocked() {
	if !c.active {
		return
	}
	c.deployment.beginDraining()
	if c.activeCancel != nil {
		c.activeCancel()
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = c.store.ReleaseMaintenanceLease(releaseCtx, c.lease)
	cancel()
	c.active = false
	c.lease = storage.MaintenanceLease{}
	c.activeCancel = nil
	c.nextRenew = time.Time{}
}
