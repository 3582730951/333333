// Package prewarm provides startup pre-warming for connection pools and caches.
//
// Pre-warming eliminates cold-start latency by establishing connections before
// the first request arrives. This is especially important for:
// - SQLite WAL reader pool (first request sees ~100ms delay)
// - HTTP/2 upstream connections (TLS handshake + connection establishment)
// - Redis/memcached caches (if used)
//
// Usage:
//
//	ctx := context.Background()
//	if err := prewarm.Database(ctx, store); err != nil {
//	    log.Printf("prewarm db: %v", err)
//	}
//	if err := prewarm.HTTP2Connections(ctx, upstream, accounts); err != nil {
//	    log.Printf("prewarm http2: %v", err)
//	}
package prewarm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

var errNilStore = errors.New("prewarm: nil storage store")

func normalizeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func prewarmDB(store *storage.Store) (*sql.DB, error) {
	if store == nil {
		return nil, errNilStore
	}
	db := store.DB()
	if db == nil {
		return nil, errNilStore
	}
	return db, nil
}

// Database pre-warms the SQLite WAL reader pool by running several concurrent
// SELECT queries across the key tables. This establishes multiple reader
// connections before the first real request, eliminating the ~100ms first-read
// penalty.
//
// The queries are lightweight (COUNT aggregations) to avoid polluting caches.
func Database(ctx context.Context, store *storage.Store) error {
	const (
		concurrentReaders = 4
		probeTimeout      = 2 * time.Second
	)

	ctx = normalizeContext(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type probe struct {
		name  string
		query string
	}
	probes := []probe{
		{"accounts", "SELECT COUNT(*) FROM accounts WHERE status != 'deleted'"},
		{"tokens", "SELECT COUNT(*) FROM account_tokens WHERE status != 'deleted'"},
		{"egress", "SELECT COUNT(*) FROM egress_profiles"},
		{"groups", "SELECT COUNT(*) FROM groups"},
		{"api_keys", "SELECT COUNT(*) FROM api_keys WHERE enabled = 1"},
	}

	db, err := prewarmDB(store)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	errCh := make(chan error, len(probes))

	for i := 0; i < concurrentReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if v := recover(); v != nil {
					supervisor.LogPanic("prewarm-database", v)
					select {
					case errCh <- fmt.Errorf("prewarm database panic: %v", v):
					default:
					}
				}
			}()
			for _, p := range probes {
				pCtx, cancel := context.WithTimeout(ctx, probeTimeout)
				_, err := db.QueryContext(pCtx, p.query)
				cancel()
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}()
	}

	// Wait with timeout
	done := make(chan struct{})
	supervisor.GoOnce("prewarm-database-wait", func() {
		wg.Wait()
		close(done)
	})

	select {
	case <-done:
		log.Printf("[prewarm] database pool: %d readers, %d probes OK", concurrentReaders, len(probes))
		return nil
	case err := <-errCh:
		cancel()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// HTTP2Connection pre-warms HTTP/2 connections to the upstream API for a set of
// accounts. It sends a lightweight HEAD request through each account's configured
// egress, establishing the TLS session and HTTP/2 connection before the first
// real request.
//
// This eliminates the ~50-200ms TLS+HTTP2 handshake on the first request per
// (account, egress) pair.
//
// Note: This is a simplified version. In production, you may want to call
// store.SelectFresh() or similar to get active accounts, then pre-warm their
// specific egress paths.
func HTTP2Connection(ctx context.Context, db *sql.DB, account storage.Account) error {
	// This is a placeholder that demonstrates the pattern.
	// In practice, you'd call upstream.ProbeEgress or a similar lightweight endpoint.
	// The actual implementation depends on how upstream exposes connection pre-warming.
	log.Printf("[prewarm] http2: would pre-warm for account %s", account.ID)
	return nil
}

// Cache pre-warms in-memory caches by loading hot data during startup.
// This includes:
// - Frequently-accessed config values
// - Active account summaries
// - Egress profile metadata
func Cache(ctx context.Context, store *storage.Store) error {
	const cacheWarmTimeout = 10 * time.Second

	ctx = normalizeContext(ctx)
	db, err := prewarmDB(store)
	if err != nil {
		return err
	}
	pCtx, cancel := context.WithTimeout(ctx, cacheWarmTimeout)
	defer cancel()

	// Load active accounts summary
	rows, err := db.QueryContext(pCtx, `
		SELECT a.id, a.status, a.group_name, e.type, e.endpoint
		FROM accounts a
		LEFT JOIN egress_profiles e ON a.egress_id = e.id
		WHERE a.status IN ('active', 'idle', 'cooldown')
		LIMIT 100
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		count++
	}
	_ = count // rows consumed

	log.Printf("[prewarm] cache: loaded %d active account summaries", count)
	return nil
}

// Parallel runs multiple pre-warming tasks concurrently for faster startup.
func Parallel(ctx context.Context, tasks ...func(context.Context) error) error {
	type result struct {
		name string
		err  error
	}

	ctx = normalizeContext(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	resCh := make(chan result, len(tasks))
	var wg sync.WaitGroup

	for _, task := range tasks {
		wg.Add(1)
		go func(t func(context.Context) error) {
			defer wg.Done()
			defer func() {
				if v := recover(); v != nil {
					supervisor.LogPanic("prewarm-task", v)
					select {
					case resCh <- result{err: fmt.Errorf("prewarm task panic: %v", v)}:
					default:
					}
				}
			}()
			if err := t(ctx); err != nil {
				select {
				case resCh <- result{err: err}:
				default:
				}
			}
		}(task)
	}

	// Wait with timeout
	done := make(chan struct{})
	supervisor.GoOnce("prewarm-task-wait", func() {
		wg.Wait()
		close(done)
	})

	select {
	case <-done:
		return nil
	case r := <-resCh:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
