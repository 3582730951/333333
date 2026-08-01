package storage

import (
	"errors"
	"testing"
)

func TestCustomProviderRoutesRoundTripAndResolve(t *testing.T) {
	store := newTestStore(t)
	provider := CustomProvider{
		ID: "multi-route", Name: "Multi Route", BaseURL: "https://default.example/v1",
		UpstreamProtocol: CustomProviderProtocolChatCompletions,
		TransportProfile: CustomProviderTransportGeneric,
		Enabled:          true,
		Routes: []CustomProviderRoute{
			{
				DownstreamPath: "responses", BaseURL: "https://responses.example/openai/v1",
				UpstreamProtocol: CustomProviderProtocolResponses,
				TransportProfile: CustomProviderTransportCodexCLI,
			},
			{
				ID: "claude-edge", DownstreamPath: "/v1/messages",
				BaseURL:          "https://messages.example/anthropic/v1",
				UpstreamProtocol: CustomProviderProtocolAnthropicMessages,
				TransportProfile: CustomProviderTransportClaudeCode,
			},
			{DownstreamPath: "passthrough"},
		},
	}
	if err := store.UpsertCustomProvider(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.GetCustomProvider(t.Context(), provider.ID)
	if err != nil || !found {
		t.Fatalf("get provider: found=%v err=%v", found, err)
	}
	if len(got.Routes) != 3 || got.Routes[0].ID != "responses" ||
		got.Routes[0].DownstreamPath != CustomProviderDownstreamResponses ||
		got.Routes[2].ID != "passthrough" || got.Routes[2].BaseURL != provider.BaseURL {
		t.Fatalf("normalized routes = %+v", got.Routes)
	}
	responses, routeID := ResolveCustomProviderRoute(got, "/v1/responses")
	if routeID != "responses" || responses.BaseURL != "https://responses.example/openai/v1" ||
		responses.UpstreamProtocol != CustomProviderProtocolResponses ||
		responses.ResolvedDownstreamPath != CustomProviderDownstreamResponses {
		t.Fatalf("responses route = id=%q provider=%+v", routeID, responses)
	}
	passthrough, routeID := ResolveCustomProviderRoute(got, "/v1/files/file-1")
	if routeID != "passthrough" || passthrough.BaseURL != provider.BaseURL ||
		passthrough.ResolvedDownstreamPath != CustomProviderDownstreamWildcard {
		t.Fatalf("passthrough route = id=%q provider=%+v", routeID, passthrough)
	}
}

func TestCustomProviderRoutesRejectAmbiguousEntries(t *testing.T) {
	store := newTestStore(t)
	base := CustomProvider{
		ID: "ambiguous-routes", BaseURL: "https://default.example/v1",
		UpstreamProtocol: CustomProviderProtocolChatCompletions,
		TransportProfile: CustomProviderTransportGeneric,
		Enabled:          true,
	}
	base.Routes = []CustomProviderRoute{
		{DownstreamPath: "responses"},
		{DownstreamPath: "/v1/responses"},
	}
	if err := store.UpsertCustomProvider(t.Context(), base); !errors.Is(err, ErrInvalidProviderRoute) {
		t.Fatalf("duplicate path error = %v, want ErrInvalidProviderRoute", err)
	}
	base.Routes = []CustomProviderRoute{{ID: "bad id", DownstreamPath: "/v1/messages"}}
	if err := store.UpsertCustomProvider(t.Context(), base); !errors.Is(err, ErrInvalidProviderRoute) {
		t.Fatalf("invalid id error = %v, want ErrInvalidProviderRoute", err)
	}
}
