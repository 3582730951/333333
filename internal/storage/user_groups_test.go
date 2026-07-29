package storage

import (
	"context"
	"reflect"
	"testing"
)

func TestUserGroupCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Create
	g := UserGroup{
		ID: "ug_test001", Name: "mygroup",
		SystemPrompt: "You are helpful.", PromptMode: "prepend",
		SystemPromptApplyToCompaction: true,
		ModelInstructionProfiles: ModelInstructionProfiles{
			ModelInstructionFamilyGPT:    {Enabled: true, Files: []string{"gpt.md"}},
			ModelInstructionFamilyClaude: {Files: []string{"claude.md"}},
		},
		ForceModel: "", ForceEffort: "",
		BlockClaudeTargetGroups: []string{"claude-pool"},
		BlockGPTTargetGroups:    []string{"gpt-pool"},
	}
	if err := s.CreateUserGroup(ctx, g); err != nil {
		t.Fatal(err)
	}

	// Get by ID
	got, ok, err := s.GetUserGroup(ctx, "ug_test001")
	if err != nil || !ok {
		t.Fatalf("GetUserGroup: %v %v", ok, err)
	}
	if got.Name != "mygroup" || got.SystemPrompt != "You are helpful." {
		t.Errorf("unexpected: %+v", got)
	}
	if profile := got.ModelInstructionProfiles[ModelInstructionFamilyGPT]; !profile.Enabled || len(profile.Files) != 1 || profile.Files[0] != "gpt.md" {
		t.Errorf("model instruction profiles not persisted: %+v", got.ModelInstructionProfiles)
	}
	if !reflect.DeepEqual(got.BlockClaudeTargetGroups, []string{"claude-pool"}) ||
		!reflect.DeepEqual(got.BlockGPTTargetGroups, []string{"gpt-pool"}) {
		t.Errorf("target-family blocks not persisted: claude=%v gpt=%v", got.BlockClaudeTargetGroups, got.BlockGPTTargetGroups)
	}

	// Get by name
	got2, ok2, err2 := s.GetUserGroupByName(ctx, "mygroup")
	if err2 != nil || !ok2 || got2.ID != "ug_test001" {
		t.Errorf("GetUserGroupByName: %v %v %+v", ok2, err2, got2)
	}

	// List
	list, err := s.ListUserGroups(ctx)
	if err != nil || len(list) < 1 {
		t.Errorf("ListUserGroups: %v %d", err, len(list))
	}

	// Update
	g.SystemPrompt = "Updated."
	if err := s.UpdateUserGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	got3, _, _ := s.GetUserGroup(ctx, "ug_test001")
	if got3.SystemPrompt != "Updated." {
		t.Errorf("update not persisted: %q", got3.SystemPrompt)
	}

	// Delete
	if err := s.DeleteUserGroup(ctx, "ug_test001"); err != nil {
		t.Fatal(err)
	}
	_, ok4, _ := s.GetUserGroup(ctx, "ug_test001")
	if ok4 {
		t.Error("expected not found after delete")
	}
}

func TestUserGroupTargetFamilyBlocksAreScopedAndValidated(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if err := s.CreateGroup(ctx, Group{Name: "traffic-a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateGroup(ctx, Group{Name: "traffic-b"}); err != nil {
		t.Fatal(err)
	}
	group := UserGroup{
		ID:   "ug_target_family_blocks",
		Name: "target-family-blocks",
		Targets: []TargetRef{
			{Kind: TargetKindAccountPoolGroup, ID: "traffic-a"},
			{Kind: TargetKindAccountPoolGroup, ID: "traffic-b"},
		},
		BlockClaudeTargetGroups: []string{" traffic-a ", "traffic-a"},
		BlockGPTTargetGroups:    []string{"traffic-b"},
	}
	if err := s.CreateUserGroupDefinition(ctx, group); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.GetUserGroup(ctx, group.ID)
	if err != nil || !found {
		t.Fatalf("get user group found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(got.BlockClaudeTargetGroups, []string{"traffic-a"}) ||
		!reflect.DeepEqual(got.BlockGPTTargetGroups, []string{"traffic-b"}) {
		t.Fatalf("normalized blocks claude=%v gpt=%v", got.BlockClaudeTargetGroups, got.BlockGPTTargetGroups)
	}

	group.BlockClaudeTargetGroups = []string{"not-selected"}
	if err := s.ReplaceUserGroupDefinition(ctx, group); err == nil {
		t.Fatal("unselected account-pool block was accepted")
	}
}

func TestUserGroupRouteGenerationChangesWhenCapacityIsAdded(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if err := s.CreateGroup(ctx, Group{Name: "generation-pool"}); err != nil {
		t.Fatal(err)
	}
	group := UserGroup{
		ID:      "ug_route_generation",
		Name:    "route-generation",
		Targets: []TargetRef{{Kind: TargetKindAccountPoolGroup, ID: "generation-pool"}},
	}
	if err := s.CreateUserGroupDefinition(ctx, group); err != nil {
		t.Fatal(err)
	}
	before, err := s.UserGroupRouteGeneration(ctx, group.ID, "cyber")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAccount(ctx, Account{
		ID: "acc_generation", Label: "generation", GroupName: "generation-pool", Status: "active",
	}, AccountToken{AccountID: "acc_generation", AccessToken: "fixture-token"}); err != nil {
		t.Fatal(err)
	}
	after, err := s.UserGroupRouteGeneration(ctx, group.ID, "cyber")
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatalf("route generation did not change after account import: %q", before)
	}
}

func TestUserGroupTargetCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Create parent user_group
	if err := s.CreateUserGroup(ctx, UserGroup{ID: "ug_tgt01", Name: "tgtgroup", PromptMode: "prepend"}); err != nil {
		t.Fatal(err)
	}

	// Add targets
	tgt1 := UserGroupTarget{UserGroupID: "ug_tgt01", TargetType: UserGroupTargetTypeBaseGroup, TargetRef: "cyber", AffinityWeight: 2}
	tgt2 := UserGroupTarget{UserGroupID: "ug_tgt01", TargetType: UserGroupTargetTypeKiro, TargetRef: "", AffinityWeight: 1}
	for _, t2 := range []UserGroupTarget{tgt1, tgt2} {
		if err := s.UpsertUserGroupTarget(ctx, t2); err != nil {
			t.Fatalf("UpsertUserGroupTarget: %v", err)
		}
	}

	// List
	got, err := s.GetUserGroupTargets(ctx, "ug_tgt01")
	if err != nil || len(got) != 2 {
		t.Fatalf("expected 2 targets, got %d %v", len(got), err)
	}

	// Remove
	if err := s.RemoveUserGroupTarget(ctx, got[0].ID); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetUserGroupTargets(ctx, "ug_tgt01")
	if len(got2) != 1 {
		t.Errorf("expected 1 after remove, got %d", len(got2))
	}
}

func TestSetAPIKeyUserGroup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Create group + key
	if err := s.CreateUserGroup(ctx, UserGroup{ID: "ug_key01", Name: "keygroup", PromptMode: "prepend"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAPIKey(ctx, APIKey{KeyHash: "testhash01", GroupName: "cyber", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// Link
	if err := s.SetAPIKeyUserGroup(ctx, "testhash01", "ug_key01"); err != nil {
		t.Fatal(err)
	}

	// Verify via GetUserGroupForAPIKey
	ug, ok, err := s.GetUserGroupForAPIKey(ctx, "testhash01")
	if err != nil || !ok || ug.ID != "ug_key01" {
		t.Errorf("GetUserGroupForAPIKey: ok=%v err=%v id=%q", ok, err, ug.ID)
	}
}

func TestBackfillUserGroups(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// The store is already initialized (backfill ran). Create a legacy group + key.
	if err := s.CreateGroup(ctx, Group{Name: "backfilltest", PromptMode: "prepend", SystemPrompt: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAPIKey(ctx, APIKey{KeyHash: "bfhash01", GroupName: "backfilltest", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// Run backfill explicitly.
	if err := s.backfillUserGroups(ctx); err != nil {
		t.Fatal(err)
	}

	// Verify user_group was created with correct name.
	ug, ok, err := s.GetUserGroupByName(ctx, "backfilltest")
	if err != nil || !ok {
		t.Fatalf("user_group not created: ok=%v err=%v", ok, err)
	}
	if ug.SystemPrompt != "hello" {
		t.Errorf("system_prompt not migrated: %q", ug.SystemPrompt)
	}

	// Verify target was created.
	targets, tErr := s.GetUserGroupTargets(ctx, ug.ID)
	if tErr != nil || len(targets) == 0 {
		t.Fatalf("targets not created: %v %d", tErr, len(targets))
	}
	if targets[0].TargetType != UserGroupTargetTypeBaseGroup || targets[0].TargetRef != "backfilltest" {
		t.Errorf("unexpected target: %+v", targets[0])
	}

	// Verify api_key was re-pointed.
	key, ok2, kErr := s.LookupAPIKey(ctx, "bfhash01")
	if kErr != nil || !ok2 {
		t.Fatalf("key lookup failed: %v %v", ok2, kErr)
	}
	if key.UserGroupID == "" {
		t.Error("api_key.user_group_id not set after backfill")
	}
}

func TestLegacyUserGroupMigrationCanBeDisabled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateGroup(ctx, Group{Name: "do-not-copy", PromptMode: "prepend"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_POOL_MIGRATE_USER_GROUPS", "0")
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetUserGroupByName(ctx, "do-not-copy"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("legacy account-pool group was copied while migration was disabled")
	}

	t.Setenv("CODEX_POOL_MIGRATE_USER_GROUPS", "1")
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetUserGroupByName(ctx, "do-not-copy"); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("legacy account-pool group was not copied when migration was enabled")
	}
}
