package lifecycle

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

// Scheduler manages periodic health checks and renewals
type Scheduler struct {
	store   *storage.Store
	checker lifecycleHealthChecker

	mu       sync.RWMutex
	running  bool
	stopChan chan struct{}

	// Statistics
	stats Statistics

	// batchSize and concurrency cap how many accounts a single health-check cycle
	// processes at once, so a large pool cannot OOM a low-RAM VPS. Loaded from the
	// settings table on each cycle (defaults: 200 / 10).
	batchSize   int
	concurrency int
}

type lifecycleHealthChecker interface {
	CheckAccount(context.Context, storage.Account) HealthStatus
	BatchCheckAccountsN(context.Context, []storage.Account, int) []HealthStatus
}

// Statistics tracks lifecycle metrics
type Statistics struct {
	TotalChecks   int64 `json:"total_checks"`
	AliveCount    int64 `json:"alive_count"`
	DeadCount     int64 `json:"dead_count"`
	LastCheckTime int64 `json:"last_check_time"`
}

// NewScheduler creates a lifecycle scheduler
func NewScheduler(store *storage.Store) *Scheduler {
	return &Scheduler{
		store:       store,
		checker:     NewHealthChecker(store),
		stopChan:    make(chan struct{}),
		batchSize:   200,
		concurrency: 10,
	}
}

// Start begins periodic health checks
func (s *Scheduler) Start(ctx context.Context, interval time.Duration) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	stopChan := make(chan struct{})
	s.stopChan = stopChan
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.stopChan == stopChan {
			s.running = false
		}
		s.mu.Unlock()
	}()

	log.Printf("[lifecycle] Scheduler started with interval %v", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run initial check immediately
	if ctx.Err() == nil {
		s.runHealthCheckCycleSafely(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopChan:
			return
		case <-ticker.C:
			s.runHealthCheckCycleSafely(ctx)
		}
	}
}

// Stop halts the scheduler
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}

	stopChan := s.stopChan
	s.running = false
	s.mu.Unlock()

	if stopChan != nil {
		close(stopChan)
	}
	log.Println("[lifecycle] Scheduler stopped")
}

func (s *Scheduler) runHealthCheckCycleSafely(ctx context.Context) {
	defer func() {
		if v := recover(); v != nil {
			supervisor.LogPanic("registration-lifecycle-scheduler", v)
		}
	}()
	s.runHealthCheckCycle(ctx)
}

// runHealthCheckCycle performs one full health check cycle, processing accounts in
// bounded batches so a large pool cannot hold the whole list + all its in-flight
// HTTP responses in memory at once on a low-RAM VPS.
func (s *Scheduler) runHealthCheckCycle(ctx context.Context) {
	log.Println("[lifecycle] Starting health check cycle...")

	// Reload the batch/concurrency knobs from settings so an operator can tune
	// memory pressure from the SettingsV2 page without a restart.
	s.refreshMemoryKnobs(ctx)
	batchSize := s.batchSize
	if batchSize < 10 {
		batchSize = 10
	}
	concurrency := s.concurrency
	if concurrency < 1 {
		concurrency = 1
	}

	aliveCount := int64(0)
	deadCount := int64(0)
	offset := 0
	total := 0
	for {
		if ctx.Err() != nil {
			break
		}
		// Pull one page at a time. The previous implementation sliced bounded
		// batches only after ListAccounts had already materialized the complete
		// pool, defeating the memory cap on large installations.
		batch, count, err := s.store.ListAccountsPage(ctx, batchSize, offset, "", "")
		if err != nil {
			log.Printf("[lifecycle] Failed to list account batch at offset %d: %v", offset, err)
			return
		}
		if offset == 0 {
			total = count
			if total == 0 {
				log.Println("[lifecycle] No accounts to check")
				return
			}
			log.Printf("[lifecycle] Checking %d accounts (batch=%d concurrency=%d)...", total, batchSize, concurrency)
		}
		if len(batch) == 0 {
			break
		}
		results := s.checker.BatchCheckAccountsN(ctx, batch, concurrency)

		for i, result := range results {
			account := batch[i]

			s.mu.Lock()
			s.stats.TotalChecks++
			s.mu.Unlock()

			if result.Alive {
				aliveCount++
				log.Printf("[lifecycle] Account %s: ALIVE (%.0fms)", account.ID, float64(result.ResponseMS))
			} else {
				deadCount++
				log.Printf("[lifecycle] Account %s (provider=%s): DEAD - %s",
					account.ID, account.Provider, result.ErrorReason)

				// Mark account as quarantined
				_ = s.store.SetAccountQuarantine(ctx, account.ID,
					time.Now().Add(24*time.Hour).Unix(), result.ErrorReason)
			}
		}
		offset += len(batch)
		if len(batch) < batchSize || offset >= total {
			break
		}
	}

	// Update statistics
	s.mu.Lock()
	s.stats.AliveCount = aliveCount
	s.stats.DeadCount = deadCount
	s.stats.LastCheckTime = time.Now().Unix()
	s.mu.Unlock()

	log.Printf("[lifecycle] Check cycle complete: %d alive, %d dead", aliveCount, deadCount)
}

// refreshMemoryKnobs reloads batchSize/concurrency from the settings table so an
// operator can tune lifecycle memory pressure from the admin UI at runtime.
func (s *Scheduler) refreshMemoryKnobs(ctx context.Context) {
	if v, ok, _ := s.store.GetSetting(ctx, "lifecycle_batch_size"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 10 {
			s.batchSize = n
		}
	}
	if v, ok, _ := s.store.GetSetting(ctx, "lifecycle_concurrency"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 1 {
			s.concurrency = n
		}
	}
}

// GetStatistics returns current lifecycle statistics
func (s *Scheduler) GetStatistics() Statistics {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stats
}

// CheckAccountNow performs an immediate health check on a specific account
func (s *Scheduler) CheckAccountNow(ctx context.Context, accountID string) (HealthStatus, error) {
	account, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return HealthStatus{}, err
	}

	return s.checker.CheckAccount(ctx, account), nil
}

// ExportStatistics exports statistics as JSON
func (s *Scheduler) ExportStatistics() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return json.Marshal(s.stats)
}
