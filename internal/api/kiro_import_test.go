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
