package ban

import (
	"net/http"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		ok     bool
		status int
		body   string
		want   State
	}{
		{"success", true, 200, `{"ok":true}`, Alive},
		{"account_deactivated", false, 403, `{"error":{"code":"account_deactivated","message":"Your account was deactivated"}}`, Banned},
		{"workspace_deactivated", false, 401, `{"detail":"workspace_deactivated"}`, Banned},
		{"suspended", false, 403, `{"error":"Your account has been disabled for a policy violation"}`, Banned},
		{"bare deactivated on 403", false, 403, `the workspace was deactivated`, Banned},
		{"region block is not a ban", false, 403, `{"error":{"code":"unsupported_country_region_territory"}}`, RegionBlocked},
		{"missing scope is not token expiry", false, 401, `{"error":{"message":"You have insufficient permissions for this operation. Missing scopes: api.responses.write."}}`, PermissionDenied},
		{"token expired is not a ban", false, 401, `{"error":{"code":"invalid_grant"}}`, AuthExpired},
		{"usage limit", false, 429, `{"error":"You've hit your usage limit. Try again later."}`, RateLimited},
		{"bare 429", false, 429, `slow down`, RateLimited},
		{"payment required", false, 402, `{"error":"balance exhausted"}`, RateLimited},
		{"bare 401", false, 401, `unauthorized`, AuthExpired},
		{"unrelated 400", false, 400, `{"error":{"code":"model_not_found"}}`, Unknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.ok, c.status, nil, []byte(c.body))
			if got.State != c.want {
				t.Fatalf("Classify(%q) = %q (%s), want %q", c.body, got.State, got.Reason, c.want)
			}
		})
	}
}

func TestClassifyHeaderRegion(t *testing.T) {
	h := http.Header{}
	h.Set("x-error-json", `{"identity_error_code":"region_restricted"}`)
	if got := Classify(false, 403, h, []byte(`forbidden`)); got.State != RegionBlocked {
		t.Fatalf("header region block = %q", got.State)
	}
}

func TestBannedNeverFromRecoverable(t *testing.T) {
	// A rate-limit body that happens to be a 403 must not be read as a ban.
	if got := Classify(false, 403, nil, []byte(`{"error":"quota exceeded"}`)); got.IsBanned() {
		t.Fatalf("quota wrongly classified as ban")
	}
}

func TestClassifyAWSUserSuspendedFromKiro503(t *testing.T) {
	body := `{"message":"Your AWS Builder ID temporarily is suspended. We have locked your account as a security precaution. To restore access, contact AWS Support at https://support.aws.amazon.com/ and complete verification of your identity."}`
	got := Classify(false, http.StatusServiceUnavailable, nil, []byte(body))
	if got.State != Banned || got.Reason != "aws_user_suspended" {
		t.Fatalf("AWS suspension verdict = %+v, want banned/aws_user_suspended", got)
	}
}

func TestClassifyDoesNotTreatIncidentalSuspendedAsAWSUserSuspension(t *testing.T) {
	for _, body := range []string{
		`{"message":"service temporarily unavailable"}`,
		`{"message":"the background job is suspended while the model is overloaded"}`,
		`{"message":"contact support if this suspended animation demo fails"}`,
	} {
		got := Classify(false, http.StatusServiceUnavailable, nil, []byte(body))
		if got.IsBanned() || got.Reason == "aws_user_suspended" {
			t.Fatalf("incidental body %q classified as %+v", body, got)
		}
	}
}

// TestIsAccountLevel verifies the new IsAccountLevel method distinguishes
// account-level failures (ban, auth expiry) from function-level restrictions
// (permission denied, region block).
func TestIsAccountLevel(t *testing.T) {
	tests := []struct {
		state State
		want  bool
	}{
		{Alive, false},
		{Banned, true},
		{AuthExpired, true},
		{PermissionDenied, false},
		{RegionBlocked, false},
		{RateLimited, false},
		{Unknown, false},
	}
	for _, tt := range tests {
		v := Verdict{State: tt.state}
		if got := v.IsAccountLevel(); got != tt.want {
			t.Errorf("Verdict{State: %v}.IsAccountLevel() = %v, want %v", tt.state, got, tt.want)
		}
	}
}
