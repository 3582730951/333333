package sentinel

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"
)

const MaxAttempts = 500000

type Config []interface{}

// GenerateRequirementsToken creates a requirements token (nonce=1)
func GenerateRequirementsToken(userAgent, sid string) string {
	cfg := newConfig(userAgent, sid)
	cfg[3] = float64(1)
	cfg[9] = float64(rand.Intn(46) + 5)
	return "gAAAAAC" + encodeConfig(cfg)
}

// SolvePoW brute-forces a PoW token that satisfies the difficulty
func SolvePoW(seed, difficulty, userAgent, sid string) (string, error) {
	diff := difficulty
	if diff == "" {
		diff = "0"
	}
	start := time.Now()
	for i := 0; i < MaxAttempts; i++ {
		cfg := newConfig(userAgent, sid)
		cfg[3] = float64(i)
		cfg[9] = float64(time.Since(start).Milliseconds())
		payload := encodeConfig(cfg)
		hash := FNV1a32(seed + payload)
		if len(hash) >= len(diff) && hash[:len(diff)] <= diff {
			return "gAAAAAB" + payload + "~S", nil
		}
	}
	return "", fmt.Errorf("PoW exhausted %d attempts", MaxAttempts)
}

func newConfig(userAgent, sid string) Config {
	perfNow := rand.Float64()*49000 + 1000
	now := time.Now().UTC()
	plugins := []string{"plugins-undefined", "mimeTypes-undefined"}
	locs := []string{"location", "documentURI"}
	objs := []string{"Object", "parseFloat"}
	nums := []int{4, 8, 12, 16}
	return Config{
		"1920x1080",
		now.Format("Mon Jan 02 2006 15:04:05 GMT+0000"),
		float64(4294705152),
		float64(rand.Float64()),
		userAgent,
		"https://sentinel.openai.com/sentinel/20260124ceb8/sdk.js",
		nil,
		nil,
		"en-US",
		float64(rand.Float64()),
		plugins[rand.Intn(len(plugins))],
		locs[rand.Intn(len(locs))],
		objs[rand.Intn(len(objs))],
		perfNow,
		sid,
		"",
		float64(nums[rand.Intn(len(nums))]),
		float64(time.Now().UnixMilli()) - perfNow,
	}
}

func encodeConfig(cfg Config) string {
	b, _ := json.Marshal(cfg)
	return base64.StdEncoding.EncodeToString(b)
}
