package cf

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestDetectCloudflareSignals(t *testing.T) {
	h := http.Header{}
	h.Set("cf-mitigated", "challenge")
	h.Set("cf-ray", "ray-1")
	d := Detect(403, h, []byte("<html>Just a moment</html>"))
	if !d.Matched || d.CFRay != "ray-1" {
		t.Fatalf("unexpected detection: %+v", d)
	}
}

func TestDetectDoesNotTreatBareCFRayAsCloudflareBlock(t *testing.T) {
	h := http.Header{}
	h.Set("cf-ray", "ray-json")
	h.Set("server", "cloudflare")
	h.Set("Content-Type", "application/json")
	d := Detect(401, h, []byte(`{"error":{"code":"refresh_token_invalidated","message":"Your refresh token has been invalidated."}}`))
	if d.Matched {
		t.Fatalf("ordinary JSON auth error was classified as CF: %+v", d)
	}
}

func TestDetectBareCFRayChallengeIsEdgeOnly(t *testing.T) {
	h := http.Header{}
	h.Set("cf-ray", "ray-edge")
	d := Detect(403, h, []byte(`{"error":"challenge"}`))
	if !EdgeOnly(d) || Recordable(d) {
		t.Fatalf("cf-ray challenge should be edge-only, not recordable: %+v", d)
	}
}

func TestDetectCloudflareHTMLInterstitial(t *testing.T) {
	h := http.Header{}
	h.Set("cf-ray", "ray-html")
	h.Set("server", "cloudflare")
	h.Set("Content-Type", "text/html")
	d := Detect(403, h, []byte("<html><title>Access denied</title><body>Request blocked by Cloudflare</body></html>"))
	if !d.Matched || d.CFRay != "ray-html" || d.Category != "cf_body" {
		t.Fatalf("HTML interstitial was not classified as CF: %+v", d)
	}
}

func TestDetectJSONMentioningCloudflareNotBlocked(t *testing.T) {
	// A JSON API error that merely contains the word "cloudflare"/"captcha" must NOT
	// be classified as a CF block — that false positive is what benched healthy
	// accounts ("动不动就冷却"). Broad words only count inside an HTML interstitial.
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	d := Detect(403, h, []byte(`{"error":{"message":"upstream cloudflare reported a captcha somewhere"}}`))
	if d.Matched {
		t.Fatalf("JSON error mentioning cloudflare/captcha must not be a CF block: %+v", d)
	}
}

func TestDetectHTMLCaptchaStillBlocked(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "text/html")
	h.Set("cf-ray", "ray-x")
	d := Detect(403, h, []byte("<html><body>Please complete the captcha</body></html>"))
	if !d.Matched || d.Category != "cf_body" {
		t.Fatalf("HTML captcha page should still be a CF block: %+v", d)
	}
}

func TestStormBreakerThresholds(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", Label: "a", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	breaker := StormBreaker{Store: store}
	d := Detection{Matched: true, Category: "cf_challenge", CFRay: "ray"}
	if err := breaker.Record(ctx, "acc-1", storage.DefaultDirectEgressID, 403, d); err != nil {
		t.Fatal(err)
	}
	binding, _ := store.GetEgressBinding(ctx, "acc-1")
	if binding.CooldownUntil <= storage.Now() {
		t.Fatalf("binding cooldown not set: %+v", binding)
	}
	if err := breaker.Record(ctx, "acc-1", storage.DefaultDirectEgressID, 403, d); err != nil {
		t.Fatal(err)
	}
	binding, _ = store.GetEgressBinding(ctx, "acc-1")
	if binding.CooldownUntil < storage.Now()+25*60 {
		t.Fatalf("binding 30m cooldown not applied: %+v", binding)
	}
}
