package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CheckoutURLGenerator generates ChatGPT Plus checkout URLs (Stripe-hosted, PayPal, GoPay).
// The flow is:
//  1. POST chatgpt.com/backend-api/payments/checkout → cs_id
//  2. POST api.stripe.com/v1/payment_pages/{cs_id}/init → stripe_hosted_url
//  3. Replace checkout.stripe.com → pay.openai.com → long_url
type CheckoutURLGenerator struct {
	client *http.Client
}

// NewCheckoutURLGenerator creates a generator with optional proxy.
func NewCheckoutURLGenerator(proxyURL string) *CheckoutURLGenerator {
	client := &http.Client{Timeout: checkoutHTTPTimeout}
	if proxyURL != "" {
		if purl, err := url.Parse(proxyURL); err == nil {
			client.Transport = &http.Transport{Proxy: http.ProxyURL(purl)}
		}
	}
	return &CheckoutURLGenerator{client: client}
}

// CheckoutRequest configures the checkout link generation.
type CheckoutRequest struct {
	AccessToken    string // ChatGPT session access token
	LinkType       string // "hosted" (Stripe), "paypal", "gopay"
	BillingCountry string // "US", "JP" (paypal), "ID" (gopay)
	CheckoutUIMode string // "hosted" (default) or "custom" (short link)
	PaymentLocale  string // "en", "ja", etc.
	CustomID       string // Optional: custom_id for webhook mapping (e.g., account.ID)
}

// CheckoutResponse contains the generated checkout URL and metadata.
type CheckoutResponse struct {
	CSID            string `json:"cs_id"`
	ProcessorEntity string `json:"processor_entity"`
	BillingCountry  string `json:"billing_country"`
	Currency        string `json:"currency"`
	PaymentLocale   string `json:"payment_locale"`
	LinkType        string `json:"link_type"`
	StripeHostedURL string `json:"stripe_hosted_url"`
	LongURL         string `json:"long_url"`
}

const (
	stripePublicKey = "pk_live_51HOrSwC6h1nxGoI3lTAgRjYVrz4dU3fVOabyCcKR3pbEJguCVAlqCxdxCUvoRh1XWwRacViovU3kLKvpkjh7IqkW00iXQsjo3n"
	stripeVersion   = "2025-03-31.basil; checkout_server_update_beta=v1; checkout_manual_approval_preview=v1"

	checkoutHTTPTimeout    = 60 * time.Second
	checkoutErrorBodyLimit = 64 * 1024
)

var countryCurrencies = map[string]string{
	"US": "USD", "GB": "GBP", "CA": "CAD", "AU": "AUD", "JP": "JPY",
	"SG": "SGD", "HK": "HKD", "TW": "TWD", "KR": "KRW", "ID": "IDR",
	"MY": "MYR", "TH": "THB", "VN": "VND", "PH": "PHP", "IN": "INR",
	"DE": "EUR", "FR": "EUR", "IT": "EUR", "ES": "EUR", "NL": "EUR",
	"MX": "MXN", "BR": "BRL", "NZ": "NZD", "CH": "CHF", "SE": "SEK",
}

var lockedCountries = map[string]string{
	"paypal": "JP",
	"gopay":  "ID",
}

// GenerateCheckoutURL generates a Plus checkout URL for the given account.
func (g *CheckoutURLGenerator) GenerateCheckoutURL(ctx context.Context, req CheckoutRequest) (*CheckoutResponse, error) {
	// Lock country for payment method
	if req.LinkType != "hosted" {
		if locked, ok := lockedCountries[req.LinkType]; ok {
			req.BillingCountry = locked
		}
	}
	if req.BillingCountry == "" {
		req.BillingCountry = "US"
	}
	if req.CheckoutUIMode == "" {
		req.CheckoutUIMode = "hosted"
	}
	if req.PaymentLocale == "" {
		req.PaymentLocale = "en"
	}

	currency := countryCurrencies[req.BillingCountry]
	if currency == "" {
		currency = "USD"
	}

	// Step 1: POST chatgpt.com/backend-api/payments/checkout
	csID, processorEntity, err := g.step1OpenAI(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("step1 openai checkout: %w", err)
	}

	// Step 2: POST api.stripe.com/v1/payment_pages/{cs_id}/init
	stripeHostedURL, err := g.step2Stripe(ctx, csID, req.PaymentLocale)
	if err != nil {
		return nil, fmt.Errorf("step2 stripe init: %w", err)
	}

	// Step 3: Replace checkout.stripe.com → pay.openai.com
	longURL := strings.Replace(stripeHostedURL, "checkout.stripe.com", "pay.openai.com", 1)

	return &CheckoutResponse{
		CSID:            csID,
		ProcessorEntity: processorEntity,
		BillingCountry:  req.BillingCountry,
		Currency:        currency,
		PaymentLocale:   req.PaymentLocale,
		LinkType:        req.LinkType,
		StripeHostedURL: stripeHostedURL,
		LongURL:         longURL,
	}, nil
}

func (g *CheckoutURLGenerator) step1OpenAI(ctx context.Context, req CheckoutRequest) (csID, processorEntity string, err error) {
	payload := map[string]interface{}{
		"plan_tier":              "plus",
		"billing_country":        req.BillingCountry,
		"checkout_ui_mode":       req.CheckoutUIMode,
		"payment_locale":         req.PaymentLocale,
		"stripe_publishable_key": stripePublicKey,
		"payment_method_type":    req.LinkType,
	}
	if req.CustomID != "" {
		payload["custom_id"] = req.CustomID // for webhook → account mapping
	}
	body, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://chatgpt.com/backend-api/payments/checkout", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Origin", "https://chatgpt.com")
	httpReq.Header.Set("Referer", "https://chatgpt.com/")
	httpReq.Header.Set("Authorization", "Bearer "+req.AccessToken)
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b := readCheckoutErrorBody(resp.Body)
		return "", "", fmt.Errorf("openai checkout: HTTP %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		CSID            string `json:"cs_id"`
		ProcessorEntity string `json:"processor_entity"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", err
	}
	return result.CSID, result.ProcessorEntity, nil
}

func (g *CheckoutURLGenerator) step2Stripe(ctx context.Context, csID, locale string) (string, error) {
	stripeJSID := uuid.New().String()

	form := url.Values{}
	form.Set("browser_locale", "en-US")
	form.Set("browser_timezone", "Asia/Shanghai")
	form.Set("elements_session_client[client_betas][0]", "custom_checkout_server_updates_1")
	form.Set("elements_session_client[client_betas][1]", "custom_checkout_manual_approval_1")
	form.Set("elements_session_client[elements_init_source]", "custom_checkout")
	form.Set("elements_session_client[referrer_host]", "chatgpt.com")
	form.Set("elements_session_client[stripe_js_id]", stripeJSID)
	form.Set("elements_session_client[locale]", locale)
	form.Set("elements_session_client[is_aggregation_expected]", "false")
	form.Set("elements_options_client[saved_payment_method][enable_save]", "auto")
	form.Set("elements_options_client[saved_payment_method][enable_redisplay]", "auto")
	form.Set("key", stripePublicKey)
	form.Set("_stripe_version", stripeVersion)

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("https://api.stripe.com/v1/payment_pages/%s/init", csID),
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+stripePublicKey)
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b := readCheckoutErrorBody(resp.Body)
		return "", fmt.Errorf("stripe init: HTTP %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		CheckoutSessionURL string `json:"checkout_session_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.CheckoutSessionURL, nil
}

func readCheckoutErrorBody(body io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(body, checkoutErrorBodyLimit))
	return b
}
