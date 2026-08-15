package main

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestParseTiktokenBPE(t *testing.T) {
	ranks, err := parseTiktokenBPE([]byte("YQ== 0\nYg== 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ranks["a"] != 0 || ranks["b"] != 1 || len(ranks) != 2 {
		t.Fatalf("ranks=%v", ranks)
	}
	if _, err = parseTiktokenBPE([]byte("invalid-row")); err == nil {
		t.Fatal("invalid tokenizer row accepted")
	}
}

func TestVerifiedSHA256AndPinnedEncodingMetadata(t *testing.T) {
	raw := []byte("tokenizer")
	sum := sha256.Sum256(raw)
	if !verifiedSHA256(raw, hex.EncodeToString(sum[:])) {
		t.Fatal("valid SHA-256 rejected")
	}
	if verifiedSHA256(raw, o200kBaseSHA256) {
		t.Fatal("invalid SHA-256 accepted")
	}
	if tokenizerChecksum("o200k_base") != o200kBaseSHA256 || tokenizerChecksum("unknown") != "" {
		t.Fatal("encoding checksum metadata is not pinned")
	}
}

func TestMixedAgentFixtureCarriesSearchToolSkillAndCLIContract(t *testing.T) {
	body, err := marshalFixtureBody("gpt-5.6-sol", "extreme-007", "mixed-agent", "high-entropy-placeholder")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyMixedAgentFixture(body, "extreme-007"); err != nil {
		t.Fatalf("mixed-agent fixture contract failed: %v\n%s", err, body)
	}
	if got := fixtureVerificationKind("o200k_base", "mixed-agent"); got != "tiktoken:o200k_base:mixed-agent" {
		t.Fatalf("verification kind = %q", got)
	}
	if _, err := normalizeFixtureProfile("unknown"); err == nil {
		t.Fatal("unknown fixture profile was accepted")
	}
}

func TestMixedAgentFixtureVerificationRejectsOrphanedSkillOutput(t *testing.T) {
	body, err := marshalFixtureBody("gpt-5.6-sol", "extreme-008", "mixed-agent", "payload")
	if err != nil {
		t.Fatal(err)
	}
	for index := range body {
		if index+len("call_skill_extreme-008") <= len(body) && string(body[index:index+len("call_skill_extreme-008")]) == "call_skill_extreme-008" {
			body[index] = 'x'
		}
	}
	if err := verifyMixedAgentFixture(body, "extreme-008"); err == nil {
		t.Fatal("orphaned Skill output was accepted")
	}
}
