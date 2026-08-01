// Package provider defines interfaces for SMS, Mailbox, and Captcha providers
package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/registration/provider/catalog"
)

// SMSProvider provides phone numbers and retrieves SMS codes
type SMSProvider interface {
	GetNumber(ctx context.Context, country string) (phone, orderID string, err error)
	WaitCode(ctx context.Context, orderID string, timeout time.Duration) (code string, err error)
	CancelNumber(ctx context.Context, orderID string) error
	Name() string
	Type() string
}

// SMSSettlementProvider is an optional provider capability for explicitly marking a
// consumed activation complete. Providers whose check endpoint auto-completes an order
// need not implement it. The pipeline invokes exactly one of CompleteNumber or
// CancelNumber for each acquired order.
type SMSSettlementProvider interface {
	CompleteNumber(ctx context.Context, orderID string) error
}

// MailboxProvider creates email addresses and retrieves verification codes
type MailboxProvider interface {
	CreateEmail(ctx context.Context) (email, password, mailboxID string, err error)
	WaitOTP(ctx context.Context, mailboxID string, timeout time.Duration) (code string, err error)
	DeleteEmail(ctx context.Context, mailboxID string) error
	Name() string
	Type() string
}

type MailboxCapabilityProvider interface {
	MailboxDomains() []string
	MailboxUsesCustomDomain() bool
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
	SMS            []SMSProvider
	Mailbox        []MailboxProvider
	Captcha        []CaptchaSolver
	Stats          *SMSStats // per-platform, per-country success-rate tracker (optional, nullable)
	priceRefreshMu sync.Mutex
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
type PriceProvider interface {
	GetCountries(ctx context.Context) ([]catalog.CountryInfo, error)
	GetTopCountries(ctx context.Context, service string) ([]catalog.CountryPrice, error)
}

// FullPriceProvider supplies every priced country, rather than only the provider's top ten.
type FullPriceProvider interface {
	GetAllCountryPrices(ctx context.Context, service string) ([]catalog.CountryPrice, error)
}

// BoundedSMSProvider lets the platform enforce the administrator's price guard at purchase
// time as well as in the selector.
type BoundedSMSProvider interface {
	GetNumberWithPriceBounds(ctx context.Context, country string, minPrice, maxPrice float64) (phone, orderID string, err error)
}

// SMSPurchase carries the exact provider/country/price selection through the pipeline so
// historical success statistics describe the number that was actually purchased.
type SMSPurchase struct {
	Provider   SMSProvider `json:"-"`
	Phone      string      `json:"phone"`
	OrderID    string      `json:"order_id"`
	CountryID  string      `json:"country_id"`
	CountryISO string      `json:"country_iso"`
	Price      float64     `json:"price"`
}

type CountryInfo = catalog.CountryInfo
type CountryPrice = catalog.CountryPrice
type smsCountryInfo = catalog.CountryInfo
type smsCountryPrice = catalog.CountryPrice

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

// GetSMSPurchase acquires from the first working provider while preserving the country and
// enforcing price bounds on adapters that support them.
func (m *Manager) GetSMSPurchase(ctx context.Context, country string, minPrice, maxPrice float64) (SMSPurchase, error) {
	for _, p := range m.SMS {
		purchase, err := m.purchaseFrom(ctx, p, country, minPrice, maxPrice)
		if err == nil {
			return purchase, nil
		}
	}
	return SMSPurchase{}, ErrNoProviderAvailable
}

func (m *Manager) GetSMSPurchaseFromProvider(ctx context.Context, providerName, country string, minPrice, maxPrice float64) (SMSPurchase, error) {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	if providerName == "" || providerName == "auto" {
		return m.GetSMSPurchase(ctx, country, minPrice, maxPrice)
	}
	for _, p := range m.SMS {
		if strings.EqualFold(strings.TrimSpace(p.Name()), providerName) {
			return m.purchaseFrom(ctx, p, country, minPrice, maxPrice)
		}
	}
	return SMSPurchase{}, ErrNoProviderAvailable
}

func (m *Manager) purchaseFrom(ctx context.Context, p SMSProvider, country string, minPrice, maxPrice float64) (SMSPurchase, error) {
	country = strings.TrimSpace(country)
	metadata := SMSPriceSnapshot{}
	if m != nil && m.Stats != nil {
		for _, item := range m.Stats.PriceSnapshots(ctx, 0) {
			if !strings.EqualFold(item.Provider, p.Name()) {
				continue
			}
			if strings.EqualFold(item.CountryID, country) || strings.EqualFold(item.CountryISO, country) {
				metadata = item
				break
			}
		}
	}
	if metadata.Price > 0 && !priceWithinBounds(metadata.Price, minPrice, maxPrice) {
		return SMSPurchase{}, fmt.Errorf("SMS country price %.4f is outside configured bounds", metadata.Price)
	}
	providerCountry := country
	if metadata.CountryID != "" {
		providerCountry = metadata.CountryID
	}
	var phone, orderID string
	var err error
	if bounded, ok := p.(BoundedSMSProvider); ok {
		phone, orderID, err = bounded.GetNumberWithPriceBounds(ctx, providerCountry, minPrice, maxPrice)
	} else {
		if minPrice > 0 || maxPrice > 0 {
			return SMSPurchase{}, fmt.Errorf("SMS provider %s cannot enforce configured price bounds", p.Name())
		}
		phone, orderID, err = p.GetNumber(ctx, providerCountry)
	}
	if err != nil {
		return SMSPurchase{}, err
	}
	iso := normalizedCountryISO(metadata.CountryISO, providerCountry)
	if iso == "" && len(country) == 2 {
		iso = strings.ToUpper(country)
	}
	return SMSPurchase{Provider: p, Phone: phone, OrderID: orderID, CountryID: providerCountry, CountryISO: iso, Price: metadata.Price}, nil
}

// GetMailbox returns the highest priority mailbox provider and creates an email
func (m *Manager) GetMailbox(ctx context.Context) (MailboxProvider, string, string, string, error) {
	return m.GetMailboxWithConstraints(ctx, "", "")
}

// GetMailboxFromProvider honors the provider selected by the registration form or
// lifecycle defaults. Empty/auto keeps priority-based fallback behavior.
func (m *Manager) GetMailboxFromProvider(ctx context.Context, providerName string) (MailboxProvider, string, string, string, error) {
	return m.GetMailboxWithConstraints(ctx, providerName, "")
}

// GetMailboxWithConstraints preserves priority fallback while enforcing an
// optional provider and canonical email domain. A provisioned address that does
// not satisfy the domain constraint is released before the next candidate is
// tried, so team rotation can never silently cross domains.
func (m *Manager) GetMailboxWithConstraints(
	ctx context.Context,
	providerName string,
	requiredDomain string,
) (MailboxProvider, string, string, string, error) {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	requiredDomain = normalizeMailboxDomain(requiredDomain)
	var lastErr error
	for _, p := range m.Mailbox {
		name := strings.ToLower(strings.TrimSpace(p.Name()))
		matches := providerName == "" || providerName == "auto" || name == providerName ||
			(providerName == "tempmail" && name == "tempmail_lol") ||
			(providerName == "tempmaillol" && name == "tempmail_lol") ||
			(providerName == "emailpool" && name == "email_pool") ||
			(providerName == "outlook_pool" && name == "email_pool")
		if !matches {
			continue
		}
		if requiredDomain != "" {
			if capable, ok := p.(MailboxCapabilityProvider); ok {
				domains := capable.MailboxDomains()
				if len(domains) > 0 && !mailboxDomainListed(domains, requiredDomain) {
					continue
				}
			}
		}
		email, password, mailboxID, err := p.CreateEmail(ctx)
		if err != nil {
			lastErr = err
			if providerName != "" && providerName != "auto" {
				break
			}
			continue
		}
		if requiredDomain != "" && mailboxAddressDomain(email) != requiredDomain {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = p.DeleteEmail(cleanupCtx, mailboxID)
			cancel()
			lastErr = fmt.Errorf("mailbox provider %s returned an address outside required domain %s", p.Name(), requiredDomain)
			if providerName != "" && providerName != "auto" {
				break
			}
			continue
		}
		return p, email, password, mailboxID, nil
	}
	if lastErr != nil {
		return nil, "", "", "", lastErr
	}
	return nil, "", "", "", ErrNoProviderAvailable
}

func normalizeMailboxDomain(value string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(value, "@")), "."))
}

func mailboxAddressDomain(address string) string {
	index := strings.LastIndexByte(strings.TrimSpace(address), '@')
	if index < 0 || index == len(address)-1 {
		return ""
	}
	return normalizeMailboxDomain(address[index+1:])
}

func mailboxDomainListed(domains []string, required string) bool {
	for _, domain := range domains {
		if normalizeMailboxDomain(domain) == required {
			return true
		}
	}
	return false
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
