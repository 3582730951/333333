package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"codex-account-pool/internal/registration/pipeline"
)

// decodeRegistrationRequest accepts the current snake_case contract plus the
// field spellings emitted by the legacy email/turbo pages and early SPA builds.
// Canonical fields always win. The returned value is still normalized and
// validated by Handler.normalizeRegisterRequest before any work is queued.
func decodeRegistrationRequest(body io.Reader) (pipeline.RegisterRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(body, adminJSONBodyLimit+1))
	if err != nil {
		return pipeline.RegisterRequest{}, err
	}
	if int64(len(raw)) > adminJSONBodyLimit {
		return pipeline.RegisterRequest{}, fmt.Errorf("registration request exceeds request limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root map[string]interface{}
	if err := decoder.Decode(&root); err != nil {
		return pipeline.RegisterRequest{}, err
	}
	if root == nil {
		return pipeline.RegisterRequest{}, fmt.Errorf("registration request must be a JSON object")
	}
	// Old turbo clients placed routing/provider values inside config. Use those
	// only as a fallback behind top-level canonical and alias fields.
	nested, _ := root["config"].(map[string]interface{})
	value := func(keys ...string) interface{} {
		if found, ok := firstRegistrationValue(root, keys...); ok {
			return found
		}
		found, _ := firstRegistrationValue(nested, keys...)
		return found
	}

	var req pipeline.RegisterRequest
	if req.Platform, err = registrationString(value("platform", "service")); err != nil {
		return req, registrationFieldError("platform", err)
	}
	if req.Method, err = registrationString(value("method", "engine", "register_method", "registration_method", "registerMethod")); err != nil {
		return req, registrationFieldError("method", err)
	}
	if req.Count, err = registrationInt(value("count", "total", "amount")); err != nil {
		return req, registrationFieldError("count", err)
	}
	if req.GroupName, err = registrationString(value("group_name", "groupName", "group")); err != nil {
		return req, registrationFieldError("group_name", err)
	}
	if req.EgressID, err = registrationString(value("egress_id", "egressId", "egress")); err != nil {
		return req, registrationFieldError("egress_id", err)
	}
	if req.RegistrationEgressPoolID, err = registrationString(value(
		"registration_egress_pool_id", "registrationEgressPoolId", "registration_pool_id",
		"registrationPoolId", "egress_pool_id", "egressPoolId",
	)); err != nil {
		return req, registrationFieldError("registration_egress_pool_id", err)
	}
	if req.RuntimeEgressPoolID, err = registrationString(value("runtime_egress_pool_id", "runtimeEgressPoolId", "runtime_pool_id")); err != nil {
		return req, registrationFieldError("runtime_egress_pool_id", err)
	}
	if req.UpgradeToPlus, err = registrationBool(value("upgrade_to_plus", "upgradeToPlus")); err != nil {
		return req, registrationFieldError("upgrade_to_plus", err)
	}
	if req.IdentityMode, err = registrationString(value("identity_mode", "identityMode", "identity")); err != nil {
		return req, registrationFieldError("identity_mode", err)
	}
	if req.Country, err = registrationString(value("country", "phone_country_code", "countryCode")); err != nil {
		return req, registrationFieldError("country", err)
	}
	if req.SMSProvider, err = registrationString(value("sms_provider", "smsProvider", "sms_platform")); err != nil {
		return req, registrationFieldError("sms_provider", err)
	}
	if req.MailboxProvider, err = registrationString(value(
		"mailbox_provider", "mailboxProvider", "mail_provider", "mailProvider", "email_provider", "emailProvider",
	)); err != nil {
		return req, registrationFieldError("mailbox_provider", err)
	}
	if req.MailboxDomain, err = registrationString(value("mailbox_domain", "mailboxDomain", "mail_domain", "mailDomain")); err != nil {
		return req, registrationFieldError("mailbox_domain", err)
	}
	if req.CaptchaSolver, err = registrationString(value("captcha_solver", "captchaSolver", "captcha_provider", "captchaProvider")); err != nil {
		return req, registrationFieldError("captcha_solver", err)
	}
	if req.Canary, err = registrationBool(value("canary", "is_canary", "isCanary")); err != nil {
		return req, registrationFieldError("canary", err)
	}
	return req, nil
}

func firstRegistrationValue(values map[string]interface{}, keys ...string) (interface{}, bool) {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value, true
		}
	}
	return nil, false
}

func registrationString(value interface{}) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "", nil
	case string:
		return typed, nil
	case json.Number:
		return typed.String(), nil
	default:
		return "", fmt.Errorf("must be a string")
	}
}

func registrationInt(value interface{}) (int, error) {
	switch typed := value.(type) {
	case nil:
		return 0, nil
	case json.Number:
		n, err := strconv.ParseInt(typed.String(), 10, 32)
		return int(n), err
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(typed))
		return n, err
	case float64:
		if math.Trunc(typed) != typed {
			return 0, fmt.Errorf("must be an integer")
		}
		return int(typed), nil
	default:
		return 0, fmt.Errorf("must be an integer")
	}
}

func registrationBool(value interface{}) (bool, error) {
	switch typed := value.(type) {
	case nil:
		return false, nil
	case bool:
		return typed, nil
	case json.Number:
		return typed.String() == "1", nil
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "on", "yes":
			return true, nil
		case "", "false", "0", "off", "no":
			return false, nil
		}
	}
	return false, fmt.Errorf("must be a boolean")
}

func registrationFieldError(field string, err error) error {
	return fmt.Errorf("registration field %s: %w", field, err)
}

func normalizeRegistrationMethodAlias(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "email", "email_otp", "email_register", "protocol2", "protocol_v_2":
		return "protocol_v2"
	case "turbo", "turbo_gpt", "turbo_gpt_register", "playwright", "browser3", "browser_v_3":
		return "browser_v3"
	default:
		return normalized
	}
}

func normalizeMailboxProviderAlias(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "emailpool", "outlook_pool", "hotmail_pool", "legacy_email_pool":
		return "email_pool"
	default:
		return normalized
	}
}
