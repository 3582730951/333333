package captcha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const captchaProviderResponseBodyLimit = 256 * 1024

// YesCaptchaProvider implements YesCaptcha API
type YesCaptchaProvider struct {
	apiKey     string
	httpClient *http.Client
}

// NewYesCaptchaProvider creates a YesCaptcha solver
func NewYesCaptchaProvider(apiKey string, httpClient *http.Client) *YesCaptchaProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &YesCaptchaProvider{
		apiKey:     apiKey,
		httpClient: httpClient,
	}
}

func (p *YesCaptchaProvider) Name() string { return "yescaptcha" }
func (p *YesCaptchaProvider) Type() string { return "captcha" }

// CaptchaRequest defines a captcha challenge
type CaptchaRequest struct {
	Type     string // "recaptcha_v2" | "hcaptcha" | "funcaptcha" | "turnstile"
	SiteKey  string
	PageURL  string
	ProxyURL string
	// Turnstile-only: the cfturnstile metadata.action (optional, page-defined) and
	// metadata.type ("turnstile" default). Some Cloudflare-Managed Turnstile pages set
	// a custom action the solver must replay.
	TurnstileAction string
}

// Solve submits and waits for captcha solution
func (p *YesCaptchaProvider) Solve(ctx context.Context, req CaptchaRequest) (string, error) {
	// Submit task
	taskID, err := p.submitTask(ctx, req)
	if err != nil {
		return "", err
	}

	// Poll for result
	return p.waitResult(ctx, taskID, 120*time.Second)
}

func (p *YesCaptchaProvider) submitTask(ctx context.Context, req CaptchaRequest) (string, error) {
	taskData := map[string]interface{}{
		"clientKey": p.apiKey,
		"task": map[string]interface{}{
			"websiteURL": req.PageURL,
			"websiteKey": req.SiteKey,
		},
	}

	// Set task type based on captcha type
	switch req.Type {
	case "recaptcha_v2":
		taskData["task"].(map[string]interface{})["type"] = "NoCaptchaTaskProxyless"
	case "hcaptcha":
		taskData["task"].(map[string]interface{})["type"] = "HCaptchaTaskProxyless"
	case "funcaptcha":
		taskData["task"].(map[string]interface{})["type"] = "FunCaptchaTaskProxyless"
	case "turnstile":
		// Cloudflare Turnstile: AntiTurnstileTaskProxyLess (YesCaptcha docs).
		// Requires websiteURL, websiteKey, and optionally metadata.action.
		taskData["task"].(map[string]interface{})["type"] = "AntiTurnstileTaskProxyLess"
		meta := map[string]interface{}{"type": "Turnstile"}
		if req.TurnstileAction != "" {
			meta["action"] = req.TurnstileAction
		}
		taskData["task"].(map[string]interface{})["metadata"] = meta
	default:
		return "", fmt.Errorf("unsupported captcha type: %s", req.Type)
	}

	bodyBytes, _ := json.Marshal(taskData)
	httpReq, _ := http.NewRequestWithContext(ctx, "POST",
		"https://api.yescaptcha.com/createTask",
		bytes.NewReader(bodyBytes))
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		ErrorID          int    `json:"errorId"`
		ErrorDescription string `json:"errorDescription"`
		TaskID           string `json:"taskId"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if result.ErrorID != 0 {
		return "", fmt.Errorf("yescaptcha: %s", result.ErrorDescription)
	}

	return result.TaskID, nil
}

func (p *YesCaptchaProvider) waitResult(ctx context.Context, taskID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timeout waiting for captcha solution")
			}

			reqData := map[string]string{
				"clientKey": p.apiKey,
				"taskId":    taskID,
			}
			bodyBytes, _ := json.Marshal(reqData)

			httpReq, _ := http.NewRequestWithContext(ctx, "POST",
				"https://api.yescaptcha.com/getTaskResult",
				bytes.NewReader(bodyBytes))
			httpReq.Header.Set("Content-Type", "application/json")

			resp, err := p.httpClient.Do(httpReq)
			if err != nil {
				continue
			}

			var result struct {
				ErrorID          int    `json:"errorId"`
				ErrorDescription string `json:"errorDescription"`
				Status           string `json:"status"`
				Solution         struct {
					GRecaptchaResponse string `json:"gRecaptchaResponse"`
					Token              string `json:"token"`
				} `json:"solution"`
			}

			json.NewDecoder(resp.Body).Decode(&result)
			resp.Body.Close()

			if result.Status == "ready" {
				if result.Solution.GRecaptchaResponse != "" {
					return result.Solution.GRecaptchaResponse, nil
				}
				if result.Solution.Token != "" {
					return result.Solution.Token, nil
				}
			}
		}
	}
}

// TwoCaptchaProvider implements 2Captcha API
type TwoCaptchaProvider struct {
	apiKey     string
	httpClient *http.Client
}

// NewTwoCaptchaProvider creates a 2Captcha solver
func NewTwoCaptchaProvider(apiKey string, httpClient *http.Client) *TwoCaptchaProvider {
	return &TwoCaptchaProvider{
		apiKey:     apiKey,
		httpClient: httpClient,
	}
}

func (p *TwoCaptchaProvider) Name() string { return "2captcha" }
func (p *TwoCaptchaProvider) Type() string { return "captcha" }

// Solve submits and waits for captcha solution
func (p *TwoCaptchaProvider) Solve(ctx context.Context, req CaptchaRequest) (string, error) {
	taskID, err := p.submitTask(ctx, req)
	if err != nil {
		return "", err
	}
	return p.waitResult(ctx, taskID, 120*time.Second)
}

func (p *TwoCaptchaProvider) submitTask(ctx context.Context, req CaptchaRequest) (string, error) {
	params := url.Values{}
	params.Set("key", p.apiKey)
	params.Set("method", "userrecaptcha")
	params.Set("googlekey", req.SiteKey)
	params.Set("pageurl", req.PageURL)
	params.Set("json", "1")

	switch req.Type {
	case "hcaptcha":
		params.Set("method", "hcaptcha")
	case "funcaptcha":
		params.Set("method", "funcaptcha")
		params.Set("publickey", req.SiteKey)
		params.Del("googlekey")
	}

	httpReq, _ := http.NewRequestWithContext(ctx, "POST",
		"https://2captcha.com/in.php",
		bytes.NewBufferString(params.Encode()))
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Status  int    `json:"status"`
		Request string `json:"request"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if result.Status != 1 {
		return "", fmt.Errorf("2captcha: submit failed")
	}

	return result.Request, nil
}

func (p *TwoCaptchaProvider) waitResult(ctx context.Context, taskID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Wait 5 seconds before first check (2captcha recommendation)
	time.Sleep(5 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timeout waiting for captcha solution")
			}

			httpReq, _ := http.NewRequestWithContext(ctx, "GET",
				fmt.Sprintf("https://2captcha.com/res.php?key=%s&action=get&id=%s&json=1",
					p.apiKey, taskID), nil)

			resp, err := p.httpClient.Do(httpReq)
			if err != nil {
				continue
			}

			body := readCaptchaProviderBody(resp.Body)
			resp.Body.Close()

			var result struct {
				Status  int    `json:"status"`
				Request string `json:"request"`
			}

			json.Unmarshal(body, &result)

			if result.Status == 1 {
				return result.Request, nil
			}
		}
	}
}

func readCaptchaProviderBody(body io.Reader) []byte {
	raw, _ := io.ReadAll(io.LimitReader(body, captchaProviderResponseBodyLimit))
	return raw
}
