package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/hostbridge"
)

func TestBuildQoderMessagesDeveloperInstructions(t *testing.T) {
	template := []interface{}{map[string]interface{}{"role": "system", "content": "template instructions"}}
	for _, content := range []interface{}{
		"caller instructions",
		[]interface{}{
			map[string]interface{}{"type": "text", "text": "caller"},
			map[string]interface{}{"type": "input_text", "text": "instructions"},
		},
	} {
		t.Run(fmt.Sprintf("%T", content), func(t *testing.T) {
			incoming := []interface{}{
				map[string]interface{}{"role": "developer", "content": content},
				map[string]interface{}{"role": "user", "content": "hello"},
			}
			before, err := json.Marshal(incoming)
			if err != nil {
				t.Fatal(err)
			}
			messages := BuildQoderMessages(template, incoming, "hello", false)
			if len(messages) != 2 {
				t.Fatalf("message count = %d, want 2 without template instructions", len(messages))
			}
			instruction := messages[0].(map[string]interface{})
			if instruction["role"] != "system" {
				t.Errorf("instruction role = %v, want system", instruction["role"])
			}
			wantText := "caller instructions"
			if _, blocks := content.([]interface{}); blocks {
				wantText = "caller\n\ninstructions"
			}
			if instruction["content"] != wantText {
				t.Errorf("instruction content = %q, want %q", instruction["content"], wantText)
			}
			after, err := json.Marshal(incoming)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("conversion mutated the incoming messages")
			}
		})
	}

	messages := BuildQoderMessages(template, []interface{}{
		map[string]interface{}{"role": "user", "content": "hello"},
	}, "hello", false)
	if len(messages) != 2 || messages[0].(map[string]interface{})["content"] != "template instructions" {
		t.Fatalf("missing template instructions for a request without caller instructions: %v", messages)
	}
}

func TestChatCompletionsDeveloperInstructions(t *testing.T) {
	for _, target := range []struct {
		name        string
		region      Region
		vpcEndpoint string
	}{
		{"cn", RegionCN, ""},
		{"global", RegionGlobal, ""},
		{"vpc", RegionCN, "instance.vpc.qoder.com.cn"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", target.name, stream), func(t *testing.T) {
				endpoints, errEndpoints := resolveEndpoints(target.region, target.vpcEndpoint)
				if errEndpoints != nil {
					t.Fatal(errEndpoints)
				}
				var upstreamMessages []map[string]interface{}
				streamRead := false
				hostbridge.Configure(func(_ string, method string, payload any) (json.RawMessage, error) {
					switch method {
					case pluginabi.MethodHostHTTPDoStream:
						data, err := json.Marshal(payload)
						if err != nil {
							return nil, err
						}
						var request struct {
							URL  string
							Body []byte
						}
						if err := json.Unmarshal(data, &request); err != nil {
							return nil, err
						}
						if request.URL != endpoints.ChatStreamURL {
							return nil, fmt.Errorf("upstream URL = %q, want %q", request.URL, endpoints.ChatStreamURL)
						}
						body, err := Decode(string(request.Body))
						if err != nil {
							return nil, err
						}
						var requestBody struct{ Messages []map[string]interface{} }
						if err := json.Unmarshal(body, &requestBody); err != nil {
							return nil, err
						}
						upstreamMessages = requestBody.Messages
						for _, message := range upstreamMessages {
							if message["role"] == "developer" {
								return nil, fmt.Errorf("upstream 400: unsupported developer role")
							}
						}
						return json.RawMessage(`{"status_code":200,"stream_id":"instructions-test"}`), nil
					case pluginabi.MethodHostHTTPStreamRead:
						if streamRead {
							return json.RawMessage(`{"done":true}`), nil
						}
						streamRead = true
						frame := "data: {\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"role\\\":\\\"assistant\\\",\\\"content\\\":\\\"OK\\\"}}]}\",\"statusCodeValue\":200}\n\n"
						return json.Marshal(map[string]interface{}{"payload": []byte(frame)})
					case pluginabi.MethodHostHTTPStreamClose:
						return json.RawMessage(`{}`), nil
					default:
						return nil, fmt.Errorf("unexpected host method %s", method)
					}
				})
				t.Cleanup(func() { hostbridge.Configure(nil) })
				session := &SessionContext{}
				bridge := &Bridge{
					region:    target.region,
					endpoints: &endpoints,
					sess:      session,
					client:    NewBearerClient(session),
					templateBase: map[string]interface{}{
						"messages": []interface{}{map[string]interface{}{"role": "system", "content": "template instructions"}},
					},
				}
				payload := []byte(`{"model":"deepseek-flash","messages":[
					{"role":"system","content":"base instructions"},
					{"role":"developer","content":"first instruction"},
					{"role":"user","content":"hello"},
					{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"inspect","arguments":"{}"}}]},
					{"role":"tool","tool_call_id":"call_1","content":"result"},
					{"role":"developer","content":[{"type":"text","text":"second instruction"}]},
					{"role":"latest_reminder","content":"reminder"},
					{"role":"user","content":"continue"}
				],"tools":[{"type":"function","function":{"name":"inspect","parameters":{"type":"object"}}}]}`)
				var output strings.Builder
				var err error
				if stream {
					err = StreamChatCompletions(context.Background(), bridge, payload, func(chunk []byte) error {
						_, err := output.Write(chunk)
						return err
					})
				} else {
					err = RunChatCompletions(bridge, payload, &output)
				}
				if err != nil {
					t.Fatalf("chat request failed: %v", err)
				}
				if !strings.Contains(output.String(), `"content":"OK"`) {
					t.Fatalf("missing upstream reply: %s", output.String())
				}
				roles := make([]string, len(upstreamMessages))
				for i, message := range upstreamMessages {
					roles[i], _ = message["role"].(string)
				}
				wantRoles := []string{"system", "system", "user", "assistant", "tool", "system", "latest_reminder", "user"}
				if !reflect.DeepEqual(roles, wantRoles) {
					t.Fatalf("upstream roles = %v, want %v", roles, wantRoles)
				}
				if upstreamMessages[1]["content"] != "first instruction" || upstreamMessages[5]["content"] != "second instruction" {
					t.Fatal("developer instruction content was lost")
				}
				toolCalls := upstreamMessages[3]["tool_calls"].([]interface{})
				if toolCalls[0].(map[string]interface{})["id"] != "call_1" || upstreamMessages[4]["tool_call_id"] != "call_1" {
					t.Fatal("tool call correlation was lost")
				}
			})
		}
	}
}

func TestReadQoderResponseBodyLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, strings.Repeat("x", int(shortResponseMaxBytes)+1))
	}))
	defer server.Close()

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if _, err = readQoderResponseBody(resp.Body); err == nil {
		t.Fatal("expected oversized response to be rejected")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unexpected oversized response error: %v", err)
	}
}

func TestReadQoderResponseBodyAllowsNormalResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := readQoderResponseBody(resp.Body)
	if err != nil {
		t.Fatalf("normal response rejected: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
}

func TestMergeToolCallDeltas(t *testing.T) {
	delta1 := []interface{}{
		map[string]interface{}{
			"index": 0,
			"id":    "call_bash_1",
			"type":  "function",
			"function": map[string]interface{}{
				"name":      "Bash",
				"arguments": "{\"command\": \"",
			},
		},
	}
	delta2 := []interface{}{
		map[string]interface{}{
			"index": 0,
			"function": map[string]interface{}{
				"arguments": "ls -la\"}",
			},
		},
	}
	delta3Bad := []interface{}{
		map[string]interface{}{
			"index": 1,
			"id":    "call_unnamed",
			"type":  "function",
			"function": map[string]interface{}{
				"name":      "",
				"arguments": "{}",
			},
		},
	}

	var accumulated []interface{}
	accumulated = mergeToolCallDeltas(accumulated, delta1)
	accumulated = mergeToolCallDeltas(accumulated, delta2)
	accumulated = mergeToolCallDeltas(accumulated, delta3Bad)

	valid := filterValidToolCalls(accumulated)
	if len(valid) != 1 {
		t.Fatalf("expected 1 valid tool call after filtering unnamed calls, got %d", len(valid))
	}

	call := valid[0].(map[string]interface{})
	if call["id"] != "call_bash_1" {
		t.Errorf("expected id 'call_bash_1', got %v", call["id"])
	}
	fn := call["function"].(map[string]interface{})
	if fn["name"] != "Bash" {
		t.Errorf("expected name 'Bash', got %v", fn["name"])
	}
	if fn["arguments"] != `{"command": "ls -la"}` {
		t.Errorf("expected merged arguments '{\"command\": \"ls -la\"}', got %v", fn["arguments"])
	}
}

// ==================== 专有协议测试 ====================

func TestMapModel(t *testing.T) {
	// MapModel 只处理「索引未命中」的兜底：本插件自己注册的模型由
	// Bridge.resolveModel 先走 modelIndex（见 TestBridgeResolveModel）。
	cases := []struct {
		agent    string
		model    string
		expected string
	}{
		{"codex", "gfmodel", "gfmodel"},
		{"codex", "gmodel", "gmodel"},
		{"codex", "kmodel_latest", "kmodel_latest"},
		{"codex", "qmodel_38max", "qmodel_38max"},
		{"claude", "claude-sonnet-4-6", "gmodel"}, // sonnet 模糊匹配兜底
		{"codex", "Performance", "performance"},   // 大小写归一
	}

	for _, c := range cases {
		got := MapModel(c.agent, c.model)
		if got != c.expected {
			t.Errorf("MapModel(%q, %q) = %q; want %q", c.agent, c.model, got, c.expected)
		}
	}
}

func TestBridgeResolveModel(t *testing.T) {
	var index modelIndex
	index.replace(testModels())
	bridge := &Bridge{models: &index}

	cases := []struct {
		agent    string
		model    string
		expected string
	}{
		// 对外 slug → 上游 key
		{"codex", "deepseek-v4-pro", "dmodel"},
		{"codex", "qwen3.8-max", "qmodel_38max"},
		{"codex", "glm-5.3", "gmodel"},
		// 大小写不敏感
		{"codex", "DeepSeek-V4-Pro", "dmodel"},
		// 旧上游 key 仍可直传（向后兼容）
		{"codex", "dmodel", "dmodel"},
		{"codex", "qmodel_38max", "qmodel_38max"},
		// 索引未命中 → 内置映射兜底
		{"claude", "claude-sonnet-4-6", "gmodel"},
	}
	for _, c := range cases {
		if got := bridge.resolveModel(c.agent, c.model); got != c.expected {
			t.Errorf("resolveModel(%q, %q) = %q; want %q", c.agent, c.model, got, c.expected)
		}
	}
}

func TestBridgeResolveModelWithoutIndex(t *testing.T) {
	// 没有索引时（测试/独立构造）退回 MapModel，行为与变更前一致。
	bridge := &Bridge{}
	if got := bridge.resolveModel("claude", "claude-sonnet-4-6"); got != "gmodel" {
		t.Errorf("resolveModel without index = %q; want gmodel", got)
	}
}
