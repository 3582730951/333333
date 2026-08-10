package upstream

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"codex-account-pool/internal/jsonview"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const antigravityZeroWidthSpace = "\u200b"

// obfuscateAntigravitySystemWords mirrors the native compatibility treatment:
// configured terms are visually preserved but split with a zero-width space in
// systemInstruction only. User messages and tool payloads remain byte-identical.
func obfuscateAntigravitySystemWords(payload []byte, words []string) []byte {
	valid := make([]string, 0, len(words))
	for _, word := range words {
		word = strings.TrimSpace(word)
		if utf8.RuneCountInString(word) >= 2 && !strings.Contains(word, antigravityZeroWidthSpace) {
			valid = append(valid, word)
		}
	}
	if len(valid) == 0 {
		return payload
	}
	sort.Slice(valid, func(i, j int) bool { return len(valid[i]) > len(valid[j]) })
	escaped := make([]string, len(valid))
	for i, word := range valid {
		escaped[i] = regexp.QuoteMeta(word)
	}
	matcher, err := regexp.Compile("(?i)" + strings.Join(escaped, "|"))
	if err != nil {
		return payload
	}
	parts := jsonview.Get(payload, "request.systemInstruction.parts")
	if !parts.IsArray() {
		return payload
	}
	parts.ForEach(func(index, part gjson.Result) bool {
		text := part.Get("text")
		if text.Type != gjson.String {
			return true
		}
		original := text.String()
		updated := matcher.ReplaceAllStringFunc(original, func(word string) string {
			_, size := utf8.DecodeRuneInString(word)
			if size <= 0 || size >= len(word) {
				return word
			}
			return word[:size] + antigravityZeroWidthSpace + word[size:]
		})
		if updated != original {
			payload, _ = sjson.SetBytes(payload, "request.systemInstruction.parts."+index.String()+".text", updated)
		}
		return true
	})
	return payload
}
