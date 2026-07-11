package kiro

import (
	"errors"
	"testing"
)

func TestValidateEndpointOfficialAndAllowlist(t *testing.T) {
	official, err := ValidateEndpoint("", "us-west-2", nil)
	if err != nil || official != "https://q.us-west-2.amazonaws.com" {
		t.Fatalf("official=%q err=%v", official, err)
	}
	if _, err := ValidateEndpoint("https://q.us-east-1.amazonaws.com.evil.test", "us-east-1", nil); !errors.Is(err, ErrEndpointNotAllowed) {
		t.Fatalf("lookalike host err=%v", err)
	}
	if _, err := ValidateEndpoint("http://127.0.0.1:9999", "us-east-1", nil); !errors.Is(err, ErrEndpointNotAllowed) {
		t.Fatalf("private endpoint escaped allowlist: %v", err)
	}
	allowed, err := ValidateEndpoint("http://127.0.0.1:9999/base", "us-east-1", []string{"127.0.0.1:9999"})
	if err != nil || allowed != "http://127.0.0.1:9999/base" {
		t.Fatalf("allowlisted=%q err=%v", allowed, err)
	}
	if _, err := ValidateEndpoint("http://user:secret@127.0.0.1:9999", "us-east-1", []string{"127.0.0.1:9999"}); !errors.Is(err, ErrEndpointNotAllowed) {
		t.Fatalf("userinfo endpoint accepted: %v", err)
	}
}

func TestEndpointHashIsStableAndHostSensitive(t *testing.T) {
	a, err := EndpointHash("https://q.us-east-1.amazonaws.com/", "us-east-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := EndpointHash("https://q.us-east-1.amazonaws.com", "us-east-1", nil)
	c, _ := EndpointHash("https://q.us-west-2.amazonaws.com", "us-west-2", nil)
	if a != b || a == c || len(a) != 64 {
		t.Fatalf("hashes a=%q b=%q c=%q", a, b, c)
	}
}

func TestValidateEndpointRejectsRegionAuthorityInjection(t *testing.T) {
	for _, region := range []string{"us-east-1@evil.test", "us-east-1/evil", "us.east.1", "-us-east-1", "us-east-1-"} {
		if _, err := ValidateEndpoint("", region, nil); !errors.Is(err, ErrEndpointNotAllowed) {
			t.Fatalf("region %q err=%v, want ErrEndpointNotAllowed", region, err)
		}
	}
	if got, err := ValidateEndpoint("", "US-EAST-1", nil); err != nil || got != "https://q.us-east-1.amazonaws.com" {
		t.Fatalf("uppercase official region got=%q err=%v", got, err)
	}
}
