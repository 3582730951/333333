// Package agentidentity implements the OpenAI Agent Identity wire contract used
// by Codex account exports from sub2api. The exported private key is a base64
// PKCS#8 Ed25519 key; it is never logged or included in returned errors.
package agentidentity

import (
	"crypto/ed25519"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

var assertionPattern = regexp.MustCompile(`(?i)AgentAssertion\s+[A-Za-z0-9_-]+`)

const (
	CredentialMode = "agent_identity"
	ExportAuthMode = "agentIdentity"
	AuthAPIBaseURL = "https://auth.openai.com/api/accounts"
)

// Credentials is the minimum material needed for AgentAssertion authentication.
type Credentials struct {
	RuntimeID  string
	PrivateKey string
	TaskID     string
}

type assertionEnvelope struct {
	RuntimeID string `json:"agent_runtime_id"`
	TaskID    string `json:"task_id"`
	Timestamp string `json:"timestamp"`
	Signature string `json:"signature"`
}

type taskRegistrationResponse struct {
	TaskID               string `json:"task_id"`
	TaskIDCamel          string `json:"taskId"`
	EncryptedTaskID      string `json:"encrypted_task_id"`
	EncryptedTaskIDCamel string `json:"encryptedTaskId"`
}

func privateKey(encoded string) (ed25519.PrivateKey, error) {
	raw := strings.TrimSpace(encoded)
	if raw == "" {
		return nil, errors.New("agent identity private key is missing")
	}
	der, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("agent identity private key is not valid base64")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("agent identity private key is not valid PKCS#8")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("agent identity private key is not Ed25519")
	}
	return key, nil
}

// Validate verifies the credential shape without exposing key material.
func Validate(c Credentials, requireTask bool) error {
	if strings.TrimSpace(c.RuntimeID) == "" {
		return errors.New("agent identity runtime id is missing")
	}
	if requireTask && strings.TrimSpace(c.TaskID) == "" {
		return errors.New("agent identity task id is missing")
	}
	_, err := privateKey(c.PrivateKey)
	return err
}

// BuildAssertion creates the exact short-lived Authorization value expected by
// the Codex backend: AgentAssertion + base64url(JSON envelope).
func BuildAssertion(c Credentials, now time.Time) (string, error) {
	if err := Validate(c, true); err != nil {
		return "", err
	}
	key, _ := privateKey(c.PrivateKey)
	timestamp := now.UTC().Format(time.RFC3339)
	payload := []byte(strings.TrimSpace(c.RuntimeID) + ":" + strings.TrimSpace(c.TaskID) + ":" + timestamp)
	signature := ed25519.Sign(key, payload)
	envelope, err := json.Marshal(assertionEnvelope{
		RuntimeID: strings.TrimSpace(c.RuntimeID),
		TaskID:    strings.TrimSpace(c.TaskID),
		Timestamp: timestamp,
		Signature: base64.StdEncoding.EncodeToString(signature),
	})
	if err != nil {
		return "", errors.New("failed to serialize agent assertion")
	}
	return "AgentAssertion " + base64.RawURLEncoding.EncodeToString(envelope), nil
}

// BuildTaskRegistration builds the signed task-registration URL and JSON body.
func BuildTaskRegistration(c Credentials, baseURL string, now time.Time) (string, []byte, error) {
	if err := Validate(c, false); err != nil {
		return "", nil, err
	}
	key, _ := privateKey(c.PrivateKey)
	timestamp := now.UTC().Format(time.RFC3339)
	signature := ed25519.Sign(key, []byte(strings.TrimSpace(c.RuntimeID)+":"+timestamp))
	body, err := json.Marshal(map[string]string{
		"timestamp": timestamp,
		"signature": base64.StdEncoding.EncodeToString(signature),
	})
	if err != nil {
		return "", nil, errors.New("failed to serialize agent task registration")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = AuthAPIBaseURL
	}
	return baseURL + "/v1/agent/" + strings.TrimSpace(c.RuntimeID) + "/task/register", body, nil
}

// ParseTaskRegistrationResponse accepts both plain and encrypted task IDs used
// by OpenAI's registration endpoint.
func ParseTaskRegistrationResponse(c Credentials, raw []byte) (string, error) {
	var response taskRegistrationResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", errors.New("agent task registration response is invalid")
	}
	for _, candidate := range []string{response.TaskID, response.TaskIDCamel} {
		if taskID := strings.TrimSpace(candidate); taskID != "" {
			return taskID, nil
		}
	}
	encrypted := strings.TrimSpace(response.EncryptedTaskID)
	if encrypted == "" {
		encrypted = strings.TrimSpace(response.EncryptedTaskIDCamel)
	}
	if encrypted == "" {
		return "", errors.New("agent task registration response omitted task id")
	}
	return decryptTaskID(c, encrypted)
}

func decryptTaskID(c Credentials, encoded string) (string, error) {
	key, err := privateKey(c.PrivateKey)
	if err != nil {
		return "", err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", errors.New("encrypted agent task id is not valid base64")
	}
	digest := sha512.Sum512(key.Seed())
	var curvePrivate [32]byte
	copy(curvePrivate[:], digest[:32])
	curvePrivate[0] &= 248
	curvePrivate[31] &= 127
	curvePrivate[31] |= 64
	publicBytes, err := curve25519.X25519(curvePrivate[:], curve25519.Basepoint)
	if err != nil {
		return "", errors.New("failed to derive agent identity decryption key")
	}
	var curvePublic [32]byte
	copy(curvePublic[:], publicBytes)
	plaintext, ok := box.OpenAnonymous(nil, ciphertext, &curvePublic, &curvePrivate)
	if !ok {
		return "", errors.New("failed to decrypt encrypted agent task id")
	}
	taskID := strings.TrimSpace(string(plaintext))
	if taskID == "" {
		return "", errors.New("decrypted agent task id is empty")
	}
	return taskID, nil
}

// InvalidTaskResponse recognizes only task-lifecycle 401s. Other 401s remain
// normal account-auth failures and must not trigger a task rotation.
func InvalidTaskResponse(status int, body []byte) bool {
	if status != http.StatusUnauthorized {
		return false
	}
	lower := strings.ToLower(string(body))
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(lower)
	for _, marker := range []string{
		`"code":"invalid_task_id"`,
		`"code":"task_not_found"`,
		`"code":"task_expired"`,
		`"error":"invalid_task_id"`,
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	for _, marker := range []string{
		"invalid task_id", "invalid task id", "task_id is invalid", "task id is invalid",
		"task not found", "task expired", "unknown task_id", "unknown task id",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func RegistrationStatusError(status int) error {
	return fmt.Errorf("agent task registration returned status %d", status)
}

// RedactSensitive defensively removes Agent Identity material from an upstream
// error before it reaches logs, audit records, or a downstream client.
func RedactSensitive(body []byte, c Credentials) []byte {
	redacted := string(body)
	for _, secret := range []string{c.RuntimeID, c.PrivateKey, c.TaskID} {
		if secret = strings.TrimSpace(secret); secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
		}
	}
	redacted = assertionPattern.ReplaceAllString(redacted, "AgentAssertion [REDACTED]")
	return []byte(redacted)
}
