package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func normalizeAutomationPolicyType(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "schedule", "scheduled_registration", "auto_register", "automatic_registration":
		return PolicyTypeScheduled
	case "auto_refill", "pool_refill", "autorefill", "refill_pool":
		return PolicyTypeRefill
	case "health_check", "healthcheck", "account_health":
		return PolicyTypeHealth
	default:
		return normalized
	}
}

func decodeAutomationPolicies(raw []byte) (map[string]*Policy, error) {
	var root interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	out := map[string]*Policy{}
	if err := collectAutomationPolicies(root, "", out); err != nil {
		return nil, err
	}
	return out, nil
}

func collectAutomationPolicies(root interface{}, fallbackType string, out map[string]*Policy) error {
	switch value := root.(type) {
	case []interface{}:
		for _, item := range value {
			if err := collectAutomationPolicies(item, "", out); err != nil {
				return err
			}
		}
		return nil
	case map[string]interface{}:
		if nested, ok := value["policies"]; ok {
			return collectAutomationPolicies(nested, "", out)
		}
		if automationPolicyObject(value) {
			policy, err := decodeAutomationPolicyObject(value, fallbackType)
			if err != nil {
				return err
			}
			if existing := out[policy.Type]; existing == nil || policy.Updated >= existing.Updated {
				out[policy.Type] = policy
			}
			return nil
		}
		for key, item := range value {
			if err := collectAutomationPolicies(item, key, out); err != nil {
				return fmt.Errorf("policy %s: %w", key, err)
			}
		}
		return nil
	default:
		return errors.New("automation policies must be an object, array, or policies envelope")
	}
}

func automationPolicyObject(value map[string]interface{}) bool {
	for _, key := range []string{"type", "policy_type", "policyType", "enabled", "active", "config", "settings", "options"} {
		if _, ok := value[key]; ok {
			return true
		}
	}
	return false
}

func decodeAutomationPolicyObject(value map[string]interface{}, fallbackType string) (*Policy, error) {
	policyType, err := registrationString(firstLegacyValue(value, "type", "policy_type", "policyType", "kind"))
	if err != nil {
		return nil, err
	}
	policyType = normalizeAutomationPolicyType(firstNonEmpty(policyType, fallbackType))
	if policyType == "" {
		return nil, errors.New("policy type is required")
	}
	enabled, err := registrationBool(firstLegacyValue(value, "enabled", "active", "is_enabled", "isEnabled"))
	if err != nil {
		return nil, fmt.Errorf("enabled: %w", err)
	}
	configValue := firstLegacyValue(value, "config", "settings", "options")
	configMap := map[string]interface{}{}
	if configValue != nil {
		var ok bool
		configMap, ok = configValue.(map[string]interface{})
		if !ok {
			return nil, errors.New("policy config must be an object")
		}
	}
	id, err := registrationString(firstLegacyValue(value, "id", "policy_id", "policyId"))
	if err != nil {
		return nil, err
	}
	created := automationInt64(firstLegacyValue(value, "created_at", "createdAt", "created"))
	updated := automationInt64(firstLegacyValue(value, "updated_at", "updatedAt", "updated"))
	return &Policy{
		ID: firstNonEmpty(id, policyType), Type: policyType, Enabled: enabled,
		Config: canonicalizeAutomationConfig(configMap), Created: created, Updated: updated,
	}, nil
}

func automationInt64(value interface{}) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case json.Number:
		value, _ := typed.Int64()
		return value
	case string:
		value, _ := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return value
	default:
		return 0
	}
}

func canonicalizeAutomationConfig(input map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for key, value := range input {
		out[key] = value
	}
	aliases := map[string][]string{
		"target":                      {"desired_count", "desiredCount", "pool_size", "poolSize"},
		"threshold":                   {"min_accounts", "minAccounts", "refill_threshold", "refillThreshold"},
		"interval":                    {"interval_seconds", "intervalSeconds", "check_interval", "checkInterval"},
		"register_method":             {"method", "engine", "registration_method", "registerMethod"},
		"identity_mode":               {"identity", "identityMode"},
		"group":                       {"group_name", "groupName"},
		"egress_id":                   {"egress", "egressId"},
		"registration_egress_pool_id": {"egress_pool_id", "egressPoolId", "registration_pool_id", "registrationPoolId"},
		"sms_provider":                {"smsProvider", "sms_platform", "smsPlatform"},
		"mailbox_provider":            {"mailboxProvider", "mail_provider", "mailProvider", "email_provider", "emailProvider"},
		"mailbox_domain":              {"mailboxDomain", "mail_domain", "mailDomain"},
		"captcha_solver":              {"captchaSolver", "captcha_provider", "captchaProvider"},
	}
	for canonical, legacyKeys := range aliases {
		if _, present := out[canonical]; !present {
			for _, legacy := range legacyKeys {
				if value, ok := input[legacy]; ok {
					out[canonical] = value
					break
				}
			}
		}
		for _, legacy := range legacyKeys {
			delete(out, legacy)
		}
	}
	if method, ok := out["register_method"].(string); ok {
		out["register_method"] = normalizeRegistrationMethodAlias(method)
	}
	if identity, ok := out["identity_mode"].(string); ok {
		identity = strings.ToLower(strings.TrimSpace(identity))
		if identity == "sms" {
			identity = "phone"
		} else if identity == "mail" || identity == "mailbox" {
			identity = "email"
		}
		out["identity_mode"] = identity
	}
	if mailbox, ok := out["mailbox_provider"].(string); ok {
		out["mailbox_provider"] = normalizeMailboxProviderAlias(mailbox)
	}
	return out
}

func decodeAutomationPolicyRequest(reader io.Reader) (*Policy, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, adminJSONBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > adminJSONBodyLimit {
		return nil, errors.New("automation policy request exceeds request limit")
	}
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	return decodeAutomationPolicyObject(root, "")
}
