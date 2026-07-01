package sms

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SMSPoolProvider implements the SMSPool (smspool.net) form API.
//
// SMSPool addresses a service by numeric ID (not name), so the operator must supply a
// `service` id in the provider config; `country` is the SMSPool country id (passed
// through from the task, default handled by the pipeline). Endpoints (form-encoded):
//
//	POST /purchase/sms  {key,country,service[,max_price]} -> {success,number,order_id}
//	POST /sms/check     {key,orderid}                      -> {status,sms}
//	POST /sms/cancel    {key,orderid}
type SMSPoolProvider struct {
	apiKey     string
	service    string
	maxPrice   string
	httpClient *http.Client
}

// NewSMSPoolProvider creates an SMSPool provider. service is the SMSPool service id
// (required by SMSPool); maxPrice is optional ("" = no cap).
func NewSMSPoolProvider(apiKey, service, maxPrice string, httpClient *http.Client) *SMSPoolProvider {
	return &SMSPoolProvider{apiKey: apiKey, service: service, maxPrice: maxPrice, httpClient: httpClient}
}

func (p *SMSPoolProvider) Name() string { return "smspool" }
func (p *SMSPoolProvider) Type() string { return "sms" }

func (p *SMSPoolProvider) post(ctx context.Context, path string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.smspool.net"+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 1024)
	for {
		n, rerr := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil || len(buf) > 1<<16 {
			break
		}
	}
	return buf, nil
}

// GetNumber purchases a phone number for the configured service.
func (p *SMSPoolProvider) GetNumber(ctx context.Context, country string) (string, string, error) {
	if p.service == "" {
		return "", "", fmt.Errorf("smspool: a service id is required in the provider config")
	}
	form := url.Values{}
	form.Set("key", p.apiKey)
	form.Set("country", country)
	form.Set("service", p.service)
	if p.maxPrice != "" {
		form.Set("max_price", p.maxPrice)
	}
	body, err := p.post(ctx, "/purchase/sms", form)
	if err != nil {
		return "", "", err
	}
	var data struct {
		Success int         `json:"success"`
		Number  string      `json:"number"`
		OrderID interface{} `json:"order_id"`
		Message string      `json:"message"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", "", fmt.Errorf("smspool: %s", strings.TrimSpace(string(body)))
	}
	if data.Success != 1 || data.Number == "" {
		return "", "", fmt.Errorf("smspool: %s", firstNonEmptyStr(data.Message, strings.TrimSpace(string(body))))
	}
	return data.Number, fmt.Sprintf("%v", data.OrderID), nil
}

// WaitCode polls SMSPool for the received code.
func (p *SMSPoolProvider) WaitCode(ctx context.Context, orderID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timeout waiting for SMS")
			}
			form := url.Values{}
			form.Set("key", p.apiKey)
			form.Set("orderid", orderID)
			body, err := p.post(ctx, "/sms/check", form)
			if err != nil {
				continue
			}
			var data struct {
				Status int    `json:"status"`
				SMS    string `json:"sms"`
			}
			json.Unmarshal(body, &data)
			if strings.TrimSpace(data.SMS) != "" {
				return data.SMS, nil
			}
		}
	}
}

// CancelNumber cancels/refunds the order.
func (p *SMSPoolProvider) CancelNumber(ctx context.Context, orderID string) error {
	form := url.Values{}
	form.Set("key", p.apiKey)
	form.Set("orderid", orderID)
	_, err := p.post(ctx, "/sms/cancel", form)
	return err
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
