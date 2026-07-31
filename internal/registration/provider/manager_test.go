package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

// mockSMSProvider implements SMSProvider + BalanceProvider + PriceProvider for testing
// the GetBestSMS selection algorithm without hitting a real platform.
type mockSMSProvider struct {
	namev     string
	bal       float64
	balErr    error
	tops      []smsCountryPrice
	topErr    error
	countries []smsCountryInfo
	// CountryID -> result (phone, orderID, err). Controls which GetNumber calls succeed.
	numberResults map[string]struct {
		phone   string
		orderID string
		err     error
	}
	calls []string // record GetNumber country-arg sequence
}

type mockMailboxProvider struct {
	name        string
	domains     []string
	email       string
	createErr   error
	createCalls int
	deleteCalls int
}

func (m *mockMailboxProvider) Name() string { return m.name }
func (m *mockMailboxProvider) Type() string { return "mailbox" }
func (m *mockMailboxProvider) MailboxDomains() []string {
	return append([]string(nil), m.domains...)
}
func (m *mockMailboxProvider) MailboxUsesCustomDomain() bool { return len(m.domains) > 0 }
func (m *mockMailboxProvider) CreateEmail(context.Context) (string, string, string, error) {
	m.createCalls++
	return m.email, "", m.name + "-lease", m.createErr
}
func (m *mockMailboxProvider) WaitOTP(context.Context, string, time.Duration) (string, error) {
	return "", nil
}
func (m *mockMailboxProvider) DeleteEmail(context.Context, string) error {
	m.deleteCalls++
	return nil
}

func (m *mockSMSProvider) Name() string { return m.namev }
func (m *mockSMSProvider) Type() string { return "sms" }
func (m *mockSMSProvider) GetNumber(ctx context.Context, country string) (string, string, error) {
	m.calls = append(m.calls, country)
	r, ok := m.numberResults[country]
	if !ok {
		return "", "", errors.New("NO_NUMBERS")
	}
	return r.phone, r.orderID, r.err
}
func (m *mockSMSProvider) WaitCode(ctx context.Context, orderID string, timeout time.Duration) (string, error) {
	return "", nil
}
func (m *mockSMSProvider) CancelNumber(ctx context.Context, orderID string) error { return nil }
func (m *mockSMSProvider) GetBalance(ctx context.Context) (float64, error) {
	return m.bal, m.balErr
}
func (m *mockSMSProvider) GetCountries(ctx context.Context) ([]interface{}, error) {
	out := make([]interface{}, 0, len(m.countries))
	for i := range m.countries {
		out = append(out, m.countries[i])
	}
	return out, nil
}
func (m *mockSMSProvider) GetTopCountries(ctx context.Context, service string) ([]interface{}, error) {
	if m.topErr != nil {
		return nil, m.topErr
	}
	out := make([]interface{}, 0, len(m.tops))
	for i := range m.tops {
		out = append(out, m.tops[i])
	}
	return out, nil
}

func TestGetBestSMS_PicksCheaperFundedPlatform(t *testing.T) {
	// hero-sms: funded, BR costs $0.045 (rank 0).
	herosms := &mockSMSProvider{
		namev: "herosms", bal: 1.50,
		tops: []smsCountryPrice{
			{Country: "73", Price: 0.045, Count: 100, Rank: 0}, // BR
			{Country: "79", Price: 0.10, Count: 50, Rank: 1},   // CO
		},
		countries: []smsCountryInfo{
			{ID: 73, ISO: "BR"}, {ID: 79, ISO: "CO"},
		},
		numberResults: map[string]struct {
			phone   string
			orderID string
			err     error
		}{"73": {"+551234", "hero-order", nil}},
	}
	// smsbower: funded, BR costs $0.030 (cheaper, also rank 0).
	smsbower := &mockSMSProvider{
		namev: "smsbower", bal: 2.00,
		tops: []smsCountryPrice{
			{Country: "73", Price: 0.030, Count: 200, Rank: 0}, // BR
		},
		countries: []smsCountryInfo{{ID: 73, ISO: "BR"}},
		numberResults: map[string]struct {
			phone   string
			orderID string
			err     error
		}{"73": {"+559999", "bower-order", nil}},
	}
	m := &Manager{SMS: []SMSProvider{herosms, smsbower}}

	// BR is preferred (highest priority). Both funded. smsbower is cheaper for BR.
	// Expect smsbower to win on score (same rank, cheaper price) → its number returned.
	p, phone, orderID, err := m.GetBestSMS(context.Background(), []string{"BR", "CO", "PL"}, 3)
	if err != nil {
		t.Fatalf("GetBestSMS: %v", err)
	}
	if p.Name() != "smsbower" {
		t.Errorf("expected cheaper platform smsbower, got %s", p.Name())
	}
	if phone != "+559999" || orderID != "bower-order" {
		t.Errorf("wrong number/order: %s / %s", phone, orderID)
	}
}

func TestGetBestSMS_SkipsUnfundedPlatform(t *testing.T) {
	// hero-sms funded, smsbower NO_BALANCE → must be skipped.
	herosms := &mockSMSProvider{
		namev: "herosms", bal: 1.00,
		tops:      []smsCountryPrice{{Country: "73", Price: 0.045, Count: 10, Rank: 0}},
		countries: []smsCountryInfo{{ID: 73, ISO: "BR"}},
		numberResults: map[string]struct {
			phone   string
			orderID string
			err     error
		}{"73": {"+551", "h", nil}},
	}
	smsbower := &mockSMSProvider{
		namev: "smsbower", bal: 0, // unfunded
		tops:      []smsCountryPrice{{Country: "73", Price: 0.001, Count: 999, Rank: 0}},
		countries: []smsCountryInfo{{ID: 73, ISO: "BR"}},
		numberResults: map[string]struct {
			phone   string
			orderID string
			err     error
		}{"73": {"+552", "b", nil}},
	}
	m := &Manager{SMS: []SMSProvider{herosms, smsbower}}
	p, _, _, err := m.GetBestSMS(context.Background(), []string{"BR"}, 3)
	if err != nil {
		t.Fatalf("GetBestSMS: %v", err)
	}
	if p.Name() != "herosms" {
		t.Errorf("expected funded herosms (smsbower unfunded), got %s", p.Name())
	}
	if len(smsbower.calls) != 0 {
		t.Errorf("unfunded smsbower should not have been called for a number, got calls %v", smsbower.calls)
	}
}

func TestGetBestSMS_FallsBackToNextCandidate(t *testing.T) {
	// First candidate returns NO_NUMBERS; second succeeds.
	herosms := &mockSMSProvider{
		namev: "herosms", bal: 1.00,
		tops: []smsCountryPrice{
			{Country: "79", Price: 0.05, Count: 10, Rank: 0},  // CO (cheaper, ranked first)
			{Country: "73", Price: 0.045, Count: 10, Rank: 1}, // BR
		},
		countries: []smsCountryInfo{{ID: 79, ISO: "CO"}, {ID: 73, ISO: "BR"}},
		// CO fails, BR succeeds → expect fallback.
		numberResults: map[string]struct {
			phone   string
			orderID string
			err     error
		}{
			"79": {"", "", errors.New("NO_NUMBERS")},
			"73": {"+55ok", "ok", nil},
		},
	}
	m := &Manager{SMS: []SMSProvider{herosms}}
	p, phone, _, err := m.GetBestSMS(context.Background(), []string{"BR", "CO", "PL"}, 3)
	if err != nil {
		t.Fatalf("GetBestSMS: %v", err)
	}
	if p.Name() != "herosms" || phone != "+55ok" {
		t.Errorf("expected fallback to BR, got %s/%s", p.Name(), phone)
	}
}

func TestGetBestSMS_NoneAvailable(t *testing.T) {
	m := &Manager{SMS: []SMSProvider{}}
	_, _, _, err := m.GetBestSMS(context.Background(), []string{"BR"}, 3)
	if !errors.Is(err, ErrNoProviderAvailable) {
		t.Errorf("expected ErrNoProviderAvailable, got %v", err)
	}
}

func TestGetSMSFromProviderOnlyCallsNamedPlatform(t *testing.T) {
	herosms := &mockSMSProvider{
		namev: "herosms",
		numberResults: map[string]struct {
			phone   string
			orderID string
			err     error
		}{"BR": {"+550001", "hero-order", nil}},
	}
	smsbower := &mockSMSProvider{
		namev: "smsbower",
		numberResults: map[string]struct {
			phone   string
			orderID string
			err     error
		}{"BR": {"+550002", "bower-order", nil}},
	}
	m := &Manager{SMS: []SMSProvider{herosms, smsbower}}

	p, phone, orderID, err := m.GetSMSFromProvider(context.Background(), "smsbower", "BR")
	if err != nil {
		t.Fatalf("GetSMSFromProvider: %v", err)
	}
	if p.Name() != "smsbower" || phone != "+550002" || orderID != "bower-order" {
		t.Fatalf("got provider=%s phone=%s order=%s, want smsbower/+550002/bower-order", p.Name(), phone, orderID)
	}
	if len(herosms.calls) != 0 {
		t.Fatalf("herosms should not be called when smsbower is selected, calls=%v", herosms.calls)
	}
	if len(smsbower.calls) != 1 || smsbower.calls[0] != "BR" {
		t.Fatalf("smsbower calls=%v, want [BR]", smsbower.calls)
	}
}

func TestGetMailboxWithConstraintsSkipsIncompatibleProvider(t *testing.T) {
	incompatible := &mockMailboxProvider{
		name: "other-domain", domains: []string{"other.test"}, email: "child@other.test",
	}
	compatible := &mockMailboxProvider{
		name: "team-domain", domains: []string{"example.test"}, email: "child@example.test",
	}
	manager := &Manager{Mailbox: []MailboxProvider{incompatible, compatible}}
	selected, email, _, _, err := manager.GetMailboxWithConstraints(
		context.Background(), "auto", "EXAMPLE.TEST.",
	)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Name() != "team-domain" || email != "child@example.test" {
		t.Fatalf("selected=%v email=%q", selected.Name(), email)
	}
	if incompatible.createCalls != 0 || compatible.createCalls != 1 {
		t.Fatalf("create calls incompatible=%d compatible=%d", incompatible.createCalls, compatible.createCalls)
	}
}

func TestGetMailboxWithConstraintsReleasesMismatchedAddress(t *testing.T) {
	mismatched := &mockMailboxProvider{
		name: "dynamic", email: "child@unexpected.test",
	}
	manager := &Manager{Mailbox: []MailboxProvider{mismatched}}
	_, _, _, _, err := manager.GetMailboxWithConstraints(
		context.Background(), "dynamic", "example.test",
	)
	if err == nil {
		t.Fatal("mismatched address was accepted")
	}
	if mismatched.createCalls != 1 || mismatched.deleteCalls != 1 {
		t.Fatalf("create=%d delete=%d", mismatched.createCalls, mismatched.deleteCalls)
	}
}
