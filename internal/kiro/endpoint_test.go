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
	for _, endpoint := range []string{
		"https://runtime.us-west-2.kiro.dev",
		"https://management.us-west-2.kiro.dev",
	} {
		if got, endpointErr := ValidateEndpoint(endpoint, "us-west-2", nil); endpointErr != nil || got != endpoint {
			t.Fatalf("official service endpoint=%q err=%v", got, endpointErr)
		}
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

func TestOperationEndpointsMatchKiroCLIAndTranslateLegacyOfficialHost(t *testing.T) {
	runtime, err := GenerateAssistantResponseEndpoint("", "eu-central-1", nil)
	if err != nil || runtime != "https://runtime.eu-central-1.kiro.dev/" {
		t.Fatalf("default runtime endpoint=%q err=%v", runtime, err)
	}
	management, err := ListAvailableModelsEndpoint("", "eu-central-1", nil)
	if err != nil || management != "https://management.eu-central-1.kiro.dev/" {
		t.Fatalf("default management endpoint=%q err=%v", management, err)
	}
	usage, err := GetUsageLimitsEndpoint("", "eu-central-1", nil)
	if err != nil || usage != management {
		t.Fatalf("default usage endpoint=%q management=%q err=%v", usage, management, err)
	}

	legacy := "https://q.us-west-2.amazonaws.com/generateAssistantResponse"
	runtime, err = GenerateAssistantResponseEndpoint(legacy, "us-east-1", nil)
	if err != nil || runtime != "https://runtime.us-west-2.kiro.dev/" {
		t.Fatalf("legacy official runtime endpoint=%q err=%v", runtime, err)
	}
	management, err = ListAvailableModelsEndpoint(legacy, "us-east-1", nil)
	if err != nil || management != "https://management.us-west-2.kiro.dev/" {
		t.Fatalf("legacy official management endpoint=%q err=%v", management, err)
	}

	custom := "http://127.0.0.1:9999/base/generateAssistantResponse"
	allowlist := []string{"127.0.0.1:9999"}
	runtime, err = GenerateAssistantResponseEndpoint(custom, "us-east-1", allowlist)
	if err != nil || runtime != custom {
		t.Fatalf("custom runtime endpoint=%q err=%v", runtime, err)
	}
	management, err = ListAvailableModelsEndpoint(custom, "us-east-1", allowlist)
	if err != nil || management != "http://127.0.0.1:9999/base/listAvailableModels" {
		t.Fatalf("custom catalog endpoint=%q err=%v", management, err)
	}
	usage, err = GetUsageLimitsEndpoint(custom, "us-east-1", allowlist)
	if err != nil || usage != "http://127.0.0.1:9999/base/getUsageLimits" {
		t.Fatalf("custom usage endpoint=%q err=%v", usage, err)
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
