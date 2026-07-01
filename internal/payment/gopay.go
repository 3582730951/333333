package payment

import (
	"context"
	"fmt"

	"codex-account-pool/internal/gopay"
	"codex-account-pool/internal/storage"
)

// GopayProvider wraps the existing gopay.Manager into the payment.Provider interface.
// It reuses the proven Stripe→Midtrans→GoPay chain (no re-port).
type GopayProvider struct {
	mgr   *gopay.Manager
	store *storage.Store
}

// NewGopayProvider builds the GoPay payment provider adapter.
func NewGopayProvider(mgr *gopay.Manager, store *storage.Store) *GopayProvider {
	return &GopayProvider{mgr: mgr, store: store}
}

func (p *GopayProvider) Name() string { return "gopay" }

// Subscribe upgrades the account via GoPay. The account's stored session token is used;
// phone/pin come from the GoPay manager's saved settings (operator-configured). proxyURL
// is passed to the payment flow but the GoPay manager also respects its own egress
// setting (this is a pass-through for orchestrator consistency).
func (p *GopayProvider) Subscribe(ctx context.Context, account *storage.Account, proxyURL string) error {
	if p.mgr == nil {
		return fmt.Errorf("gopay manager not initialized")
	}
	if !p.mgr.Enabled(ctx) {
		return fmt.Errorf("gopay is disabled (enable it on the GoPay settings page)")
	}
	tok, err := p.store.GetToken(ctx, account.ID)
	if err != nil || tok.AccessToken == "" {
		return fmt.Errorf("account %s has no session token: %w", account.ID, err)
	}
	// The gopay.Manager.Subscribe() signature is (ctx, sessionToken, phone, pin) →
	// (result map, error). Phone/pin come from the manager's saved settings (the operator
	// configured them once; all upgrades share them). On success the account's plan_type
	// is updated inside the Python gRPC flow; the manager returns a result map (ignored here).
	if _, err := p.mgr.Subscribe(ctx, tok.AccessToken, "", ""); err != nil {
		return fmt.Errorf("gopay subscribe: %w", err)
	}
	// Now update the account's PlanType in the pool (the Python flow updated ChatGPT's view,
	// but the pool's stored row is stale until we sync it here).
	account.PlanType = "plus"
	if _, err := p.store.DB().ExecContext(ctx,
		`UPDATE accounts SET plan_type = ? WHERE id = ?`, account.PlanType, account.ID); err != nil {
		return fmt.Errorf("update account plan_type: %w", err)
	}
	return nil
}
