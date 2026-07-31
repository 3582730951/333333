package mailbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const mailboxHTTPResponseBodyLimit = 256 * 1024

// GenericHTTP is a data-driven mailbox provider where ALL behavior (auth, create,
// list, detail) is described via JSON config. Zero-code onboarding for new HTTP mailbox
// APIs (outlookEmail, DuckMail, TempMail.lol, etc.).
//
// Config pipeline (all steps optional except list_emails):
//
//	auth_steps[]  →  create_email  →  list_emails  →  get_detail
//
// Variables pool: user config (api_url, auth_token, ...) + step-extracted values
// (email, account_id, message_id) are available as {var} placeholders in all
// subsequent steps.
type GenericHTTP struct {
	config   map[string]interface{} // merged pipeline + flat settings
	vars     map[string]string      // runtime variables pool
	session  *http.Client
	authDone bool
}

// NewGenericHTTP builds a data-driven mailbox from pipeline config + flat settings.
// pipelineConfig comes from provider_definitions.metadata_json; settings from
// provider_settings.config_json. When pipelineConfig is empty, auto-builds a pipeline
// from flat settings (list_path, create_method, etc.).
func NewGenericHTTP(pipelineConfig, settings map[string]interface{}, proxyURL string) (*GenericHTTP, error) {
	g := &GenericHTTP{
		config:  buildPipeline(pipelineConfig, settings),
		vars:    flattenToStrings(settings),
		session: &http.Client{Timeout: 30 * time.Second},
	}
	if proxyURL != "" {
		if purl, err := url.Parse(proxyURL); err == nil {
			g.session.Transport = &http.Transport{Proxy: http.ProxyURL(purl)}
		}
	}
	g.session = newGuardedMailboxHTTPClient(g.session, getStr(settings, "api_url", ""))
	return g, nil
}

func (g *GenericHTTP) GetEmail(ctx context.Context) (string, string, error) {
	if err := g.runAuth(ctx); err != nil {
		return "", "", err
	}
	mode := getStr(g.config, "email_mode", "fixed")
	switch mode {
	case "fixed":
		email := g.vars["email"]
		if email == "" {
			return "", "", fmt.Errorf("email_mode=fixed but no email configured")
		}
		return email, g.vars["account_id"], nil
	case "generate":
		createStep := g.config["create_email"]
		if createStep == nil {
			return "", "", fmt.Errorf("email_mode=generate but no create_email step")
		}
		// Multi-step create (list of steps)
		steps := []map[string]interface{}{}
		if stepSlice, ok := createStep.([]interface{}); ok {
			for _, s := range stepSlice {
				if sm, ok := s.(map[string]interface{}); ok {
					steps = append(steps, sm)
				}
			}
		} else if stepMap, ok := createStep.(map[string]interface{}); ok {
			steps = append(steps, stepMap)
		}
		for _, step := range steps {
			if _, err := g.executeStep(ctx, step); err != nil {
				return "", "", err
			}
		}
		email := g.vars["email"]
		if email == "" {
			return "", "", fmt.Errorf("create_email completed but no email extracted")
		}
		return email, g.vars["account_id"], nil
	default:
		return "", "", fmt.Errorf("unsupported email_mode: %s", mode)
	}
}

func (g *GenericHTTP) GetCurrentIDs(ctx context.Context, email, accountID string) ([]string, error) {
	if err := g.runAuth(ctx); err != nil {
		return nil, err
	}
	g.vars["email"] = email
	if accountID != "" {
		g.vars["account_id"] = accountID
	}
	listStep := getMap(g.config, "list_emails")
	if listStep == nil {
		return []string{}, nil
	}
	resp, err := g.executeStep(ctx, listStep)
	if err != nil {
		return nil, err
	}
	listPath := getStr(g.config, "response_list_path", "")
	idField := getStr(g.config, "response_id_field", "id")
	items := deepGet(resp, listPath)
	itemSlice, ok := items.([]interface{})
	if !ok {
		return []string{}, nil
	}
	ids := []string{}
	for _, item := range itemSlice {
		if m, ok := item.(map[string]interface{}); ok {
			if id := getStr(m, idField, ""); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

func (g *GenericHTTP) WaitForCode(ctx context.Context, email, accountID string, beforeIDs []string, timeout int) (string, error) {
	if err := g.runAuth(ctx); err != nil {
		return "", err
	}
	g.vars["email"] = email
	if accountID != "" {
		g.vars["account_id"] = accountID
	}
	listStep := getMap(g.config, "list_emails")
	if listStep == nil {
		return "", fmt.Errorf("no list_emails step configured")
	}
	listPath := getStr(g.config, "response_list_path", "")
	idField := getStr(g.config, "response_id_field", "id")
	bodyFieldsStr := getStr(g.config, "response_body_fields", "subject,content,html,text,body,preview")
	bodyFields := strings.Split(bodyFieldsStr, ",")
	for i := range bodyFields {
		bodyFields[i] = strings.TrimSpace(bodyFields[i])
	}
	detailStep := getMap(g.config, "get_detail")
	seen := make(map[string]bool)
	for _, id := range beforeIDs {
		seen[id] = true
	}
	start := time.Now()
	codeRe := regexp.MustCompile(`(?:\D|^)(\d{6})(?:\D|$)`)
	for time.Since(start) < time.Duration(timeout)*time.Second {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("mailbox wait cancelled: %w", ctx.Err())
		default:
		}
		resp, err := g.executeStep(ctx, listStep)
		if err != nil {
			if !waitMailboxPoll(ctx, 3*time.Second) {
				return "", fmt.Errorf("mailbox wait cancelled: %w", ctx.Err())
			}
			continue
		}
		items := deepGet(resp, listPath)
		itemSlice, _ := items.([]interface{})
		for _, item := range itemSlice {
			m, _ := item.(map[string]interface{})
			if m == nil {
				continue
			}
			mid := getStr(m, idField, "")
			if mid == "" || seen[mid] {
				continue
			}
			seen[mid] = true
			// Get detail if configured
			detailData := m
			if detailStep != nil {
				g.vars["message_id"] = mid
				detailResp, err := g.executeStep(ctx, detailStep)
				if err == nil {
					if dm, ok := detailResp.(map[string]interface{}); ok {
						detailData = dm
					}
				}
			}
			// Check special field
			if code := getStr(detailData, "verification_code", ""); code != "" && code != "None" {
				return code, nil
			}
			// Concat body fields
			var parts []string
			for _, f := range bodyFields {
				if v := getStr(detailData, f, ""); v != "" {
					parts = append(parts, v)
				}
			}
			combined := strings.Join(parts, " ")
			if matches := codeRe.FindStringSubmatch(combined); len(matches) > 1 {
				return matches[1], nil
			}
		}
		if !waitMailboxPoll(ctx, 3*time.Second) {
			return "", fmt.Errorf("mailbox wait cancelled: %w", ctx.Err())
		}
	}
	return "", fmt.Errorf("timeout waiting for code (%ds)", timeout)
}

func (g *GenericHTTP) WaitForLink(ctx context.Context, email, accountID string, beforeIDs []string, timeout int) (string, error) {
	if err := g.runAuth(ctx); err != nil {
		return "", err
	}
	g.vars["email"] = email
	if accountID != "" {
		g.vars["account_id"] = accountID
	}
	listStep := getMap(g.config, "list_emails")
	if listStep == nil {
		return "", fmt.Errorf("no list_emails step configured")
	}
	listPath := getStr(g.config, "response_list_path", "")
	idField := getStr(g.config, "response_id_field", "id")
	bodyFieldsStr := getStr(g.config, "response_body_fields", "subject,content,html,text,body,preview")
	bodyFields := strings.Split(bodyFieldsStr, ",")
	for i := range bodyFields {
		bodyFields[i] = strings.TrimSpace(bodyFields[i])
	}
	detailStep := getMap(g.config, "get_detail")
	seen := make(map[string]bool)
	for _, id := range beforeIDs {
		seen[id] = true
	}
	start := time.Now()
	linkRe := regexp.MustCompile(`https?://[^\s<>"]+`)
	for time.Since(start) < time.Duration(timeout)*time.Second {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("mailbox wait cancelled: %w", ctx.Err())
		default:
		}
		resp, err := g.executeStep(ctx, listStep)
		if err != nil {
			if !waitMailboxPoll(ctx, 3*time.Second) {
				return "", fmt.Errorf("mailbox wait cancelled: %w", ctx.Err())
			}
			continue
		}
		items := deepGet(resp, listPath)
		itemSlice, _ := items.([]interface{})
		for _, item := range itemSlice {
			m, _ := item.(map[string]interface{})
			if m == nil {
				continue
			}
			mid := getStr(m, idField, "")
			if mid == "" || seen[mid] {
				continue
			}
			seen[mid] = true
			detailData := m
			if detailStep != nil {
				g.vars["message_id"] = mid
				detailResp, err := g.executeStep(ctx, detailStep)
				if err == nil {
					if dm, ok := detailResp.(map[string]interface{}); ok {
						detailData = dm
					}
				}
			}
			var parts []string
			for _, f := range bodyFields {
				if v := getStr(detailData, f, ""); v != "" {
					parts = append(parts, v)
				}
			}
			combined := strings.Join(parts, " ")
			if matches := linkRe.FindAllString(combined, -1); len(matches) > 0 {
				for _, link := range matches {
					if strings.Contains(link, "verify") || strings.Contains(link, "confirm") || strings.Contains(link, "activate") {
						return link, nil
					}
				}
			}
		}
		if !waitMailboxPoll(ctx, 3*time.Second) {
			return "", fmt.Errorf("mailbox wait cancelled: %w", ctx.Err())
		}
	}
	return "", fmt.Errorf("timeout waiting for link (%ds)", timeout)
}

func waitMailboxPoll(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// ── step execution engine ──

func (g *GenericHTTP) runAuth(ctx context.Context) error {
	if g.authDone {
		return nil
	}
	authSteps := getSlice(g.config, "auth_steps")
	for _, step := range authSteps {
		if stepMap, ok := step.(map[string]interface{}); ok {
			if _, err := g.executeStep(ctx, stepMap); err != nil {
				return err
			}
		}
	}
	g.authDone = true
	return nil
}

func (g *GenericHTTP) executeStep(ctx context.Context, step map[string]interface{}) (interface{}, error) {
	apiURL := strings.TrimRight(g.vars["api_url"], "/")
	method := render(getStr(step, "method", "GET"), g.vars)
	path := render(getStr(step, "path", ""), g.vars)
	fullURL := apiURL + path
	if strings.HasPrefix(path, "http") {
		fullURL = path
	}
	// headers
	headers := renderMap(getMap(step, "headers"), g.vars)
	// params
	params := renderMap(getMap(step, "params"), g.vars)
	// api_key as query param
	if g.vars["auth_type"] == "api_key_param" && g.vars["auth_token"] != "" {
		keyName := g.vars["auth_header_name"]
		if keyName == "" {
			keyName = "apikey"
		}
		params[keyName] = g.vars["auth_token"]
	}
	// body
	bodyTemplate := getMap(step, "body")
	body := renderMap(bodyTemplate, g.vars)
	// build request
	var reqBody io.Reader
	if len(body) > 0 && (method == "POST" || method == "PUT" || method == "PATCH") {
		if getStr(step, "content_type", "json") == "form" {
			form := url.Values{}
			for k, v := range body {
				form.Set(k, fmt.Sprint(v))
			}
			reqBody = strings.NewReader(form.Encode())
			headers["content-type"] = "application/x-www-form-urlencoded"
		} else {
			b, _ := json.Marshal(body)
			reqBody = bytes.NewReader(b)
			headers["content-type"] = "application/json"
		}
	}
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			q.Set(k, fmt.Sprint(v))
		}
		fullURL += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, fmt.Sprint(v))
	}
	// auth header
	if g.vars["auth_type"] == "bearer" && g.vars["auth_token"] != "" {
		req.Header.Set("authorization", "Bearer "+g.vars["auth_token"])
	} else if g.vars["auth_type"] == "header" && g.vars["auth_token"] != "" {
		headerName := g.vars["auth_header_name"]
		if headerName == "" {
			headerName = "authorization"
		}
		req.Header.Set(headerName, g.vars["auth_token"])
	}
	resp, err := g.session.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := readMailboxHTTPBody(resp.Body)
	var respData interface{}
	if err := json.Unmarshal(raw, &respData); err != nil {
		// fallback to text
		respData = map[string]interface{}{"_text": string(raw)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mailbox HTTP step returned status %d", resp.StatusCode)
	}
	// extract variables
	extractMap := getMap(step, "extract")
	for varName, jsonPath := range extractMap {
		extracted := deepGet(respData, fmt.Sprint(jsonPath))
		if extracted != nil {
			g.vars[varName] = fmt.Sprint(extracted)
		}
	}
	return respData, nil
}

func readMailboxHTTPBody(body io.Reader) []byte {
	raw, _ := io.ReadAll(io.LimitReader(body, mailboxHTTPResponseBodyLimit))
	return raw
}

// ── pipeline builder ──

func buildPipeline(raw, flat map[string]interface{}) map[string]interface{} {
	p := copyMap(raw)
	if p == nil {
		p = make(map[string]interface{})
	}
	// defaults from flat
	if _, ok := p["email_mode"]; !ok {
		p["email_mode"] = getStr(flat, "email_mode", "fixed")
	}
	if _, ok := p["response_list_path"]; !ok {
		p["response_list_path"] = getStr(flat, "response_list_path", "")
	}
	if _, ok := p["response_id_field"]; !ok {
		p["response_id_field"] = getStr(flat, "response_id_field", "id")
	}
	if _, ok := p["response_body_fields"]; !ok {
		p["response_body_fields"] = getStr(flat, "response_body_fields", "subject,content,html,text,body,preview")
	}
	// build list_emails from flat if missing
	if _, ok := p["list_emails"]; !ok {
		if listPath := getStr(flat, "list_path", ""); listPath != "" {
			step := map[string]interface{}{
				"method": getStr(flat, "list_method", "GET"),
				"path":   listPath,
			}
			if paramsJSON := getStr(flat, "list_params", ""); paramsJSON != "" {
				var params map[string]interface{}
				if json.Unmarshal([]byte(paramsJSON), &params) == nil {
					step["params"] = params
				}
			}
			p["list_emails"] = step
		}
	}
	// build create_email from flat
	if _, ok := p["create_email"]; !ok {
		if createMethod := getStr(flat, "create_method", ""); createMethod != "" {
			if createPath := getStr(flat, "create_path", ""); createPath != "" {
				step := map[string]interface{}{"method": createMethod, "path": createPath}
				if bodyJSON := getStr(flat, "create_body", ""); bodyJSON != "" {
					var body map[string]interface{}
					if json.Unmarshal([]byte(bodyJSON), &body) == nil {
						step["body"] = body
					}
				}
				if emailField := getStr(flat, "create_email_field", ""); emailField != "" {
					step["extract"] = map[string]interface{}{"email": emailField}
				}
				p["create_email"] = step
			}
		}
	}
	// build get_detail from flat
	if _, ok := p["get_detail"]; !ok {
		if detailPath := getStr(flat, "detail_path", ""); detailPath != "" {
			p["get_detail"] = map[string]interface{}{"method": "GET", "path": detailPath}
		}
	}
	return p
}

// ── helpers ──

func render(template string, vars map[string]string) string {
	for k, v := range vars {
		template = strings.ReplaceAll(template, "{"+k+"}", v)
	}
	return template
}

func renderMap(template map[string]interface{}, vars map[string]string) map[string]interface{} {
	if template == nil {
		return nil
	}
	result := make(map[string]interface{})
	for k, v := range template {
		if s, ok := v.(string); ok {
			result[k] = render(s, vars)
		} else if m, ok := v.(map[string]interface{}); ok {
			result[k] = renderMap(m, vars)
		} else {
			result[k] = v
		}
	}
	return result
}

func deepGet(data interface{}, path string) interface{} {
	if path == "" {
		return data
	}
	keys := strings.Split(path, ".")
	current := data
	for _, key := range keys {
		if m, ok := current.(map[string]interface{}); ok {
			current = m[key]
		} else {
			return nil
		}
		if current == nil {
			return nil
		}
	}
	return current
}

func getStr(m map[string]interface{}, key, def string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func getMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key]; ok {
		if m, ok := v.(map[string]interface{}); ok {
			return m
		}
	}
	return nil
}

func getSlice(m map[string]interface{}, key string) []interface{} {
	if v, ok := m[key]; ok {
		if s, ok := v.([]interface{}); ok {
			return s
		}
	}
	return nil
}

func copyMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	result := make(map[string]interface{})
	for k, v := range m {
		result[k] = v
	}
	return result
}

func flattenToStrings(m map[string]interface{}) map[string]string {
	result := make(map[string]string)
	for k, v := range m {
		result[k] = fmt.Sprint(v)
	}
	return result
}
