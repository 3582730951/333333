package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnthropicRequestToResponsesNativeClaudeCodeShape(t *testing.T) {
	longTool := "mcp__workspace__" + strings.Repeat("very_long_tool_name_", 5)
	in := []byte(`{
	  "model":"gpt-5.6-sol",
	  "max_tokens":64000,
	  "temperature":0.2,
	  "top_p":0.9,
	  "stream":false,
	  "system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.207\r\n\r\nYou are Claude Code."}],
	  "output_config":{"effort":"xhigh"},
	  "metadata":{"user_id":"session-42"},
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]},
	    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"` + longTool + `","input":{"path":"a.go","integer_id":9007199254740993}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[{"type":"text","text":"missing"}]}]}
	  ],
	  "tools":[{"name":"` + longTool + `","description":"inspect a file","input_schema":{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"path":{"type":"string","cache_control":{"type":"ephemeral"}}}}}],
	  "tool_choice":{"type":"tool","name":"` + longTool + `","disable_parallel_tool_use":true}
	}`)

	converted, err := AnthropicRequestToResponses(in)
	if err != nil {
		t.Fatal(err)
	}
	root := mustUnmarshal(t, converted.Body)
	if root["model"] != "gpt-5.6-sol" || root["stream"] != true || root["store"] != false {
		t.Fatalf("native Responses envelope wrong: %s", converted.Body)
	}
	if root["instructions"] != "You are Claude Code." {
		t.Fatalf("Claude billing preamble was not stripped: %q", root["instructions"])
	}
	for _, field := range []string{"max_output_tokens", "max_tokens", "temperature", "top_p"} {
		if _, present := root[field]; present {
			t.Fatalf("OAuth-incompatible field %q leaked: %s", field, converted.Body)
		}
	}
	reasoning := root["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "xhigh" || reasoning["summary"] != "auto" {
		t.Fatalf("Claude effort not mapped: %v", reasoning)
	}
	include := root["include"].([]interface{})
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("encrypted reasoning include missing: %v", include)
	}
	if root["parallel_tool_calls"] != false {
		t.Fatalf("parallel tool policy not mapped: %v", root["parallel_tool_calls"])
	}

	tools := root["tools"].([]interface{})
	tool := tools[0].(map[string]interface{})
	wireName := tool["name"].(string)
	if len(wireName) > codexToolNameLimit || converted.ToolNames[wireName] != longTool {
		t.Fatalf("tool name shortening is not reversible: wire=%q map=%v", wireName, converted.ToolNames)
	}
	parameters := tool["parameters"].(map[string]interface{})
	if _, leaked := parameters["$schema"]; leaked {
		t.Fatalf("schema metadata leaked: %v", parameters)
	}
	choice := root["tool_choice"].(map[string]interface{})
	if choice["name"] != wireName {
		t.Fatalf("tool_choice did not use wire name: %v", choice)
	}

	input := root["input"].([]interface{})
	if len(input) != 3 {
		t.Fatalf("want typed user message, function call, and result; got %d: %v", len(input), input)
	}
	userParts := input[0].(map[string]interface{})["content"].([]interface{})
	if userParts[0].(map[string]interface{})["type"] != "input_text" || userParts[1].(map[string]interface{})["type"] != "input_image" {
		t.Fatalf("typed user content lost: %v", userParts)
	}
	call := input[1].(map[string]interface{})
	if call["type"] != "function_call" || call["name"] != wireName || !strings.Contains(call["arguments"].(string), "9007199254740993") {
		t.Fatalf("function call conversion lost identity or integer precision: %v", call)
	}
	result := input[2].(map[string]interface{})
	if result["type"] != "function_call_output" || result["call_id"] != "toolu_1" {
		t.Fatalf("tool result conversion wrong: %v", result)
	}
	output := result["output"].([]interface{})
	if output[0].(map[string]interface{})["text"] != toolResultErrorMarker {
		t.Fatalf("tool error marker missing: %v", output)
	}
}

func TestAnthropicRequestToResponsesMakesBuiltInAgentInheritCodexModel(t *testing.T) {
	in := []byte(`{
	  "model":"gpt-5.6-sol",
	  "messages":[{"role":"user","content":"delegate"}],
	  "tools":[
	    {
	      "name":"Agent",
	      "description":"Launch a new agent",
	      "input_schema":{
	        "$schema":"https://json-schema.org/draft/2020-12/schema",
	        "type":"object",
	        "properties":{
	          "description":{"type":"string"},
	          "prompt":{"type":"string"},
	          "subagent_type":{"type":"string"},
	          "model":{"type":"string","enum":["sonnet","opus","haiku","fable"]},
	          "run_in_background":{"type":"boolean"},
	          "isolation":{"type":"string","enum":["worktree","remote"]}
	        },
	        "required":["description","prompt"],
	        "additionalProperties":false
	      }
	    },
	    {
	      "name":"Workflow",
	      "input_schema":{
	        "type":"object",
	        "properties":{"name":{"type":"string"},"model":{"type":"string"}},
	        "required":["name"]
	      }
	    }
	  ]
	}`)

	converted, err := AnthropicRequestToResponses(in)
	if err != nil {
		t.Fatal(err)
	}
	if !converted.InheritModelTools["Agent"] || converted.InheritModelTools["Workflow"] {
		t.Fatalf("inherit-model tool detection wrong: %v", converted.InheritModelTools)
	}
	root := mustUnmarshal(t, converted.Body)
	tools := root["tools"].([]interface{})
	if len(tools) != 2 {
		t.Fatalf("Claude Code tools were dropped: %v", tools)
	}
	agentParameters := tools[0].(map[string]interface{})["parameters"].(map[string]interface{})
	agentProperties := agentParameters["properties"].(map[string]interface{})
	if _, leaked := agentProperties["model"]; leaked {
		t.Fatalf("built-in Agent.model reached Codex: %v", agentProperties)
	}
	for _, want := range []string{"description", "prompt", "subagent_type", "run_in_background", "isolation"} {
		if _, ok := agentProperties[want]; !ok {
			t.Fatalf("Agent property %q was lost: %v", want, agentProperties)
		}
	}
	workflowProperties := tools[1].(map[string]interface{})["parameters"].(map[string]interface{})["properties"].(map[string]interface{})
	if _, ok := workflowProperties["model"]; !ok {
		t.Fatalf("non-Agent model property must be preserved: %v", workflowProperties)
	}

	responses := []byte(`{
	  "id":"resp_agent","model":"gpt-5.6-sol","status":"completed",
	  "output":[{
	    "type":"function_call","call_id":"call_agent","name":"Agent",
	    "arguments":"{\"description\":\"scan repo\",\"prompt\":\"inspect\",\"subagent_type\":\"general-purpose\",\"model\":\"haiku\"}"
	  }]
	}`)
	messageRaw, err := ResponsesToAnthropicResponse(responses, "gpt-5.6-sol", nil, converted.InheritModelTools)
	if err != nil {
		t.Fatal(err)
	}
	message := mustUnmarshal(t, messageRaw)
	input := message["content"].([]interface{})[0].(map[string]interface{})["input"].(map[string]interface{})
	if _, leaked := input["model"]; leaked {
		t.Fatalf("Agent.model reached Claude Code response: %v", input)
	}
	if input["subagent_type"] != "general-purpose" || input["prompt"] != "inspect" {
		t.Fatalf("Agent input was over-sanitized: %v", input)
	}
}

func TestAnthropicRequestToResponsesLeavesCustomAgentModelUntouched(t *testing.T) {
	in := []byte(`{
	  "model":"gpt-5.6-sol",
	  "messages":[{"role":"user","content":"run custom tool"}],
	  "tools":[{
	    "name":"Agent",
	    "input_schema":{
	      "type":"object",
	      "properties":{"task":{"type":"string"},"model":{"type":"string","enum":["fast"]}},
	      "required":["task"]
	    }
	  }]
	}`)
	converted, err := AnthropicRequestToResponses(in)
	if err != nil {
		t.Fatal(err)
	}
	if converted.InheritModelTools["Agent"] {
		t.Fatalf("custom Agent was mistaken for Claude Code built-in: %v", converted.InheritModelTools)
	}
	root := mustUnmarshal(t, converted.Body)
	properties := root["tools"].([]interface{})[0].(map[string]interface{})["parameters"].(map[string]interface{})["properties"].(map[string]interface{})
	if _, ok := properties["model"]; !ok {
		t.Fatalf("custom Agent.model was removed: %v", properties)
	}
}

func TestResponsesToAnthropicResponseSupportsStableToolKinds(t *testing.T) {
	responses := []byte(`{
	  "id":"resp_tools","model":"gpt-5.6-sol","status":"completed",
	  "output":[
	    {"type":"function_call","call_id":"call_fn","namespace":"apps","name":"lookup","arguments":"{\"id\":900719925474099312345}"},
	    {"type":"custom_tool_call","call_id":"call_custom","name":"shell_text","input":"echo hello"},
	    {"type":"tool_search_call","call_id":"call_search","execution":"client","arguments":{"query":"calendar","limit":900719925474099312345}}
	  ]
	}`)
	anthropic, err := ResponsesToAnthropicResponse(responses, "gpt-5.6-sol", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(anthropic), "900719925474099312345") {
		t.Fatalf("large tool argument was rounded: %s", anthropic)
	}
	message := mustUnmarshal(t, anthropic)
	content := message["content"].([]interface{})
	if len(content) != 3 {
		t.Fatalf("stable tool calls were dropped: %s", anthropic)
	}
	function := content[0].(map[string]interface{})
	if function["type"] != "tool_use" || function["name"] == "lookup" || len(function["name"].(string)) > 64 {
		t.Fatalf("namespace function identity was not safely flattened: %v", function)
	}
	custom := content[1].(map[string]interface{})
	if custom["name"] != "shell_text" || custom["input"].(map[string]interface{})["input"] != "echo hello" {
		t.Fatalf("custom tool call was not preserved: %v", custom)
	}
	search := content[2].(map[string]interface{})
	if search["name"] != "tool_search" || search["input"].(map[string]interface{})["query"] != "calendar" {
		t.Fatalf("client tool search was not preserved: %v", search)
	}
}

func TestResponsesReasoningEnvelopeReplaysOnNextClaudeTurn(t *testing.T) {
	responses := []byte(`{
	  "id":"resp_reasoning","model":"gpt-5.6-sol","status":"completed",
	  "output":[
	    {"type":"reasoning","id":"rs_1","encrypted_content":"opaque-state","summary":[{"type":"summary_text","text":"Checked the repository."}]},
	    {"type":"function_call","id":"fc_1","call_id":"call_1","name":"Read","arguments":"{\"file_path\":\"a.go\",\"pages\":\"\"}"}
	  ],
	  "usage":{"input_tokens":20,"output_tokens":8,"input_tokens_details":{"cached_tokens":5}}
	}`)
	anthropic, err := ResponsesToAnthropicResponse(responses, "gpt-5.6-sol", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	message := mustUnmarshal(t, anthropic)
	content := message["content"].([]interface{})
	if len(content) != 2 || content[0].(map[string]interface{})["type"] != "thinking" {
		t.Fatalf("reasoning block missing: %s", anthropic)
	}
	thinking := content[0].(map[string]interface{})
	signature := thinking["signature"].(string)
	if !strings.HasPrefix(signature, openAIReasoningEnvelopePrefix) {
		t.Fatalf("opaque reasoning signature missing: %v", thinking)
	}
	tool := content[1].(map[string]interface{})
	if _, leaked := tool["input"].(map[string]interface{})["pages"]; leaked {
		t.Fatalf("empty Read.pages should be sanitized: %v", tool)
	}
	usage := message["usage"].(map[string]interface{})
	if usage["input_tokens"] != float64(15) || usage["cache_read_input_tokens"] != float64(5) {
		t.Fatalf("Anthropic cache buckets wrong: %v", usage)
	}

	nextRoot := map[string]interface{}{
		"model": "gpt-5.6-sol",
		"messages": []interface{}{
			map[string]interface{}{"role": "assistant", "content": []interface{}{
				thinking,
				map[string]interface{}{"type": "tool_use", "id": tool["id"], "name": tool["name"], "input": tool["input"]},
			}},
			map[string]interface{}{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": tool["id"], "content": "package main"},
			}},
		},
		"tools": []interface{}{map[string]interface{}{"name": "Read", "input_schema": map[string]interface{}{"type": "object"}}},
	}
	nextRaw, _ := json.Marshal(nextRoot)
	next, err := AnthropicRequestToResponses(nextRaw)
	if err != nil {
		t.Fatal(err)
	}
	converted := mustUnmarshal(t, next.Body)
	input := converted["input"].([]interface{})
	if len(input) != 3 || input[0].(map[string]interface{})["type"] != "reasoning" ||
		input[1].(map[string]interface{})["type"] != "function_call" || input[2].(map[string]interface{})["type"] != "function_call_output" {
		t.Fatalf("reasoning/tool replay ordering lost: %v", input)
	}
	if input[0].(map[string]interface{})["encrypted_content"] != "opaque-state" {
		t.Fatalf("encrypted reasoning state changed: %v", input[0])
	}

	orphanRoot := map[string]interface{}{
		"model":    "gpt-5.6-sol",
		"messages": []interface{}{map[string]interface{}{"role": "assistant", "content": []interface{}{thinking}}},
	}
	orphanRaw, _ := json.Marshal(orphanRoot)
	orphan, err := AnthropicRequestToResponses(orphanRaw)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustUnmarshal(t, orphan.Body)["input"].([]interface{}); len(got) != 0 {
		t.Fatalf("orphan reasoning item reached Codex: %v", got)
	}
}

func TestAnthropicRequestToResponsesMapsClaudeMaximumEffortToXHigh(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-5.6-sol","output_config":{"effort":"max"},"messages":[{"role":"user","content":"solve"}]}`,
		`{"model":"gpt-5.6-sol","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"solve"}]}`,
	} {
		converted, err := AnthropicRequestToResponses([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		reasoning := mustUnmarshal(t, converted.Body)["reasoning"].(map[string]interface{})
		if reasoning["effort"] != "xhigh" {
			t.Fatalf("Claude maximum effort mapped to %v", reasoning["effort"])
		}
	}
}
