package agentidentity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

func testCredentials(t *testing.T) (Credentials, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return Credentials{RuntimeID: "agent-runtime", PrivateKey: base64.StdEncoding.EncodeToString(der), TaskID: "task-one"}, publicKey
}

func TestBuildAssertionMatchesSub2APIWireContract(t *testing.T) {
	credentials, publicKey := testCredentials(t)
	now := time.Date(2026, 7, 21, 9, 10, 19, 0, time.FixedZone("offset", 8*60*60))
	authorization, err := BuildAssertion(credentials, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(authorization, "AgentAssertion ") {
		t.Fatalf("authorization = %q", authorization)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(authorization, "AgentAssertion "))
	if err != nil {
		t.Fatal(err)
	}
	var envelope assertionEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.RuntimeID != credentials.RuntimeID || envelope.TaskID != credentials.TaskID || envelope.Timestamp != "2026-07-21T01:10:19Z" {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(publicKey, []byte("agent-runtime:task-one:2026-07-21T01:10:19Z"), signature) {
		t.Fatal("assertion signature did not verify")
	}
}

func TestTaskRegistrationAndSensitiveRedaction(t *testing.T) {
	credentials, _ := testCredentials(t)
	url, body, err := BuildTaskRegistration(credentials, "https://auth.example.test/api/accounts/", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://auth.example.test/api/accounts/v1/agent/agent-runtime/task/register" || !json.Valid(body) {
		t.Fatalf("registration url=%q body=%s", url, body)
	}
	taskID, err := ParseTaskRegistrationResponse(credentials, []byte(`{"taskId":"task-two"}`))
	if err != nil || taskID != "task-two" {
		t.Fatalf("task=%q err=%v", taskID, err)
	}
	if !InvalidTaskResponse(401, []byte(`{"error":{"code":"invalid_task_id"}}`)) || InvalidTaskResponse(403, []byte(`{"error":{"code":"invalid_task_id"}}`)) {
		t.Fatal("invalid-task classifier mismatch")
	}
	redacted := string(RedactSensitive([]byte("agent-runtime task-one "+credentials.PrivateKey+" AgentAssertion abc_123"), credentials))
	for _, forbidden := range []string{"agent-runtime", "task-one", credentials.PrivateKey, "abc_123"} {
		if strings.Contains(redacted, forbidden) {
			t.Fatalf("redaction leaked %q: %s", forbidden, redacted)
		}
	}
}

func TestParseEncryptedTaskRegistrationResponse(t *testing.T) {
	credentials, _ := testCredentials(t)
	key, err := privateKey(credentials.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha512.Sum512(key.Seed())
	var curvePrivate [32]byte
	copy(curvePrivate[:], digest[:32])
	curvePrivate[0] &= 248
	curvePrivate[31] &= 127
	curvePrivate[31] |= 64
	publicBytes, err := curve25519.X25519(curvePrivate[:], curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	var curvePublic [32]byte
	copy(curvePublic[:], publicBytes)
	ciphertext, err := box.SealAnonymous(nil, []byte("task-encrypted"), &curvePublic, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"encrypted_task_id": base64.StdEncoding.EncodeToString(ciphertext)})
	taskID, err := ParseTaskRegistrationResponse(credentials, raw)
	if err != nil || taskID != "task-encrypted" {
		t.Fatalf("task=%q err=%v", taskID, err)
	}
}
