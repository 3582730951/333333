package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"codex-account-pool/internal/registration/provider"
)

type pipelineSMSProvider struct {
	name          string
	calls         []string
	completed     int
	cancelled     int
	lastSettledID string
}

func (p *pipelineSMSProvider) Name() string { return p.name }
func (p *pipelineSMSProvider) Type() string { return "sms" }
func (p *pipelineSMSProvider) GetNumber(ctx context.Context, country string) (string, string, error) {
	p.calls = append(p.calls, country)
	return "+550000", p.name + "-order", nil
}
func (p *pipelineSMSProvider) WaitCode(ctx context.Context, orderID string, timeout time.Duration) (string, error) {
	return "123456", nil
}
func (p *pipelineSMSProvider) CancelNumber(ctx context.Context, orderID string) error {
	p.cancelled++
	p.lastSettledID = orderID
	return nil
}
func (p *pipelineSMSProvider) CompleteNumber(ctx context.Context, orderID string) error {
	p.completed++
	p.lastSettledID = orderID
	return nil
}

func TestAcquireSMSRespectsRequestedProvider(t *testing.T) {
	hero := &pipelineSMSProvider{name: "herosms"}
	bower := &pipelineSMSProvider{name: "smsbower"}
	p := NewPipeline(nil, &provider.Manager{SMS: []provider.SMSProvider{hero, bower}}, nil, nil)

	got, _, orderID, err := p.acquireSMS(context.Background(), RegisterRequest{
		SMSProvider: "smsbower",
		Country:     "BR",
	})
	if err != nil {
		t.Fatalf("acquireSMS: %v", err)
	}
	if got.Name() != "smsbower" || orderID != "smsbower-order" {
		t.Fatalf("provider/order = %s/%s, want smsbower/smsbower-order", got.Name(), orderID)
	}
	if len(hero.calls) != 0 {
		t.Fatalf("herosms should not be called when smsbower is requested, calls=%v", hero.calls)
	}
	if len(bower.calls) != 1 || bower.calls[0] != "BR" {
		t.Fatalf("smsbower calls = %v, want [BR]", bower.calls)
	}
}

func TestAcquireSMSUnknownRequestedProviderErrors(t *testing.T) {
	p := NewPipeline(nil, &provider.Manager{SMS: []provider.SMSProvider{&pipelineSMSProvider{name: "herosms"}}}, nil, nil)
	_, _, _, err := p.acquireSMS(context.Background(), RegisterRequest{SMSProvider: "smsbower", Country: "BR"})
	if err == nil || !errors.Is(err, provider.ErrNoProviderAvailable) {
		t.Fatalf("err = %v, want ErrNoProviderAvailable", err)
	}
}

func TestSettleSMSLeaseCompletesConsumedNumberExactlyOnce(t *testing.T) {
	smsProvider := &pipelineSMSProvider{name: "herosms"}
	p := NewPipeline(nil, nil, nil, nil)
	p.settleSMSLease(context.Background(), RegisterRequest{}, smsProvider, "order-1", true)

	if smsProvider.completed != 1 || smsProvider.cancelled != 0 || smsProvider.lastSettledID != "order-1" {
		t.Fatalf("settlement = completed:%d cancelled:%d id:%q", smsProvider.completed, smsProvider.cancelled, smsProvider.lastSettledID)
	}
}

func TestSettleSMSLeaseCancelsUnconsumedNumberExactlyOnce(t *testing.T) {
	smsProvider := &pipelineSMSProvider{name: "herosms"}
	p := NewPipeline(nil, nil, nil, nil)
	p.settleSMSLease(context.Background(), RegisterRequest{}, smsProvider, "order-2", false)

	if smsProvider.completed != 0 || smsProvider.cancelled != 1 || smsProvider.lastSettledID != "order-2" {
		t.Fatalf("settlement = completed:%d cancelled:%d id:%q", smsProvider.completed, smsProvider.cancelled, smsProvider.lastSettledID)
	}
}
