package api

import "testing"

func TestParseKiroImportJSONShapesAndPartialFailure(t *testing.T) {
	raw := []byte(`{"accounts":[{"email":"a@x","refreshToken":"r1"},{"credentials":{"authMethod":"builder-id","refresh_token":"r2","client_id":"c","client_secret":"s"},"proxyUrl":"http://ignored"},{"authMethod":"apikey","kiroApiKey":"ksk_x"},{"authMethod":"idc","refreshToken":"missing-client"}]}`)
	items, err := parseKiroImportJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("items=%d", len(items))
	}
	if items[0].AuthMethod != "social" || items[1].AuthMethod != "idc" || !items[1].ProxyIgnored || items[2].AuthMethod != "api_key" {
		t.Fatalf("items=%+v", items)
	}
	if items[3].ParseError == "" {
		t.Fatal("bad item must be retained as a per-item failure")
	}
}
func TestKiroCredentialHashStableAndAuthAliases(t *testing.T) {
	a := kiroImportItem{AuthMethod: "social", RefreshToken: "r"}
	if kiroCredentialHash(a) != kiroCredentialHash(a) {
		t.Fatal("hash changed")
	}
	for in, want := range map[string]string{"builder-id": "idc", "iam": "idc", "apikey": "api_key", "API_KEY": "api_key"} {
		if got := normalizeKiroAuthMethod(in); got != want {
			t.Fatalf("%s => %s", in, got)
		}
	}
}

func TestMergeKiroTwoFileIdCCredentials(t *testing.T) {
	items, err := parseKiroImportJSON([]byte(`{"authMethod":"IdC","refreshToken":"refresh-placeholder","clientIdHash":"hash-one","region":"us-east-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ParseError == "" {
		t.Fatalf("token-only IdC input should require its registration file: %+v", items)
	}
	registration := []byte(`{"clientId":"client-placeholder","clientSecret":"secret-placeholder","expiresAt":"2099-01-01T00:00:00Z"}`)
	if err := mergeKiroClientRegistrationJSON(items, registration); err != nil {
		t.Fatal(err)
	}
	if items[0].ParseError != "" || items[0].ClientID != "client-placeholder" || items[0].ClientSecret != "secret-placeholder" {
		t.Fatalf("two-file credentials were not merged: %+v", items[0])
	}
}

func TestMergeKiroClientRegistrationsByHash(t *testing.T) {
	items, err := parseKiroImportJSON([]byte(`[{"authMethod":"idc","refreshToken":"r1","clientIdHash":"hash-a"},{"authMethod":"idc","refreshToken":"r2","clientIdHash":"hash-b"}]`))
	if err != nil {
		t.Fatal(err)
	}
	registrations := []byte(`{"hash-b":{"clientId":"client-b","clientSecret":"secret-b"},"hash-a":{"clientId":"client-a","clientSecret":"secret-a"}}`)
	if err := mergeKiroClientRegistrationJSON(items, registrations); err != nil {
		t.Fatal(err)
	}
	if items[0].ClientID != "client-a" || items[1].ClientID != "client-b" || items[0].ParseError != "" || items[1].ParseError != "" {
		t.Fatalf("hash registrations were mismatched: %+v", items)
	}
}

func TestMergeKiroClientRegistrationRejectsIncompleteJSON(t *testing.T) {
	items, _ := parseKiroImportJSON([]byte(`{"authMethod":"idc","refreshToken":"r","clientIdHash":"hash"}`))
	if err := mergeKiroClientRegistrationJSON(items, []byte(`{"clientId":"client-only"}`)); err == nil {
		t.Fatal("incomplete client registration must fail")
	}
}
