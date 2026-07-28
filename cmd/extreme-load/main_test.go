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
