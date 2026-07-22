package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func adaptiveSidecarEgress() storage.EgressProfile {
	return storage.EgressProfile{
		ID:                             "exit-a",
		Type:                           storage.CurlCFFISidecarEgressType,
		TransportSidecarID:             "sidecar-a",
		TransportSidecarMaxConcurrency: 16,
	}
}

func TestSidecarAdaptiveControllerStartsAtFourAndQueuesFairly(t *testing.T) {
	controller := newSidecarAdaptiveController()
	egress := adaptiveSidecarEgress()
	leases := make([]*sidecarAdaptiveLease, 0, sidecarAdaptiveInitialLimit)
	for i := 0; i < sidecarAdaptiveInitialLimit; i++ {
		lease, err := controller.acquire(context.Background(), egress)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	if status := controller.statuses(); len(status) != 1 || status[0].Limit != sidecarAdaptiveInitialLimit || status[0].Inflight != sidecarAdaptiveInitialLimit {
		t.Fatalf("initial adaptive state = %+v", status)
	}

	acquired := make(chan *sidecarAdaptiveLease, 1)
	failure := make(chan error, 1)
	go func() {
		lease, err := controller.acquire(context.Background(), egress)
		if err != nil {
			failure <- err
			return
		}
		acquired <- lease
	}()
	select {
	case lease := <-acquired:
		lease.release(sidecarOutcomeNeutral)
		t.Fatal("fifth sidecar request bypassed initial adaptive limit")
	case err := <-failure:
		t.Fatalf("queued request failed before capacity freed: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	leases[0].release(sidecarOutcomeNeutral)
	select {
	case lease := <-acquired:
		lease.release(sidecarOutcomeNeutral)
	case err := <-failure:
		t.Fatalf("queued request did not acquire after release: %v", err)
	case <-time.After(time.Second):
		t.Fatal("queued sidecar request was not released")
	}
	for _, lease := range leases[1:] {
		lease.release(sidecarOutcomeNeutral)
	}
}

func TestSidecarAdaptiveControllerTripsAndHalfOpenSuccessRecovers(t *testing.T) {
	controller := newSidecarAdaptiveController()
	egress := adaptiveSidecarEgress()
	for i := 0; i < 3; i++ {
		lease, err := controller.acquire(context.Background(), egress)
		if err != nil {
			t.Fatal(err)
		}
		lease.release(sidecarOutcomeFailure)
	}
	_, err := controller.acquire(context.Background(), egress)
	var open *sidecarCircuitOpenError
	if !errors.As(err, &open) || !open.RetryAt.After(time.Now()) || !open.BypassUntil.After(open.RetryAt) {
		t.Fatalf("circuit error = %#v", err)
	}
	key, _, _ := sidecarAdaptiveIdentity(egress)
	controller.mu.Lock()
	controller.states[key].openUntil = time.Now().Add(-time.Millisecond)
	controller.mu.Unlock()
	probe, err := controller.acquire(context.Background(), egress)
	if err != nil {
		t.Fatalf("half-open probe = %v", err)
	}
	probe.release(sidecarOutcomeSuccess)
	status := controller.statuses()
	if len(status) != 1 || status[0].CircuitState != "closed" || status[0].RecentFailures != 0 || status[0].BypassUntil != 0 {
		t.Fatalf("successful half-open probe did not recover: %+v", status)
	}
}

func TestSidecarAdaptiveControllerCachesBypassOnlyAfterCircuitThreshold(t *testing.T) {
	controller := newSidecarAdaptiveController()
	egress := adaptiveSidecarEgress()

	first, err := controller.acquire(context.Background(), egress)
	if err != nil {
		t.Fatal(err)
	}
	first.release(sidecarOutcomeFailure)
	controller.enableBypass(egress)
	lease, err := controller.acquire(context.Background(), egress)
	if err != nil {
		t.Fatalf("one failure must not cache a bypass: %v", err)
	}
	lease.release(sidecarOutcomeNeutral)

	for i := 0; i < 2; i++ {
		lease, err := controller.acquire(context.Background(), egress)
		if err != nil {
			t.Fatal(err)
		}
		lease.release(sidecarOutcomeFailure)
	}
	controller.enableBypass(egress)
	_, err = controller.acquire(context.Background(), egress)
	var circuit *sidecarCircuitOpenError
	if !errors.As(err, &circuit) || !circuit.BypassReady {
		t.Fatalf("three failures must activate the cached bypass: %#v", err)
	}
}

func TestSidecarDirectBypassRequiresSameProxyAndNoCustomFingerprint(t *testing.T) {
	wrapped := storage.EgressProfile{
		ID:                             "exit-a",
		Type:                           storage.CurlCFFISidecarEgressType,
		TransportSidecarID:             "sidecar-a",
		TransportBaseType:              "socks5h_proxy",
		TransportBaseURL:               "socks5h://127.0.0.1:40000",
		TransportBaseChain:             "",
		DynamicConfigJSON:              "{}",
		TransportSidecarMaxConcurrency: 16,
	}
	if !sidecarDirectBypassAllowed(wrapped, "") {
		t.Fatal("bound SOCKS chain should permit a same-exit bypass")
	}
	if sidecarDirectBypassAllowed(wrapped, "771,4865-4866,0,0,0") {
		t.Fatal("custom JA3 must forbid Go transport bypass")
	}
	if sidecarDirectBypassAllowed(storage.EgressProfile{ID: "sidecar", Type: storage.CurlCFFISidecarEgressType}, "") {
		t.Fatal("sidecar without a real chained proxy must not fall back host-direct")
	}
}

func TestSidecarPreHeaderRecoveryRequiresExplicitDeliveryCertification(t *testing.T) {
	tests := []struct {
		name string
		resp *http.Response
		want bool
	}{
		{
			name: "certified preflight",
			resp: &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{
				"X-Sidecar-Error-Code":      []string{"sidecar_preflight_error"},
				"X-Sidecar-Error-Retryable": []string{"true"},
			}},
			want: true,
		},
		{
			name: "delivery unknown",
			resp: &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{
				"X-Sidecar-Error-Code":      []string{"sidecar_delivery_unknown"},
				"X-Sidecar-Error-Retryable": []string{"false"},
			}},
			want: false,
		},
		{
			name: "unstructured 5xx",
			resp: &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, got := sidecarPreHeaderFailure(test.resp)
			if got != test.want {
				t.Fatalf("safe recovery=%v, want %v", got, test.want)
			}
		})
	}
}

func TestSidecarStructuredPreHeaderFailureRecoversOnlyThroughBoundProxy(t *testing.T) {
	previousWindow := sidecarPreHeaderRecoveryWindow
	sidecarPreHeaderRecoveryWindow = 350 * time.Millisecond
	t.Cleanup(func() { sidecarPreHeaderRecoveryWindow = previousWindow })

	var sidecarCalls, proxyCalls atomic.Int32
	var proxyWasDirect atomic.Bool
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sidecarCalls.Add(1)
		w.Header().Set("X-Sidecar-Error-Code", "sidecar_request_error")
		w.Header().Set("X-Sidecar-Error-Phase", "request")
		w.Header().Set("X-Sidecar-Error-Retryable", "true")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"code":"sidecar_request_error"}}`))
	}))
	defer sidecar.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if r.URL.Scheme == "" || r.URL.Host == "" {
			proxyWasDirect.Store(true)
		}
		_, _ = w.Write([]byte("safe chained bypass"))
	}))
	defer proxy.Close()

	client := NewClient(config.Default())
	egress := storage.EgressProfile{
		ID:                             "exit-a",
		Type:                           storage.CurlCFFISidecarEgressType,
		Endpoint:                       sidecar.URL,
		TransportSidecarID:             "sidecar-a",
		TransportSidecarMaxConcurrency: 16,
		TransportBaseType:              "http_proxy",
		TransportBaseURL:               proxy.URL,
	}
	response, err := client.postViaSidecar(context.Background(), Request{
		Method: http.MethodPost,
		Body:   []byte(`{"input":"safe"}`),
		Egress: egress,
	}, "http://upstream.invalid/v1/responses", http.Header{"Content-Type": []string{"application/json"}}, time.Second, "", false)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !bytes.Contains(body, []byte("safe chained bypass")) || sidecarCalls.Load() < 3 || proxyCalls.Load() != 1 || proxyWasDirect.Load() {
		t.Fatalf("safe bypass body=%q sidecar=%d proxy=%d direct=%v err=%v", body, sidecarCalls.Load(), proxyCalls.Load(), proxyWasDirect.Load(), err)
	}
}
