package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicChatLinkCRUDAndValidation(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateUserGroupDefinition(ctx, UserGroup{
		ID:   "ug_public_chat",
		Name: "Public Chat Route",
		Targets: []TargetRef{{
			Kind: TargetKindAccountPoolGroup,
			ID:   "cyber",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	link, err := store.UpsertPublicChatLink(ctx, PublicChatLink{
		Slug:        "Demo_Chat",
		Name:        "Demo",
		Enabled:     true,
		RouteType:   PublicChatRouteUserGroup,
		UserGroupID: "ug_public_chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if link.ID == "" || link.Slug != "demo_chat" || link.RouteType != PublicChatRouteUserGroup || link.Model != DefaultPublicChatModel {
		t.Fatalf("normalized link mismatch: %+v", link)
	}
	if link.MaxHistoryMessages != DefaultPublicChatHistory || link.RateLimitPerMinute != DefaultPublicChatRateLimit {
		t.Fatalf("default limits mismatch: %+v", link)
	}

	got, found, err := store.GetPublicChatLinkBySlug(ctx, "DEMO_chat")
	if err != nil || !found || got.ID != link.ID {
		t.Fatalf("GetPublicChatLinkBySlug = %+v found=%v err=%v", got, found, err)
	}
	rows, err := store.ListPublicChatLinks(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListPublicChatLinks len=%d err=%v", len(rows), err)
	}

	updated, err := store.UpsertPublicChatLink(ctx, PublicChatLink{
		ID:                 link.ID,
		Slug:               "pool-demo",
		Name:               "Pool Demo",
		Enabled:            false,
		RouteType:          PublicChatRouteAccountPoolGroup,
		GroupName:          "cyber",
		Model:              "gpt-public",
		MaxHistoryMessages: 1000,
		RateLimitPerMinute: 1000,
		CreatedAt:          link.CreatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.UserGroupID != "" || updated.GroupName != "cyber" || updated.MaxHistoryMessages != MaxPublicChatHistory || updated.RateLimitPerMinute != MaxPublicChatRateLimit {
		t.Fatalf("updated route/limits mismatch: %+v", updated)
	}

	if _, err := store.UpsertPublicChatLink(ctx, PublicChatLink{
		Slug:        "missing-route",
		RouteType:   PublicChatRouteUserGroup,
		UserGroupID: "ug_missing",
	}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing route error = %v", err)
	}
	if _, err := store.UpsertPublicChatLink(ctx, PublicChatLink{
		Slug:      "../bad",
		RouteType: PublicChatRouteAccountPoolGroup,
		GroupName: "cyber",
	}); err == nil {
		t.Fatal("expected invalid slug error")
	}

	if err := store.DeletePublicChatLink(ctx, link.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.GetPublicChatLink(ctx, link.ID); err != nil || found {
		t.Fatalf("deleted link found=%v err=%v", found, err)
	}
}
