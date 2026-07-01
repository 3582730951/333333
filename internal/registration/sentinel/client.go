package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	sentinelHTTPTimeout       = 30 * time.Second
	sentinelResponseBodyLimit = 256 * 1024
	sentinelLogSnippetLimit   = 200
)

// Client interacts with Sentinel API
type Client struct {
	httpClient *http.Client
	userAgent  string
	deviceID   string
	sessionID  string
}

// NewClient creates a Sentinel client
func NewClient(httpClient *http.Client, userAgent, deviceID, sessionID string) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: sentinelHTTPTimeout}
	}
	return &Client{
		httpClient: httpClient,
		userAgent:  userAgent,
		deviceID:   deviceID,
		sessionID:  sessionID,
	}
}

// Token holds main and SO tokens
type Token struct {
	MainToken string
	SOToken   string
}

// Get fetches a Sentinel token for the given flow
func (c *Client) Get(ctx context.Context, flow string) (*Token, error) {
	reqToken := GenerateRequirementsToken(c.userAgent, c.sessionID)
	reqBody := map[string]interface{}{
		"p":    reqToken,
		"id":   c.deviceID,
		"flow": flow,
	}
	bodyBytes, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://sentinel.openai.com/backend-api/sentinel/req",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Origin", "https://sentinel.openai.com")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, sentinelResponseBodyLimit))
	if err != nil {
		return nil, err
	}

	snip := sentinelSnippet(respBytes)
	if os.Getenv("REG_DEBUG") != "" {
		log.Printf("[REG-DEBUG] sentinel(%s) status=%d body=%s", flow, resp.StatusCode, strings.ReplaceAll(snip, "\n", " "))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("sentinel status %d: %s", resp.StatusCode, snip)
	}

	var data struct {
		Token       string `json:"token"`
		ProofOfWork struct {
			Required   bool   `json:"required"`
			Seed       string `json:"seed"`
			Difficulty string `json:"difficulty"`
		} `json:"proofofwork"`
		SO string `json:"so"`
		T  string `json:"t"`
	}
	if err := json.Unmarshal(respBytes, &data); err != nil {
		return nil, err
	}

	p := reqToken
	if data.ProofOfWork.Required && data.ProofOfWork.Seed != "" {
		p, err = SolvePoW(data.ProofOfWork.Seed, data.ProofOfWork.Difficulty, c.userAgent, c.sessionID)
		if err != nil {
			return nil, err
		}
	}

	soRaw := data.SO
	if soRaw == "" {
		soRaw = data.T
	}
	mainTokenObj := map[string]interface{}{
		"p":    p,
		"c":    data.Token,
		"id":   c.deviceID,
		"flow": flow,
		"t":    soRaw,
	}
	mainTokenBytes, _ := json.Marshal(mainTokenObj)

	soTokenObj := map[string]interface{}{
		"so":   soRaw,
		"c":    data.Token,
		"id":   c.deviceID,
		"flow": flow,
	}
	soTokenBytes, _ := json.Marshal(soTokenObj)

	return &Token{
		MainToken: string(mainTokenBytes),
		SOToken:   string(soTokenBytes),
	}, nil
}

func sentinelSnippet(body []byte) string {
	snip := string(body)
	if len(snip) > sentinelLogSnippetLimit {
		return snip[:sentinelLogSnippetLimit]
	}
	return snip
}

// Flows
const (
	FlowUsernamePasswordCreate = "username_password_create"
	FlowAuthorizeContinue      = "authorize_continue"
	FlowPasswordVerify         = "password_verify"
	FlowOAuthCreateAccount     = "oauth_create_account"
)
