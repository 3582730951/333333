package thinking

import "testing"

func TestParseLevelSuffixPreservesGPT56MaximumTiers(t *testing.T) {
	for input, want := range map[string]ThinkingLevel{
		"max":   LevelMax,
		"ultra": LevelUltra,
		"ULTRA": LevelUltra,
	} {
		got, ok := ParseLevelSuffix(input)
		if !ok || got != want {
			t.Fatalf("ParseLevelSuffix(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	if levelIndex("ultra") <= levelIndex("max") {
		t.Fatal("ultra must rank above max and must not clamp down to it")
	}
}
