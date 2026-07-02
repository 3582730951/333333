// Package provider defines interfaces for SMS, Mailbox, and Captcha providers
package provider

import (
	"context"
	"strings"
	"time"
)

// SMSProvider provides phone numbers and retrieves SMS codes
type SMSProvider interface {
	GetNumber(ctx context.Context, country string) (phone, orderID string, err error)
	WaitCode(ctx context.Context, orderID string, timeout time.Duration) (code string, err error)
	CancelNumber(ctx context.Context, orderID string) error
	Name() string
	Type() string
}

// MailboxProvider creates email addresses and retrieves verification codes
type MailboxProvider interface {
	CreateEmail(ctx context.Context) (email, password, mailboxID string, err error)
	WaitOTP(ctx context.Context, mailboxID string, timeout time.Duration) (code string, err error)
	DeleteEmail(ctx context.Context, mailboxID string) error
	Name() string
	Type() string
}

// CaptchaSolver solves captcha challenges
type CaptchaSolver interface {
	Solve(ctx context.Context, req CaptchaRequest) (solution string, err error)
	Name() string
	Type() string
}

// CaptchaRequest defines a captcha challenge
type CaptchaRequest struct {
	Type     string // "recaptcha_v2" | "hcaptcha" | "funcaptcha"
	SiteKey  string
	PageURL  string
	ProxyURL string
}

// Manager coordinates multiple providers with fallback
type Manager struct {
	SMS     []SMSProvider
	Mailbox []MailboxProvider
	Captcha []CaptchaSolver
	Stats   *SMSStats // per-platform, per-country success-rate tracker (optional, nullable)
}

// BalanceProvider is an optional capability an SMSProvider may implement: querying the
// account balance so the Manager can skip a platform with no funds. hero-sms and SMSBower
// both implement this.
type BalanceProvider interface {
	GetBalance(ctx context.Context) (float64, error)
}

// PriceProvider is an optional capability an SMSProvider may implement: querying the
// platform's country catalog and the success-ranked price/stock list for a service. The
// "rank" returned by GetTopCountries is the platform's own internal success-priority order
// (per the SMS-Activate-compatible getTopCountriesByService "sorted by internal priority"
// semantics) — rank 0 = highest same-day success probability.
//
// The sms package defines its own CountryInfo / CountryPrice types; the interface uses
// generic slices (any) + type assertion because the concrete types live in the sms
// sub-package to avoid a circular import.
type PriceProvider interface {
	GetCountries(ctx context.Context) ([]interface{}, error)
	GetTopCountries(ctx context.Context, service string) ([]interface{}, error)
}

// smsCountryInfo is a platform country entry (numeric id + localized names + ISO/dial if known).
type smsCountryInfo struct {
	ID   int    `json:"id"`
	Eng  string `json:"eng"`
	Chn  string `json:"chn"`
	ISO  string `json:"iso,omitempty"`
	Dial string `json:"dial,omitempty"`
}

// smsCountryPrice is one country's price+stock for a service, in the platform's internal
// success-priority order (Rank 0 = highest success probability).
type smsCountryPrice struct {
	Country string  `json:"country"` // numeric country id (as a string)
	Name    string  `json:"name,omitempty"`
	Price   float64 `json:"price"`
	Count   int     `json:"count"`
	Rank    int     `json:"rank"` // 0-based position in the platform's success ranking
}

// CountryInfo is the exported alias (the sms package's local alias points here).
type CountryInfo = smsCountryInfo

// CountryPrice is the exported alias (the sms package's local alias points here).
type CountryPrice = smsCountryPrice

// GetSMS returns the highest priority SMS provider and gets a number
func (m *Manager) GetSMS(ctx context.Context, country string) (SMSProvider, string, string, error) {
	for _, p := range m.SMS {
		phone, orderID, err := p.GetNumber(ctx, country)
		if err == nil {
			return p, phone, orderID, nil
		}
	}
	return nil, "", "", ErrNoProviderAvailable
}

func (m *Manager) GetSMSFromProvider(ctx context.Context, providerName, country string) (SMSProvider, string, string, error) {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	if providerName == "" || providerName == "auto" {
		return m.GetSMS(ctx, country)
	}
	var lastErr error
	for _, p := range m.SMS {
		if strings.EqualFold(strings.TrimSpace(p.Name()), providerName) {
			phone, orderID, err := p.GetNumber(ctx, country)
			if err == nil {
				return p, phone, orderID, nil
			}
			lastErr = err
			break
		}
	}
	if lastErr != nil {
		return nil, "", "", lastErr
	}
	return nil, "", "", ErrNoProviderAvailable
}

// GetMailbox returns the highest priority mailbox provider and creates an email
func (m *Manager) GetMailbox(ctx context.Context) (MailboxProvider, string, string, string, error) {
	for _, p := range m.Mailbox {
		email, password, mailboxID, err := p.CreateEmail(ctx)
		if err == nil {
			return p, email, password, mailboxID, nil
		}
	}
	return nil, "", "", "", ErrNoProviderAvailable
}

// SolveCaptcha tries all solvers until one succeeds
func (m *Manager) SolveCaptcha(ctx context.Context, req CaptchaRequest) (string, error) {
	for _, s := range m.Captcha {
		solution, err := s.Solve(ctx, req)
		if err == nil {
			return solution, nil
		}
	}
	return "", ErrNoProviderAvailable
}

var ErrNoProviderAvailable = &ProviderError{Msg: "no provider available"}

type ProviderError struct {
	Msg string
}

func (e *ProviderError) Error() string {
	return e.Msg
}
