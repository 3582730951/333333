package pipeline

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/registration/provider"
	"codex-account-pool/internal/supervisor"
)

// registrarOTPRelay gives an isolated browser worker one narrowly scoped capability:
// wait for the OTP belonging to the order already leased by the Go orchestrator. The
// provider credential and order ID never enter the worker config or environment.
type registrarOTPRelay struct {
	server      *http.Server
	listener    net.Listener
	url         string
	bearerToken string
	provider    provider.SMSProvider
	orderID     string
	waitTimeout time.Duration

	mu       sync.Mutex
	code     string
	inFlight chan struct{}
	lastErr  error
	consumed atomic.Bool
}

func startRegistrarOTPRelay(ctx context.Context, smsProvider provider.SMSProvider, orderID string, waitTimeout time.Duration) (*registrarOTPRelay, error) {
	if smsProvider == nil || strings.TrimSpace(orderID) == "" {
		return nil, errors.New("OTP relay requires an acquired SMS resource")
	}
	if waitTimeout <= 0 {
		waitTimeout = 3 * time.Minute
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, errors.New("OTP relay token generation failed")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("OTP relay listener unavailable")
	}
	relay := &registrarOTPRelay{
		listener:    listener,
		url:         "http://" + listener.Addr().String() + "/v1/otp",
		bearerToken: hex.EncodeToString(tokenBytes),
		provider:    smsProvider,
		orderID:     orderID,
		waitTimeout: waitTimeout,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/otp", relay.handleOTP)
	relay.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      waitTimeout + 10*time.Second,
		IdleTimeout:       10 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	supervisor.GoOnce("registration-otp-relay-server", func() {
		_ = relay.server.Serve(listener)
	})
	supervisor.GoOnce("registration-otp-relay-shutdown", func() {
		<-ctx.Done()
		relay.Close()
	})
	return relay, nil
}

func (r *registrarOTPRelay) handleOTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	provided := strings.TrimSpace(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
	if len(provided) != len(r.bearerToken) ||
		subtle.ConstantTimeCompare([]byte(provided), []byte(r.bearerToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	code, err := r.waitCode(req.Context())
	if err != nil || strings.TrimSpace(code) == "" {
		http.Error(w, "OTP unavailable", http.StatusServiceUnavailable)
		return
	}
	r.consumed.Store(true)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}

func (r *registrarOTPRelay) waitCode(ctx context.Context) (string, error) {
	r.mu.Lock()
	if r.code != "" {
		code := r.code
		r.mu.Unlock()
		return code, nil
	}
	if r.inFlight != nil {
		wait := r.inFlight
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-wait:
			r.mu.Lock()
			code, err := r.code, r.lastErr
			r.mu.Unlock()
			return code, err
		}
	}
	wait := make(chan struct{})
	r.inFlight = wait
	r.lastErr = nil
	r.mu.Unlock()

	waitCtx, cancel := context.WithTimeout(ctx, r.waitTimeout)
	code, err := r.provider.WaitCode(waitCtx, r.orderID, r.waitTimeout)
	cancel()

	r.mu.Lock()
	if err == nil {
		r.code = strings.TrimSpace(code)
	}
	r.lastErr = err
	r.inFlight = nil
	close(wait)
	r.mu.Unlock()
	return strings.TrimSpace(code), err
}

func (r *registrarOTPRelay) Consumed() bool {
	return r != nil && r.consumed.Load()
}

func (r *registrarOTPRelay) Close() {
	if r == nil || r.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.server.Shutdown(ctx)
}
