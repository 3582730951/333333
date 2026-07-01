package payment

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// ProxyPool manages a rotating pool of proxies for PayPal automation.
// Each proxy has:
//   - Cooldown period (avoid reusing too frequently)
//   - Country matching (JP for PayPal)
//   - Usage tracking (distribute load evenly)
type ProxyPool struct {
	proxies    []*ProxyEntry
	mu         sync.RWMutex
	cooldown   time.Duration // minimum time between uses of same proxy
	maxRetries int           // max consecutive failures before marking proxy dead
}

// ProxyEntry represents a single proxy with metadata.
type ProxyEntry struct {
	URL          string
	Country      string // ISO country code (e.g., "JP", "US")
	LastUsed     time.Time
	UseCount     int
	FailureCount int
	Dead         bool
}

// NewProxyPool creates a proxy pool.
func NewProxyPool(proxies []string, countries []string, cooldown time.Duration) *ProxyPool {
	entries := make([]*ProxyEntry, len(proxies))
	for i, proxy := range proxies {
		country := "JP" // default to JP (PayPal requirement)
		if i < len(countries) {
			country = countries[i]
		}
		entries[i] = &ProxyEntry{
			URL:     proxy,
			Country: country,
		}
	}
	return &ProxyPool{
		proxies:    entries,
		cooldown:   cooldown,
		maxRetries: 3,
	}
}

// GetProxy returns a proxy matching the country, respecting cooldown and load balancing.
// Returns empty string if no suitable proxy is available.
func (p *ProxyPool) GetProxy(country string) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	// Filter candidates: matching country, not dead, cooldown expired
	var candidates []*ProxyEntry
	for _, proxy := range p.proxies {
		if proxy.Dead {
			continue
		}
		if proxy.Country != country {
			continue
		}
		if now.Sub(proxy.LastUsed) < p.cooldown {
			continue // still cooling down
		}
		candidates = append(candidates, proxy)
	}

	if len(candidates) == 0 {
		return "" // no available proxies
	}

	// Load balancing: prefer least-used proxy (with some randomness)
	minUses := candidates[0].UseCount
	for _, c := range candidates {
		if c.UseCount < minUses {
			minUses = c.UseCount
		}
	}

	// Collect all proxies with minimum use count
	leastUsed := []*ProxyEntry{}
	for _, c := range candidates {
		if c.UseCount == minUses {
			leastUsed = append(leastUsed, c)
		}
	}

	// Random selection from least-used
	chosen := leastUsed[rand.Intn(len(leastUsed))]
	chosen.LastUsed = now
	chosen.UseCount++

	return chosen.URL
}

// MarkSuccess resets the failure counter for a proxy (successful use).
func (p *ProxyPool) MarkSuccess(proxyURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, proxy := range p.proxies {
		if proxy.URL == proxyURL {
			proxy.FailureCount = 0
			break
		}
	}
}

// MarkFailure increments the failure counter and marks the proxy as dead if threshold exceeded.
func (p *ProxyPool) MarkFailure(proxyURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, proxy := range p.proxies {
		if proxy.URL == proxyURL {
			proxy.FailureCount++
			if proxy.FailureCount >= p.maxRetries {
				proxy.Dead = true
				fmt.Printf("proxy %s marked dead (failures=%d)\n", proxyURL, proxy.FailureCount)
			}
			break
		}
	}
}

// Stats returns proxy pool statistics.
func (p *ProxyPool) Stats() ProxyPoolStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	stats := ProxyPoolStats{
		Total: len(p.proxies),
	}

	for _, proxy := range p.proxies {
		if proxy.Dead {
			stats.Dead++
		} else {
			stats.Active++
		}
		stats.TotalUses += proxy.UseCount
	}

	return stats
}

// ProxyPoolStats contains pool statistics.
type ProxyPoolStats struct {
	Total     int
	Active    int
	Dead      int
	TotalUses int
}

// ProxyRotationStrategy wraps PayPal automation with automatic proxy rotation.
type ProxyRotationStrategy struct {
	pool       *ProxyPool
	maxRetries int // max retries with different proxies before giving up
}

// NewProxyRotationStrategy creates a strategy that retries with different proxies on failure.
func NewProxyRotationStrategy(pool *ProxyPool, maxRetries int) *ProxyRotationStrategy {
	return &ProxyRotationStrategy{
		pool:       pool,
		maxRetries: maxRetries,
	}
}

// RunWithRotation executes PayPal automation with automatic proxy rotation on failure.
func (s *ProxyRotationStrategy) RunWithRotation(
	ctx context.Context,
	checkoutURL, email, password string,
	otpProvider OTPProvider,
) error {
	for attempt := 0; attempt < s.maxRetries; attempt++ {
		// Get a proxy from the pool
		proxyURL := s.pool.GetProxy("JP")
		if proxyURL == "" {
			return fmt.Errorf("no available proxies (attempt %d/%d)", attempt+1, s.maxRetries)
		}

		fmt.Printf("paypal automation: attempt %d/%d using proxy %s\n", attempt+1, s.maxRetries, proxyURL)

		// Run automation with this proxy
		automation := NewPayPalAutomation(checkoutURL, email, password, otpProvider)
		// TODO: pass proxyURL to automation (need to modify PayPalAutomation to accept proxy)
		err := automation.Run(ctx)

		if err == nil {
			// Success!
			s.pool.MarkSuccess(proxyURL)
			return nil
		}

		// Failure: mark proxy and retry with a different one
		fmt.Printf("paypal automation: failed with proxy %s: %v\n", proxyURL, err)
		s.pool.MarkFailure(proxyURL)

		// Wait before retry (avoid hammering)
		if attempt < s.maxRetries-1 {
			time.Sleep(5 * time.Second)
		}
	}

	return fmt.Errorf("paypal automation failed after %d attempts with different proxies", s.maxRetries)
}
