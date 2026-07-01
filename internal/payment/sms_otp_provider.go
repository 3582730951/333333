package payment

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

// SMSOTPProvider retrieves PayPal 2FA codes from SMS providers (SMSBower, HeroSMS, etc.).
// This enables fully automated PayPal checkout without manual intervention.
type SMSOTPProvider struct {
	provider SMSProvider
	country  string // Country code for phone number (e.g., "JP", "US", "ID")
	orderID  string // SMS order ID (set during GetOTP)
	phoneNum string // Rented phone number (set during GetOTP)
}

// SMSProvider is the interface for SMS receiving services (SMSBower, HeroSMS, etc.).
// It matches the interface in internal/registration/provider/sms/.
type SMSProvider interface {
	GetNumber(ctx context.Context, country string) (phone, orderID string, err error)
	WaitCode(ctx context.Context, orderID string, timeout time.Duration) (code string, err error)
	CancelNumber(ctx context.Context, orderID string) error
	Name() string
}

// NewSMSOTPProvider creates an OTP provider backed by an SMS service.
func NewSMSOTPProvider(provider SMSProvider, country string) *SMSOTPProvider {
	return &SMSOTPProvider{
		provider: provider,
		country:  country,
	}
}

// GetOTP retrieves a PayPal 2FA code via SMS.
//
// Flow:
//  1. Rent a phone number from the SMS provider
//  2. Wait for PayPal SMS (timeout 60s)
//  3. Extract 6-digit code from SMS body
//  4. Return code (cancel number on error)
func (p *SMSOTPProvider) GetOTP(ctx context.Context, phone string, timeout time.Duration) (string, error) {
	// Rent a phone number
	phoneNum, orderID, err := p.provider.GetNumber(ctx, p.country)
	if err != nil {
		return "", fmt.Errorf("rent phone number: %w", err)
	}
	p.phoneNum = phoneNum
	p.orderID = orderID

	defer func() {
		// Always cancel the number after we're done (success or failure)
		if cancelErr := p.provider.CancelNumber(context.Background(), orderID); cancelErr != nil {
			// Log but don't fail the operation
			fmt.Printf("warning: failed to cancel SMS order %s: %v\n", orderID, cancelErr)
		}
	}()

	// Wait for SMS code
	smsBody, err := p.provider.WaitCode(ctx, orderID, timeout)
	if err != nil {
		return "", fmt.Errorf("wait for sms code: %w", err)
	}

	// Extract 6-digit code from SMS body
	code := extractOTPCode(smsBody)
	if code == "" {
		return "", fmt.Errorf("no 6-digit code found in sms: %s", smsBody)
	}

	return code, nil
}

// extractOTPCode extracts a 6-digit OTP code from SMS body.
// PayPal typically sends "Your PayPal security code is: 123456"
func extractOTPCode(smsBody string) string {
	// Try exact patterns first (more reliable)
	patterns := []string{
		`code is:?\s*(\d{6})`,           // "code is: 123456" or "code is 123456"
		`code:\s*(\d{6})`,               // "code: 123456"
		`verification code:?\s*(\d{6})`, // "verification code: 123456"
		`security code:?\s*(\d{6})`,     // "security code: 123456"
		`PayPal.*?(\d{6})`,              // "PayPal ... 123456"
		`(\d{6})`,                       // fallback: any 6-digit number
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		if matches := re.FindStringSubmatch(smsBody); len(matches) > 1 {
			return matches[1]
		}
	}

	return ""
}

// Phone returns the rented phone number (for reference/logging).
func (p *SMSOTPProvider) Phone() string {
	return p.phoneNum
}

// OrderID returns the SMS order ID (for reference/logging).
func (p *SMSOTPProvider) OrderID() string {
	return p.orderID
}
