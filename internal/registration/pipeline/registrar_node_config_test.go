package pipeline

import "testing"

func TestBuildNodeJobConfigWritesSelectedSMSBowerConfig(t *testing.T) {
	cfg := buildNodeJobConfig(map[string]interface{}{}, nodeJob{
		ProxyURL:        "http://user:pass@proxy.example:8000",
		ProfileDir:      "/tmp/profile",
		TokenDir:        "/tmp/tokens",
		FingerprintSeed: "seed",
		CountryISO:      "BR",
		CountryID:       "73",
		SMSProvider:     "smsbower",
		SMSConfig:       map[string]interface{}{"api_key": "bower-key", "service": "dr"},
	})

	if cfg["smsProvider"] != "smsbower" {
		t.Fatalf("smsProvider = %#v, want smsbower", cfg["smsProvider"])
	}
	if cfg["smsBowerApiKey"] != "bower-key" || cfg["smsbowerApiKey"] != "bower-key" {
		t.Fatalf("smsbower keys missing from config: %#v", cfg)
	}
	if _, ok := cfg["heroSmsApiKey"]; ok {
		t.Fatalf("smsbower config should not overwrite legacy heroSmsApiKey: %#v", cfg["heroSmsApiKey"])
	}
	if cfg["heroSmsCountry"] != "73" || cfg["phoneCountryCode"] != "BR" {
		t.Fatalf("country overlay = heroSmsCountry:%#v phoneCountryCode:%#v, want 73/BR", cfg["heroSmsCountry"], cfg["phoneCountryCode"])
	}
}

func TestBuildNodeJobConfigKeepsHeroSMSLegacyKeys(t *testing.T) {
	cfg := buildNodeJobConfig(map[string]interface{}{}, nodeJob{
		ProfileDir:      "/tmp/profile",
		TokenDir:        "/tmp/tokens",
		FingerprintSeed: "seed",
		CountryISO:      "CO",
		CountryID:       "33",
		SMSProvider:     "herosms",
		SMSConfig:       map[string]interface{}{"api_key": "hero-key", "service": "dr"},
	})

	if cfg["smsProvider"] != "herosms" {
		t.Fatalf("smsProvider = %#v, want herosms", cfg["smsProvider"])
	}
	if cfg["heroSmsApiKey"] != "hero-key" || cfg["heroSmsService"] != "dr" {
		t.Fatalf("hero legacy keys missing from config: %#v", cfg)
	}
	if cfg["heroSmsCountry"] != "33" || cfg["phoneCountryCode"] != "CO" {
		t.Fatalf("country overlay = heroSmsCountry:%#v phoneCountryCode:%#v, want 33/CO", cfg["heroSmsCountry"], cfg["phoneCountryCode"])
	}
}
