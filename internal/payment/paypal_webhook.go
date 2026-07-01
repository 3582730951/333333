package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

const maxPayPalWebhookBodyBytes = 1 << 20

// PayPalWebhookHandler receives PayPal IPN/Webhook notifications and updates account
// plan_type instantly (no 5-minute polling). This is the production-recommended approach.
//
// PayPal sends webhooks for subscription events:
//   - BILLING.SUBSCRIPTION.CREATED
//   - BILLING.SUBSCRIPTION.ACTIVATED
//   - BILLING.SUBSCRIPTION.UPDATED
//   - BILLING.SUBSCRIPTION.CANCELLED
//
// The handler:
//  1. Verifies webhook signature (HMAC-SHA256)
//  2. Parses event payload
//  3. Updates account.plan_type in database
//  4. Returns 200 OK (PayPal retries on failure)
type PayPalWebhookHandler struct {
	store         *storage.Store
	webhookSecret string // PayPal webhook signing secret
}

type payPalWebhookError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type payPalWebhookResponse struct {
	Status string              `json:"status,omitempty"`
	Error  *payPalWebhookError `json:"error,omitempty"`
}

// NewPayPalWebhookHandler creates a webhook handler.
func NewPayPalWebhookHandler(store *storage.Store, webhookSecret string) *PayPalWebhookHandler {
	return &PayPalWebhookHandler{
		store:         store,
		webhookSecret: webhookSecret,
	}
}

// ServeHTTP implements http.Handler for /webhooks/paypal endpoint.
func (h *PayPalWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wrote := false
	writeStatus := func(status int, response payPalWebhookResponse) {
		wrote = true
		writePayPalWebhookJSON(w, status, response)
	}
	writeError := func(status int, code, message string) {
		writeStatus(status, payPalWebhookResponse{
			Error: &payPalWebhookError{Code: code, Message: message},
		})
	}
	defer func() {
		if v := recover(); v != nil {
			log.Printf("[PANIC] paypal webhook: method=%s path=%s remote=%s panic=%v",
				r.Method, r.URL.Path, r.RemoteAddr, v)
			supervisor.LogPanic("paypal-webhook", fmt.Sprintf("%s %s remote=%s panic=%v",
				r.Method, r.URL.Path, r.RemoteAddr, v))
			if !wrote {
				writeError(http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}
	}()

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if h == nil {
		writeError(http.StatusInternalServerError, "handler_not_configured", "webhook handler is not configured")
		return
	}
	if r.Body == nil {
		writeError(http.StatusBadRequest, "missing_body", "request body is required")
		return
	}
	defer r.Body.Close()

	// Read body
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPayPalWebhookBodyBytes))
	if err != nil {
		log.Printf("paypal webhook: read body failed: %v", err)
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return
		}
		writeError(http.StatusBadRequest, "bad_request", "bad request")
		return
	}

	// Verify signature
	signature := r.Header.Get("Paypal-Transmission-Sig")
	transmissionID := r.Header.Get("Paypal-Transmission-Id")
	transmissionTime := r.Header.Get("Paypal-Transmission-Time")
	certURL := r.Header.Get("Paypal-Cert-Url")

	if !h.verifySignature(signature, transmissionID, transmissionTime, body) {
		log.Printf("paypal webhook: invalid signature (id=%s, cert=%s)", transmissionID, certURL)
		writeError(http.StatusUnauthorized, "invalid_signature", "invalid signature")
		return
	}

	// Parse event
	var event PayPalWebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		log.Printf("paypal webhook: parse event failed: %v", err)
		writeError(http.StatusBadRequest, "invalid_json", "invalid json")
		return
	}

	// Process event
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := h.processEvent(ctx, &event); err != nil {
		log.Printf("paypal webhook: process event failed: %v", err)
		// Return 200 anyway (PayPal retries on 4xx/5xx)
		// We log the error for manual investigation
	}

	writeStatus(http.StatusOK, payPalWebhookResponse{Status: "ok"})
}

// PayPalWebhookEvent is the top-level webhook payload.
type PayPalWebhookEvent struct {
	ID           string `json:"id"`
	EventType    string `json:"event_type"`
	CreateTime   string `json:"create_time"`
	ResourceType string `json:"resource_type"`
	Summary      string `json:"summary"`
	Resource     struct {
		ID               string `json:"id"`     // subscription ID
		Status           string `json:"status"` // ACTIVE, CANCELLED, etc.
		StatusUpdateTime string `json:"status_update_time"`
		PlanID           string `json:"plan_id"`
		Subscriber       struct {
			EmailAddress string `json:"email_address"`
			PayerID      string `json:"payer_id"`
		} `json:"subscriber"`
		CustomID string `json:"custom_id"` // our account.id (set during checkout)
	} `json:"resource"`
}

// processEvent updates account plan_type based on webhook event.
func (h *PayPalWebhookHandler) processEvent(ctx context.Context, event *PayPalWebhookEvent) error {
	if h == nil || h.store == nil {
		return errors.New("paypal webhook store is not configured")
	}
	if event == nil {
		return errors.New("paypal webhook event is nil")
	}
	log.Printf("paypal webhook: event=%s, subscription=%s, status=%s, custom=%s",
		event.EventType, event.Resource.ID, event.Resource.Status, event.Resource.CustomID)

	// We use custom_id to map subscription → account
	accountID := event.Resource.CustomID
	if accountID == "" {
		return fmt.Errorf("no custom_id in webhook payload")
	}

	// Fetch account
	account, err := h.store.GetAccount(ctx, accountID)
	if err != nil {
		return fmt.Errorf("fetch account %s: %w", accountID, err)
	}

	// Fetch token (needed for UpsertAccount)
	token, err := h.store.GetToken(ctx, accountID)
	if err != nil {
		return fmt.Errorf("fetch token %s: %w", accountID, err)
	}

	// Update plan_type based on subscription status
	newPlan := ""
	switch event.Resource.Status {
	case "ACTIVE":
		newPlan = "plus"
	case "CANCELLED", "SUSPENDED", "EXPIRED":
		newPlan = "free"
	default:
		log.Printf("paypal webhook: unknown status=%s, ignoring", event.Resource.Status)
		return nil
	}

	if account.PlanType == newPlan {
		log.Printf("paypal webhook: account %s already plan=%s, no change", accountID, newPlan)
		return nil
	}

	// Update database via UpsertAccount
	account.PlanType = newPlan
	if err := h.store.UpsertAccount(ctx, account, token); err != nil {
		return fmt.Errorf("upsert account %s plan_type: %w", accountID, err)
	}

	log.Printf("paypal webhook: account %s plan updated to %s", accountID, newPlan)
	return nil
}

// verifySignature validates PayPal's HMAC-SHA256 signature.
//
// PayPal signature algorithm:
//  1. Concatenate: transmission_id|transmission_time|webhook_id|crc32(body)
//  2. HMAC-SHA256(secret, concat_string)
//  3. Base64(hmac) == signature header
//
// For simplicity, we use a simpler HMAC(secret, body) verification.
// Production: use PayPal SDK's signature verification (handles cert chain).
func (h *PayPalWebhookHandler) verifySignature(signature, transmissionID, transmissionTime string, body []byte) bool {
	if h.webhookSecret == "" {
		// Development mode: skip verification
		log.Printf("paypal webhook: webhook_secret not configured, skipping signature verification")
		return true
	}

	// Simple HMAC verification (production should use PayPal SDK)
	mac := hmac.New(sha256.New, []byte(h.webhookSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(signature), []byte(expected)) {
		log.Printf("paypal webhook: signature mismatch (id=%s, time=%s)", transmissionID, transmissionTime)
		return false
	}

	return true
}

func writePayPalWebhookJSON(w http.ResponseWriter, status int, response payPalWebhookResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

// RegisterWebhookRoutes adds the PayPal webhook endpoint to the server mux.
func RegisterWebhookRoutes(mux *http.ServeMux, store *storage.Store, webhookSecret string) {
	handler := NewPayPalWebhookHandler(store, webhookSecret)
	mux.Handle("/webhooks/paypal", handler)
	log.Printf("paypal webhook: registered /webhooks/paypal endpoint")
}
