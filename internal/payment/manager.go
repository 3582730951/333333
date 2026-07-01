// Package payment abstracts Plus upgrade payment providers (GoPay / PayPal) behind
// a common interface so the automation scheduler and lifecycle orchestrator are decoupled
// from the payment implementation.
package payment

import (
	"context"
	"fmt"

	"codex-account-pool/internal/storage"
)

// Provider upgrades a pooled account to ChatGPT Plus via a payment flow (Stripe→GoPay
// or PayPal). Implementations are side-effect-heavy (launch subprocesses, call third-party
// APIs, modify account state) so callers must be idempotent.
type Provider interface {
	// Name returns the provider key (gopay, paypal).
	Name() string

	// Subscribe upgrades one account to Plus. The account's stored session token + any
	// provider-specific credentials (phone/pin for GoPay) are pulled from storage. proxyURL
	// is the egress for the payment flow (must be JP/TW for ChatGPT region checks). Returns
	// an error if the upgrade fails; a nil error means the account is now Plus (the caller
	// must update account.PlanType).
	Subscribe(ctx context.Context, account *storage.Account, proxyURL string) error
}

// Manager holds all registered payment providers and selects one by name.
type Manager struct {
	providers map[string]Provider
}

// NewManager builds a payment manager with all available providers registered.
func NewManager() *Manager {
	return &Manager{providers: make(map[string]Provider)}
}

// Register adds a provider to the manager.
func (m *Manager) Register(p Provider) {
	m.providers[p.Name()] = p
}

// Get returns the provider by name, or an error if not found.
func (m *Manager) Get(name string) (Provider, error) {
	if p, ok := m.providers[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("payment provider %q not found (available: gopay, paypal)", name)
}

// List returns all registered provider names.
func (m *Manager) List() []string {
	names := make([]string, 0, len(m.providers))
	for n := range m.providers {
		names = append(names, n)
	}
	return names
}
