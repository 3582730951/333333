package registration

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/big"
	"strings"
	"time"
)

// Sentinel PoW token generator — ported from Python protocol_sentinel.py.
// Implements OpenAI's sentinel proof-of-work challenge/response flow.

const sentinelUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
const sentinelSDKURL = "https://sentinel.openai.com/sentinel/20260124ceb8/sdk.js"
const sentinelReqURL = "https://sentinel.openai.com/backend-api/sentinel/req"

type SentinelGenerator struct {
	DeviceID         string
	RequirementsSeed string
	SID              string
}

func NewSentinelGenerator(deviceID string) *SentinelGenerator {
	if deviceID == "" {
		deviceID = randomUUID()
	}
	return &SentinelGenerator{
		DeviceID:         deviceID,
		RequirementsSeed: fmt.Sprintf("%f", randomFloat64()),
		SID:              randomUUID(),
	}
}

func (g *SentinelGenerator) getConfig() []interface{} {
	screenInfo := "1920x1080"
	now := time.Now().UTC()
	dateStr := now.Format("Mon Jan 02 2006 15:04:05 GMT-0700 (Coordinated Universal Time)")
	dateStr = strings.Replace(dateStr, "GMT+0000", "GMT+0000", 1)
	config := []interface{}{
		screenInfo, dateStr, float64(4294705152), randomFloat64(),
		sentinelUserAgent, sentinelSDKURL,
		nil, nil, "en-US", "en-US,en", randomFloat64(),
		randomChoice([]string{"vendorSub", "productSub", "vendor", "maxTouchPoints"}) + "−undefined",
		randomChoice([]string{"location", "implementation", "URL", "documentURI", "compatMode"}),
		randomChoice([]string{"Object", "Function", "Array", "Number", "parseFloat", "undefined"}),
		randomFloat64()*49000 + 1000, g.SID, "",
		float64(randomInt(4, 16)), float64(time.Now().UnixMilli()) - randomFloat64()*49000,
	}
	return config
}

func (g *SentinelGenerator) base64Encode(data interface{}) string {
	raw, _ := json.Marshal(data)
	return base64.StdEncoding.EncodeToString(raw)
}

func (g *SentinelGenerator) runCheck(startTime float64, seed, difficulty string, config []interface{}, nonce int) string {
	c := make([]interface{}, len(config))
	copy(c, config)
	c[3] = nonce
	c[9] = float64(int64((float64(time.Now().UnixMilli())/1000.0-startTime)*1000)) / 1000.0 * 1000
	data := g.base64Encode(c)
	hashHex := fnv1a32Hex(seed + data)
	if strings.Compare(hashHex[:len(difficulty)], difficulty) <= 0 {
		return data + "~S"
	}
	return ""
}

func (g *SentinelGenerator) GenerateToken(seed, difficulty string) string {
	if seed == "" {
		seed = g.RequirementsSeed
		difficulty = "0"
	}
	startTime := float64(time.Now().UnixMilli()) / 1000.0
	config := g.getConfig()
	for i := 0; i < 500000; i++ {
		result := g.runCheck(startTime, seed, difficulty, config, i)
		if result != "" {
			return "gAAAAAB" + result
		}
	}
	return "gAAAAAB" + "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + g.base64Encode("None")
}

func (g *SentinelGenerator) GenerateRequirementsToken() string {
	config := g.getConfig()
	config[3] = 1
	config[9] = float64(int64(randomFloat64()*45 + 5))
	return "gAAAAAC" + g.base64Encode(config)
}

// FetchSentinelChallenge calls the sentinel backend to get a challenge.
func FetchSentinelChallenge(ctx context.Context, sidecar *SidecarHTTPClient, deviceID, flow, proxyURL string) (map[string]interface{}, error) {
	gen := NewSentinelGenerator(deviceID)
	pToken := gen.GenerateRequirementsToken()

	reqBody, _ := json.Marshal(map[string]string{
		"p":  pToken,
		"id": deviceID,
		"flow": flow,
	})

	headers := map[string]string{
		"Content-Type":       "text/plain;charset=UTF-8",
		"Referer":            "https://sentinel.openai.com/backend-api/sentinel/frame.html",
		"User-Agent":         sentinelUserAgent,
		"Origin":             "https://sentinel.openai.com",
		"sec-ch-ua":          `"Not:A-Brand";v="99", "Google Chrome";v="145", "Chromium";v="145"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
	}

	resp, err := sidecar.Post(ctx, sentinelReqURL, headers, reqBody, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("sentinel challenge request: %w", err)
	}
	body, _ := ReadBody(resp)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("sentinel challenge HTTP %d: %s", resp.StatusCode, string(body[:min(300, len(body))]))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse sentinel challenge: %w", err)
	}
	return result, nil
}

// BuildSentinelToken builds the complete openai-sentinel-token header value.
func BuildSentinelToken(ctx context.Context, sidecar *SidecarHTTPClient, deviceID, flow, proxyURL string) (string, error) {
	challenge, err := FetchSentinelChallenge(ctx, sidecar, deviceID, flow, proxyURL)
	if err != nil {
		return "", err
	}

	cValue, _ := challenge["token"].(string)
	powData, _ := challenge["proofofwork"].(map[string]interface{})

	gen := NewSentinelGenerator(deviceID)
	var pValue string
	if powData != nil {
		required, _ := powData["required"].(bool)
		seed, _ := powData["seed"].(string)
		if required && seed != "" {
			difficulty, _ := powData["difficulty"].(string)
			if difficulty == "" {
				difficulty = "0"
			}
			pValue = gen.GenerateToken(seed, difficulty)
		} else {
			pValue = gen.GenerateRequirementsToken()
		}
	} else {
		pValue = gen.GenerateRequirementsToken()
	}

	token, _ := json.Marshal(map[string]string{
		"p":    pValue,
		"t":    "",
		"c":    cValue,
		"id":   deviceID,
		"flow": flow,
	})
	return string(token), nil
}

// FNV1a-32 hash returning hex string matching Python implementation.
func fnv1a32Hex(text string) string {
	h := fnv.New32a()
	h.Write([]byte(text))
	sum := h.Sum32()
	// Python-specific post-processing
	sum ^= sum >> 16
	sum = (sum * 2246822507) & 0xFFFFFFFF
	sum ^= sum >> 13
	sum = (sum * 3266489909) & 0xFFFFFFFF
	sum ^= sum >> 16
	return fmt.Sprintf("%08x", sum)
}

func randomUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomFloat64() float64 {
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<53))
	return float64(n.Int64()) / float64(1<<53)
}

func randomChoice(options []string) string {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(options))))
	return options[n.Int64()]
}

func randomInt(min, max int) int {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(max-min+1)))
	return int(n.Int64()) + min
}
