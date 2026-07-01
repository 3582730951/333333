package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/stealth"
)

// PayPalAutomation automates the PayPal Plus checkout flow in a headless browser.
// Designed for cloud VPS (no desktop required) — uses Chrome's native headless mode.
//
// Flow:
//  1. Launch headless Chrome (--headless=new)
//  2. Open checkout URL
//  3. Wait for PayPal iframe to load
//  4. Fill email + password
//  5. Handle 2FA (SMS/app OTP) — may require manual intervention
//  6. Click authorize/agree button
//  7. Wait for redirect back to ChatGPT
//  8. Poll account plan status until "plus"
type PayPalAutomation struct {
	checkoutURL   string
	email         string
	password      string
	otpProvider   OTPProvider // optional: automated OTP retrieval
	screenshotDir string      // optional: save screenshots for debugging
}

// OTPProvider retrieves PayPal 2FA codes. Implement this to automate OTP (via SMS provider
// or TOTP). If nil, the automation will pause and wait for manual OTP entry (operator
// must check phone and enter code).
type OTPProvider interface {
	// GetOTP retrieves a PayPal 2FA code. Returns error if timeout.
	GetOTP(ctx context.Context, phone string, timeout time.Duration) (string, error)
}

// NewPayPalAutomation creates an automation instance.
func NewPayPalAutomation(checkoutURL, email, password string, otpProvider OTPProvider) *PayPalAutomation {
	return &PayPalAutomation{
		checkoutURL:   checkoutURL,
		email:         email,
		password:      password,
		otpProvider:   otpProvider,
		screenshotDir: "/tmp", // default screenshot location
	}
}

// Run executes the full PayPal checkout flow. Returns when payment is authorized and
// the browser redirects back to ChatGPT. The caller should then poll the account's
// plan status (Task #14).
func (a *PayPalAutomation) Run(ctx context.Context) error {
	// Launch headless Chrome (cloud VPS compatible — no X11/Wayland required)
	l := launcher.New().
		Headless(true).
		Set("disable-blink-features", "AutomationControlled"). // anti-detection
		Set("disable-dev-shm-usage").                          // fix shared memory issues on VPS
		Set("no-sandbox").                                     // required for some VPS environments (use with caution)
		Set("disable-gpu")

	// Optional: specify Chrome binary path if not in $PATH
	// l = l.Bin("/usr/bin/chromium-browser")

	url, err := l.Launch()
	if err != nil {
		return fmt.Errorf("launch chrome: %w", err)
	}

	browser := rod.New().ControlURL(url).MustConnect()
	defer browser.MustClose()

	// Apply stealth mode (bypass basic anti-bot detection)
	page := stealth.MustPage(browser)

	// Apply advanced fingerprint evasion (Canvas/WebGL/Audio/Navigator randomization)
	evasion := NewFingerprintEvasion(page)
	if err := evasion.Apply(); err != nil {
		return fmt.Errorf("apply fingerprint evasion: %w", err)
	}
	evasion.SetTimezone("Asia/Tokyo") // match PayPal JP region
	evasion.SetLocale("ja-JP")        // match PayPal locale

	// Set viewport (simulate desktop browser)
	page.MustSetViewport(1920, 1080, 1, false)

	log.Printf("paypal: navigating to checkout URL: %s", a.checkoutURL)
	if err := page.Navigate(a.checkoutURL); err != nil {
		return fmt.Errorf("navigate to checkout: %w", err)
	}

	// Wait for page to stabilize
	page.MustWaitLoad()
	a.screenshot(page, "01-checkout-loaded")

	// Wait for PayPal button/iframe (Stripe checkout embeds PayPal in an iframe or button)
	log.Printf("paypal: waiting for PayPal button/iframe")
	if err := a.waitAndClickPayPal(page); err != nil {
		return fmt.Errorf("click paypal button: %w", err)
	}

	a.screenshot(page, "02-paypal-clicked")

	// Switch to PayPal login page (may open in new window or redirect)
	time.Sleep(2 * time.Second)
	pages := browser.MustPages()
	var paypalPage *rod.Page
	for _, p := range pages {
		info, _ := p.Info()
		if info != nil && (containsAny(info.URL, "paypal.com", "sandbox.paypal.com")) {
			paypalPage = p
			break
		}
	}
	if paypalPage == nil {
		// PayPal may load in the same page (redirect)
		paypalPage = page
	}

	log.Printf("paypal: filling login credentials")
	if err := a.login(paypalPage); err != nil {
		return fmt.Errorf("paypal login: %w", err)
	}

	a.screenshot(paypalPage, "03-login-filled")

	// Handle 2FA (OTP)
	log.Printf("paypal: checking for 2FA")
	if err := a.handle2FA(ctx, paypalPage); err != nil {
		return fmt.Errorf("paypal 2FA: %w", err)
	}

	a.screenshot(paypalPage, "04-2fa-passed")

	// Authorize payment (click "Agree and Continue" / "Authorize")
	log.Printf("paypal: authorizing payment")
	if err := a.authorize(paypalPage); err != nil {
		return fmt.Errorf("paypal authorize: %w", err)
	}

	a.screenshot(paypalPage, "05-authorized")

	// Wait for redirect back to ChatGPT
	log.Printf("paypal: waiting for redirect to chatgpt.com")
	if err := paypalPage.WaitLoad(); err != nil {
		return fmt.Errorf("wait for redirect: %w", err)
	}
	info, _ := paypalPage.Info()
	if info != nil && containsAny(info.URL, "chatgpt.com", "openai.com") {
		log.Printf("paypal: successfully redirected to %s", info.URL)
		a.screenshot(paypalPage, "06-redirect-success")
		return nil
	}

	return fmt.Errorf("unexpected final URL: %s", info.URL)
}

// waitAndClickPayPal waits for the PayPal button on the Stripe checkout page and clicks it.
func (a *PayPalAutomation) waitAndClickPayPal(page *rod.Page) error {
	// Common selectors for PayPal button on Stripe checkout
	selectors := []string{
		`button[data-testid="payment-method-selector-PayPal"]`,
		`button:contains("PayPal")`,
		`[data-qa="payment-method-PayPal"]`,
		`#payment-method-paypal`,
		`.StripeElement iframe`, // PayPal may be in an iframe
	}

	for _, sel := range selectors {
		el, err := page.Timeout(5 * time.Second).Element(sel)
		if err == nil && el != nil {
			el.MustClick()
			return nil
		}
	}

	return fmt.Errorf("paypal button not found (tried %d selectors)", len(selectors))
}

// login fills PayPal email and password.
func (a *PayPalAutomation) login(page *rod.Page) error {
	page.MustWaitLoad()

	// Email field
	emailSel := `input[type="email"], input[name="login_email"], #email`
	emailEl, err := page.Timeout(10 * time.Second).Element(emailSel)
	if err != nil {
		return fmt.Errorf("email field not found: %w", err)
	}
	emailEl.MustInput(a.email)

	// Click "Next" or submit (some PayPal flows split email/password)
	nextBtn, _ := page.Timeout(2 * time.Second).Element(`button[type="submit"], button:contains("Next")`)
	if nextBtn != nil {
		nextBtn.MustClick()
		time.Sleep(2 * time.Second)
	}

	// Password field
	passSel := `input[type="password"], input[name="login_password"], #password`
	passEl, err := page.Timeout(10 * time.Second).Element(passSel)
	if err != nil {
		return fmt.Errorf("password field not found: %w", err)
	}
	passEl.MustInput(a.password)

	// Submit login
	submitBtn, err := page.Timeout(5 * time.Second).Element(`button[type="submit"], button:contains("Log In"), #btnLogin`)
	if err != nil {
		return fmt.Errorf("login submit button not found: %w", err)
	}
	submitBtn.MustClick()

	page.MustWaitLoad()
	return nil
}

// handle2FA checks for 2FA prompt and retrieves OTP.
func (a *PayPalAutomation) handle2FA(ctx context.Context, page *rod.Page) error {
	time.Sleep(3 * time.Second)

	// Check if 2FA prompt exists
	otpSel := `input[name="otpCode"], input[name="securityCode"], input[type="tel"]`
	otpEl, err := page.Timeout(5 * time.Second).Element(otpSel)
	if err != nil {
		// No 2FA prompt — skip
		log.Printf("paypal: no 2FA prompt detected, continuing")
		return nil
	}

	log.Printf("paypal: 2FA prompt detected")

	var otp string
	if a.otpProvider != nil {
		// Automated OTP retrieval
		log.Printf("paypal: retrieving OTP via provider")
		otp, err = a.otpProvider.GetOTP(ctx, "", 60*time.Second)
		if err != nil {
			return fmt.Errorf("get OTP: %w", err)
		}
	} else {
		// Manual OTP (operator must check phone and enter code)
		// In production, you'd implement a channel/webhook for manual entry
		return fmt.Errorf("2FA required but no OTP provider configured — operator must manually complete 2FA in browser (consider using --headed mode for debugging)")
	}

	otpEl.MustInput(otp)

	// Submit OTP
	submitBtn, err := page.Timeout(5 * time.Second).Element(`button[type="submit"], button:contains("Continue"), button:contains("Confirm")`)
	if err != nil {
		return fmt.Errorf("2FA submit button not found: %w", err)
	}
	submitBtn.MustClick()

	page.MustWaitLoad()
	return nil
}

// authorize clicks the "Agree and Continue" / "Authorize" button on PayPal.
func (a *PayPalAutomation) authorize(page *rod.Page) error {
	page.MustWaitLoad()
	time.Sleep(2 * time.Second)

	// Common authorize button selectors
	selectors := []string{
		`button:contains("Agree")`,
		`button:contains("Authorize")`,
		`button:contains("Continue")`,
		`#payment-submit-btn`,
		`[data-testid="continueButton"]`,
	}

	for _, sel := range selectors {
		btn, err := page.Timeout(5 * time.Second).Element(sel)
		if err == nil && btn != nil {
			btn.MustClick()
			return nil
		}
	}

	return fmt.Errorf("authorize button not found (tried %d selectors)", len(selectors))
}

// screenshot saves a page screenshot (for debugging in headless mode).
func (a *PayPalAutomation) screenshot(page *rod.Page, name string) {
	if a.screenshotDir == "" {
		return
	}
	path := fmt.Sprintf("%s/paypal-%s-%d.png", a.screenshotDir, name, time.Now().Unix())
	page.MustScreenshotFullPage(path)
	log.Printf("paypal: screenshot saved: %s", path)
}

// containsAny checks if s contains any of the substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) >= len(sub) && s[:len(sub)] == sub || len(s) > len(sub) && s[len(s)-len(sub):] == sub {
			return true
		}
		// Simple contains check
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

// PollAccountPlanStatus polls the account's plan_type until it changes to "plus" (Task #14).
// This is called AFTER the PayPal browser flow completes. It queries ChatGPT's
// /backend-api/accounts/me endpoint using the account's access token.
func PollAccountPlanStatus(ctx context.Context, accessToken string, interval, timeout time.Duration) error {
	client := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()

	for time.Since(start) < timeout {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://chatgpt.com/backend-api/accounts/me", nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("poll plan status: request failed: %v", err)
			time.Sleep(interval)
			continue
		}

		if resp.StatusCode == 200 {
			var result struct {
				AccountPlan struct {
					Type string `json:"type"`
				} `json:"account_plan"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
				resp.Body.Close()
				if result.AccountPlan.Type == "plus" {
					log.Printf("poll plan status: account upgraded to Plus")
					return nil
				}
				log.Printf("poll plan status: current plan = %s (waiting for 'plus')", result.AccountPlan.Type)
			}
		}
		resp.Body.Close()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}

	return fmt.Errorf("timeout waiting for plan upgrade (%v)", timeout)
}
