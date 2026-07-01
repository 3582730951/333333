// Package sentinel implements OpenAI Sentinel PoW challenge
package sentinel

import "fmt"

const (
	fnvOffsetBasis uint32 = 2166136261
	fnvPrime       uint32 = 16777619
)

// FNV1a32 computes FNV-1a 32-bit hash with extra mixing rounds
func FNV1a32(text string) string {
	h := fnvOffsetBasis
	for _, ch := range text {
		h ^= uint32(ch)
		h = (h * fnvPrime) & 0xFFFFFFFF
	}
	// Three rounds of XOR + multiply mixing
	h ^= h >> 16
	h = (h * 2246822507) & 0xFFFFFFFF
	h ^= h >> 13
	h = (h * 3266489909) & 0xFFFFFFFF
	h ^= h >> 16
	return fmt.Sprintf("%08x", h&0xFFFFFFFF)
}
