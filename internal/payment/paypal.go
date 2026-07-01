package payment

import (
	"context"
	"fmt"
	"time"

	"codex-account-pool/internal/storage"
)

// PaypalProvider upgrades accounts via PayPal checkout with headless browser automation.
type PaypalProvider struct {
	store    *storage.Store
	settings map[string]interface{} // paypal_email, paypal_password, etc.
}

// NewPaypalProvider builds the PayPal payment provider.
func NewPaypalProvider(store *storage.Store, settings map[string]interface{}) *PaypalProvider {
	return &PaypalProvider{
		store:    store,
		settings: settings,
	}
}

func (p *PaypalProvider) Name() string { return "paypal" }

func (p *PaypalProvider) Subscribe(ctx context.Context, account *storage.Account, proxyURL string) error {
	if p.store == nil {
		return fmt.Errorf("paypal provider: store not initialized")
	}
	tok, err := p.store.GetToken(ctx, account.ID)
	if err != nil || tok.AccessToken == "" {
		return fmt.Errorf("account %s has no session token: %w", account.ID, err)
	}

	// Step 1: Generate PayPal checkout URL
	gen := NewCheckoutURLGenerator(proxyURL)
	checkoutResp, err := gen.GenerateCheckoutURL(ctx, CheckoutRequest{
		AccessToken:    tok.AccessToken,
		LinkType:       "paypal",
		BillingCountry: "JP",
		CheckoutUIMode: "hosted",
		PaymentLocale:  "ja",
		CustomID:       account.ID, // for webhook → account mapping
	})
	if err != nil {
		return fmt.Errorf("generate paypal checkout URL: %w", err)
	}

	// Step 2: Run headless browser automation (requires PayPal credentials in settings)
	paypalEmail := p.getStringSetting("paypal_email")
	paypalPassword := p.getStringSetting("paypal_password")
	if paypalEmail == "" || paypalPassword == "" {
		return fmt.Errorf("paypal credentials not configured (set paypal_email/paypal_password in manager settings) — checkout URL: %s", checkoutResp.LongURL)
	}

	// Optional: SMS OTP provider (if configured)
	var otpProvider OTPProvider
	if smsProviderName := p.getStringSetting("sms_provider"); smsProviderName != "" {
		// Build SMS OTP provider (caller should inject SMS provider instance via settings)
		// For now, log that SMS OTP is configured but not wired
		// TODO: wire SMS provider from registration manager
		fmt.Printf("paypal: sms_provider=%s configured (not yet wired)\n", smsProviderName)
	}

	automation := NewPayPalAutomation(checkoutResp.LongURL, paypalEmail, paypalPassword, otpProvider)
	if err := automation.Run(ctx); err != nil {
		return fmt.Errorf("paypal browser automation failed: %w — manual URL: %s", err, checkoutResp.LongURL)
	}

	// Step 3: Poll account plan status until upgraded
	if err := PollAccountPlanStatus(ctx, tok.AccessToken, 10*time.Second, 5*time.Minute); err != nil {
		return fmt.Errorf("paypal payment completed but plan upgrade not detected: %w", err)
	}

	// Update account plan_type (caller will persist via storage layer)
	account.PlanType = "plus"
	return nil
}

func (p *PaypalProvider) getStringSetting(key string) string {
	if p.settings != nil {
		if v, ok := p.settings[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}
