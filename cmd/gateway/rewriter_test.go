package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRewriteBodyTargetsOfficialIdentityWithoutChangingContextOrTools(t *testing.T) {
	id := gatewayRewriteTestIdentity()
	const huge = "900719925474099312345678901234567890"
	body := []byte(`{
	  "model":"claude-opus-5",
	  "request_sequence":` + huge + `,
	  "session_id":"real-session-context",
	  "parent_thread_id":"real-parent-context",
	  "metadata":{"user_id":"real-user-id","opaque_counter":` + huge + `},
	  "system":[
	    {"type":"text","text":"You are a Claude agent, built on Anthropic's Claude Agent SDK."},
	    {"type":"text","text":"<env>\nWorking directory: /home/realuser/project\nPlatform: darwin\nOS Version: 23.0\nTerminal: Apple_Terminal\nHostname: real-host\nArchitecture: arm64\nUser: realuser\nDNS: 10.0.0.53\n</env>"},
	    {"type":"text","text":"Skill instructions: keep realuser@real-host and /home/realuser/project; Platform: darwin"}
	  ],
	  "tools":[{
	    "name":"Read",
	    "description":"Skill tool reads /home/realuser/project on real-host",
	    "input_schema":{"type":"object","properties":{"path":{"type":"string"}},"minimum":` + huge + `}
	  }],
	  "messages":[
	    {"role":"user","content":[
	      {"type":"text","text":"Use Skill at /home/realuser/project on real-host; <env>\nPlatform: darwin\n</env>"},
	      {"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"/home/realuser/project/main.go","offset":` + huge + `}},
	      {"type":"thinking","thinking":"private realuser real-host","signature":"sig-real-host","encrypted_content":"enc::realuser::/home/realuser::real-host"}
	    ]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"result /home/realuser/project real-host"}]}
	  ],
	  "encrypted_content":"opaque::realuser::/home/realuser::real-host"
	}`)

	rewritten, err := rewriteBody(body, id)
	if err != nil {
		t.Fatal(err)
	}
	got := string(rewritten)

	// JSON integers must never pass through float64. This covers top-level,
	// metadata, schema and tool-input locations.
	if count := strings.Count(got, huge); count != 4 {
		t.Fatalf("large integer count = %d, want 4\n---\n%s", count, got)
	}

	root := decodeRewriteTestObject(t, rewritten)
	if got := decodeRewriteTestString(t, root["session_id"]); got != "real-session-context" {
		t.Fatalf("session_id changed to %q", got)
	}
	if got := decodeRewriteTestString(t, root["parent_thread_id"]); got != "real-parent-context" {
		t.Fatalf("parent_thread_id changed to %q", got)
	}

	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(root["metadata"], &metadata); err != nil {
		t.Fatal(err)
	}
	encodedIdentity := decodeRewriteTestString(t, metadata["user_id"])
	var identityFields struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(encodedIdentity), &identityFields); err != nil {
		t.Fatalf("metadata.user_id is not valid embedded JSON: %v (%q)", err, encodedIdentity)
	}
	if identityFields.DeviceID != id.Virtual.UserID ||
		identityFields.AccountUUID != "" ||
		identityFields.SessionID != id.Virtual.SessionID {
		t.Fatalf("metadata.user_id fields = %#v", identityFields)
	}

	var system []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(root["system"], &system); err != nil {
		t.Fatal(err)
	}
	if len(system) != 3 {
		t.Fatalf("system block count = %d", len(system))
	}
	env := system[1].Text
	for _, want := range []string{
		"Working directory: /home/realuser/project",
		"Platform: Linux",
		"OS Version: 6.8.0",
		"Terminal: xterm-256color",
		"Hostname: virt-host",
		"Architecture: x86_64",
		"User: realuser",
		"DNS: 10.0.0.53",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("official env missing %q\n---\n%s", want, env)
		}
	}
	const skillText = "Skill instructions: keep realuser@real-host and /home/realuser/project; Platform: darwin"
	if system[2].Text != skillText {
		t.Fatalf("Skill system text changed:\n got: %q\nwant: %q", system[2].Text, skillText)
	}

	// These literals exercise user messages, Skill/tool definitions, tool input,
	// tool results, signatures and encrypted/opaque fields. None is identity
	// metadata, so all must survive unchanged.
	for _, want := range []string{
		`Skill tool reads /home/realuser/project on real-host`,
		`Use Skill at /home/realuser/project on real-host; <env>\nPlatform: darwin\n</env>`,
		`"/home/realuser/project/main.go"`,
		`private realuser real-host`,
		`sig-real-host`,
		`enc::realuser::/home/realuser::real-host`,
		`result /home/realuser/project real-host`,
		`opaque::realuser::/home/realuser::real-host`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("protected payload changed or missing %q\n---\n%s", want, got)
		}
	}
}

func TestRewriteBodyDoesNotInferUserOrSkillEnvAsIdentity(t *testing.T) {
	id := gatewayRewriteTestIdentity()
	body := []byte(`{
	  "metadata":{"user_id":"real"},
	  "system":[
	    {"type":"text","text":"Custom Skill instructions"},
	    {"type":"text","text":"<env>\nPlatform: darwin\nHostname: real-host\nWorking directory: /home/realuser/project\n</env>"}
	  ],
	  "messages":[{"role":"user","content":"<env>\nPlatform: darwin\nHostname: real-host\n</env>"}]
	}`)

	rewritten, err := rewriteBody(body, id)
	if err != nil {
		t.Fatal(err)
	}
	root := decodeRewriteTestObject(t, rewritten)
	var system []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(root["system"], &system); err != nil {
		t.Fatal(err)
	}
	if got := system[1].Text; got != "<env>\nPlatform: darwin\nHostname: real-host\nWorking directory: /home/realuser/project\n</env>" {
		t.Fatalf("non-official system env changed: %q", got)
	}
	var messages []struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(root["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if got := messages[0].Content; got != "<env>\nPlatform: darwin\nHostname: real-host\n</env>" {
		t.Fatalf("user env changed: %q", got)
	}
}

func TestRewriteBodyNoIdentityFieldsIsExactPassThrough(t *testing.T) {
	id := gatewayRewriteTestIdentity()
	body := []byte(" {\n  \"messages\": [{\"role\":\"user\",\"content\":\"realuser /home/realuser real-host\"}],\n  \"value\": 9007199254740993123456789\n} \n")
	rewritten, err := rewriteBody(body, id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rewritten, body) {
		t.Fatalf("body without identity fields changed:\n got: %q\nwant: %q", rewritten, body)
	}
}

func TestRewriteBodyMalformedJSONIsExactPassThrough(t *testing.T) {
	id := gatewayRewriteTestIdentity()
	body := []byte(`{"metadata":{"user_id":"real"},"messages":[`)
	rewritten, err := rewriteBody(body, id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rewritten, body) {
		t.Fatalf("malformed upstream payload changed: %q", rewritten)
	}
}

func TestRewriteMetadataUserIDEscapesVirtualValues(t *testing.T) {
	id := gatewayRewriteTestIdentity()
	id.Virtual.UserID = `device"quoted`
	id.Virtual.SessionID = `session\quoted`
	body := []byte(`{"metadata":{"user_id":"real"},"counter":9007199254740993}`)

	rewritten, err := rewriteMetadataUserID(body, id)
	if err != nil {
		t.Fatal(err)
	}
	root := decodeRewriteTestObject(t, rewritten)
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(root["metadata"], &metadata); err != nil {
		t.Fatal(err)
	}
	embedded := decodeRewriteTestString(t, metadata["user_id"])
	var fields map[string]string
	if err := json.Unmarshal([]byte(embedded), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["device_id"] != id.Virtual.UserID || fields["session_id"] != id.Virtual.SessionID {
		t.Fatalf("escaped identity changed: %#v", fields)
	}
	if !bytes.Contains(rewritten, []byte(`9007199254740993`)) {
		t.Fatalf("large integer changed: %s", rewritten)
	}
}

func gatewayRewriteTestIdentity() *CachedIdentity {
	return &CachedIdentity{
		Local: &LocalEnvironment{
			Username:   "realuser",
			Hostname:   "real-host",
			HomeDir:    "/home/realuser",
			WorkDir:    "/workspace/project",
			DNSServers: []string{"10.0.0.53", "10.0.0.54"},
		},
		Virtual: &VirtualIdentity{
			UserID:     "virtual-device-id",
			SessionID:  "virtual-session-id",
			Username:   "virtuser",
			Hostname:   "virt-host",
			HomeDir:    "/home/virtuser",
			OSName:     "Linux",
			OSRelease:  "6.8.0",
			Arch:       "x86_64",
			Terminal:   "xterm-256color",
			DNSServers: []string{"1.1.1.1", "1.0.0.1"},
			ProcessInfo: ProcessInfo{
				CWD: "/home/virtuser/workspace/project",
			},
		},
		FetchedAt: time.Now(),
	}
}

func decodeRewriteTestObject(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]json.RawMessage
	if err := decoder.Decode(&root); err != nil {
		t.Fatal(err)
	}
	return root
}

func decodeRewriteTestString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
