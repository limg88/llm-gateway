package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestServer starts a gateway whose CLIs are the fakes of fake_test.go.
func newTestServer(t *testing.T, edit func(*Config)) (*Server, *httptest.Server) {
	t.Helper()
	cfg := defaultConfig()
	cfg.ClaudeBin, cfg.CodexBin = os.Args[0], os.Args[0]
	cfg.WorkDir = t.TempDir()
	cfg.Warm = 0
	cfg.RequestTimeout = 20
	if edit != nil {
		edit(&cfg)
	}
	ts := httptest.NewUnstartedServer(nil)
	cfg.ListenAddr = ts.Listener.Addr().String()
	cfg.SelfURL = "http://" + cfg.ListenAddr
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(cfg)
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(func() {
		ts.Close()
		srv.Close()
	})
	return srv, ts
}

func post(t *testing.T, ts *httptest.Server, path, key string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// sseEvents splits an event stream into its data payloads.
func sseEvents(body []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, data)
		}
	}
	return out
}

type obj = map[string]any

func user(text string) []obj { return []obj{{"role": "user", "content": text}} }

var weatherTool = obj{"type": "function", "function": obj{
	"name": "get_weather", "description": "Weather of a city",
	"parameters": obj{"type": "object", "properties": obj{"city": obj{"type": "string"}}, "required": []string{"city"}},
}}

func chatOf(t *testing.T, body []byte) ChatCompletionResponse {
	t.Helper()
	var resp ChatCompletionResponse
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Choices) != 1 {
		t.Fatalf("bad chat response: %s", body)
	}
	return resp
}

func TestChatCompletion(t *testing.T) {
	_, ts := newTestServer(t, nil)
	status, body := post(t, ts, "/v1/chat/completions", "", obj{"model": "claude/haiku", "messages": user("hi")})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	resp := chatOf(t, body)
	if got := *resp.Choices[0].Message.Content; got != "Hello, world" {
		t.Errorf("content = %q", got)
	}
	if resp.Choices[0].FinishReason != "stop" || resp.Usage == nil || resp.Usage.PromptTokens != 13 || resp.Usage.CompletionTokens != 5 {
		t.Errorf("finish/usage = %s %+v", resp.Choices[0].FinishReason, resp.Usage)
	}
}

func TestChatCompletionStream(t *testing.T) {
	_, ts := newTestServer(t, nil)
	status, body := post(t, ts, "/chat/completions", "", obj{
		"model": "claude", "messages": user("hi"), "stream": true, "stream_options": obj{"include_usage": true},
	})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	events := sseEvents(body)
	if events[len(events)-1] != "[DONE]" {
		t.Fatalf("no [DONE]: %v", events)
	}
	var text string
	var usage *Usage
	finish := ""
	for _, e := range events[:len(events)-1] {
		var c ChatCompletionChunk
		if err := json.Unmarshal([]byte(e), &c); err != nil {
			t.Fatalf("bad chunk %q", e)
		}
		if c.Usage != nil {
			usage = c.Usage
		}
		for _, ch := range c.Choices {
			text += ch.Delta.Content
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	if text != "Hello, world" || finish != "stop" || usage == nil {
		t.Errorf("text=%q finish=%q usage=%v", text, finish, usage)
	}
}

func TestPromptAndImages(t *testing.T) {
	_, ts := newTestServer(t, nil)
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	status, body := post(t, ts, "/v1/chat/completions", "", obj{"model": "claude", "messages": []obj{
		{"role": "system", "content": "Be brief."},
		{"role": "user", "content": "first question"},
		{"role": "assistant", "content": "first answer"},
		{"role": "user", "content": []obj{{"type": "text", "text": "ECHO this"}, {"type": "image_url", "image_url": obj{"url": png}}}},
	}})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	got := *chatOf(t, body).Choices[0].Message.Content
	for _, want := range []string{`system="Be brief."`, "images=1", "<user>\nfirst question\n</user>", "<assistant>\nfirst answer\n</assistant>", "[image 1]"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
}

func TestNativeFunctionCall(t *testing.T) {
	srv, ts := newTestServer(t, nil)
	start := time.Now()
	status, body := post(t, ts, "/v1/chat/completions", "", obj{
		"model": "claude", "messages": user("CALL_TOOL please"), "tools": []obj{weatherTool},
	})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the CLI was not stopped after the call")
	}
	choice := chatOf(t, body).Choices[0]
	if choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("choice = %+v", choice)
	}
	tc := choice.Message.ToolCalls[0]
	if tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"Rome"}` || !strings.HasPrefix(tc.ID, "call_") {
		t.Errorf("call = %+v", tc)
	}
	if choice.Message.Content != nil {
		t.Errorf("content = %q, want null", *choice.Message.Content)
	}
	// The functions are unpublished once the stopped CLI is gone.
	for i := 0; srv.mcp.len() != 0; i++ {
		if i == 100 {
			t.Fatalf("%d function sets still published", srv.mcp.len())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFunctionCallWrittenAsText(t *testing.T) {
	_, ts := newTestServer(t, nil)
	status, body := post(t, ts, "/v1/chat/completions", "", obj{
		"model": "claude", "messages": user("TEXT_CALL"), "tools": []obj{weatherTool},
	})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	msg := chatOf(t, body).Choices[0].Message
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Arguments != `{"city":"Rome"}` {
		t.Fatalf("message = %s", body)
	}
	if msg.Content == nil || *msg.Content != "Let me check." {
		t.Errorf("content = %v", msg.Content)
	}
}

func TestEmulatedFunctionCall(t *testing.T) {
	_, ts := newTestServer(t, nil)
	for prompt, wantCalls := range map[string]int{"CALL_TOOL": 1, "just talk": 0} {
		status, body := post(t, ts, "/v1/chat/completions", "", obj{
			"model": "codex", "messages": user(prompt), "tools": []obj{weatherTool},
		})
		if status != 200 {
			t.Fatalf("status %d: %s", status, body)
		}
		msg := chatOf(t, body).Choices[0].Message
		if len(msg.ToolCalls) != wantCalls {
			t.Errorf("%s: %d calls: %s", prompt, len(msg.ToolCalls), body)
		}
		if wantCalls == 0 && (msg.Content == nil || *msg.Content != "no call needed") {
			t.Errorf("%s: content = %v", prompt, msg.Content)
		}
	}
}

func TestStructuredOutput(t *testing.T) {
	_, ts := newTestServer(t, nil)
	status, body := post(t, ts, "/v1/chat/completions", "", obj{
		"model": "claude", "messages": user("json please"),
		"response_format": obj{"type": "json_schema", "json_schema": obj{"name": "r", "schema": obj{"type": "object"}}},
	})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	if got := *chatOf(t, body).Choices[0].Message.Content; got != `{"ok":true}` {
		t.Errorf("content = %q", got)
	}
}

func TestFallback(t *testing.T) {
	srv, ts := newTestServer(t, func(c *Config) {
		c.Models = map[string][]string{"auto": {"claude/haiku", "codex"}}
	})
	status, body := post(t, ts, "/v1/chat/completions", "", obj{"model": "auto", "messages": user("FAIL_CLAUDE")})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	if got := *chatOf(t, body).Choices[0].Message.Content; got != "codex says hi" {
		t.Errorf("content = %q", got)
	}
	var metrics bytes.Buffer
	srv.metrics.Render(&metrics)
	for _, want := range []string{`llm_gateway_fallbacks_total{from="claude"} 1`, `llm_gateway_requests_total{endpoint="chat",backend="codex",status="ok"} 1`} {
		if !strings.Contains(metrics.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, metrics.String())
		}
	}

	// Without another target the failure reaches the client.
	status, body = post(t, ts, "/v1/chat/completions", "", obj{"model": "claude", "messages": user("FAIL_CLAUDE")})
	if status != 502 || !strings.Contains(string(body), "not logged in") {
		t.Errorf("status %d: %s", status, body)
	}
	status, body = post(t, ts, "/v1/chat/completions", "", obj{"model": "codex", "messages": user("FAIL_CODEX")})
	if status != 502 || !strings.Contains(string(body), "model overloaded") {
		t.Errorf("status %d: %s", status, body)
	}
}

func TestAuthAndLimits(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) {
		c.APIKey = "main-key"
		c.APIKeys = []KeyConfig{
			{Name: "limited", Key: "limited-key", RPM: 2},
			{Name: "codex-only", Key: "codex-key", Backends: []string{"codex"}},
		}
	})
	req := obj{"model": "claude", "messages": user("hi")}
	if status, _ := post(t, ts, "/v1/chat/completions", "", req); status != 401 {
		t.Errorf("no key: status %d", status)
	}
	if status, _ := post(t, ts, "/v1/chat/completions", "wrong", req); status != 401 {
		t.Errorf("wrong key: status %d", status)
	}
	if status, body := post(t, ts, "/v1/chat/completions", "main-key", req); status != 200 {
		t.Errorf("main key: status %d: %s", status, body)
	}
	if status, _ := post(t, ts, "/v1/chat/completions", "codex-key", req); status != 403 {
		t.Errorf("restricted key: status %d", status)
	}
	codes := []int{}
	for i := 0; i < 3; i++ {
		status, _ := post(t, ts, "/v1/chat/completions", "limited-key", req)
		codes = append(codes, status)
	}
	if fmt.Sprint(codes) != "[200 200 429]" {
		t.Errorf("rate limit: %v", codes)
	}

	// x-api-key works too, and the models are those the key can use.
	r, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	r.Header.Set("x-api-key", "codex-key")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list ModelList
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Data) != 1 || list.Data[0].ID != "codex" {
		t.Errorf("models = %+v", list.Data)
	}
}

func TestBadRequests(t *testing.T) {
	_, ts := newTestServer(t, nil)
	cases := map[string]obj{
		"not supported: use claude, codex":  {"model": "gpt-4", "messages": user("hi")},
		"at least one user message":         {"model": "claude", "messages": []obj{{"role": "system", "content": "x"}}},
		"unsupported role":                  {"model": "claude", "messages": []obj{{"role": "wizard", "content": "x"}}},
		"is not in tools":                   {"model": "claude", "messages": user("hi"), "tool_choice": obj{"type": "function", "function": obj{"name": "nope"}}, "tools": []obj{weatherTool}},
		"codex does not support image URLs": {"model": "codex", "messages": []obj{{"role": "user", "content": []obj{{"type": "image_url", "image_url": obj{"url": "https://example.com/a.png"}}}}}},
	}
	for want, req := range cases {
		status, body := post(t, ts, "/v1/chat/completions", "", req)
		if status != 400 || !strings.Contains(string(body), want) {
			t.Errorf("want 400 with %q, got %d: %s", want, status, body)
		}
	}
}

func TestResponses(t *testing.T) {
	_, ts := newTestServer(t, nil)
	status, body := post(t, ts, "/v1/responses", "", obj{"model": "claude", "instructions": "Be brief.", "input": "ECHO one"})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var first Response
	json.Unmarshal(body, &first)
	if first.Status != "completed" || len(first.Output) != 1 || first.Output[0].Type != "message" || first.Usage == nil {
		t.Fatalf("response = %s", body)
	}

	// The second request continues the first.
	status, body = post(t, ts, "/v1/responses", "", obj{"model": "claude", "input": "ECHO two", "previous_response_id": first.ID})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var second Response
	json.Unmarshal(body, &second)
	got := (*second.Output[0].Content)[0].Text
	for _, want := range []string{`system="Be brief."`, "<user>\nECHO one\n</user>", "<user>\nECHO two\n</user>"} {
		if !strings.Contains(got, want) {
			t.Errorf("continued prompt lacks %q:\n%s", want, got)
		}
	}
	if status, body := post(t, ts, "/v1/responses", "", obj{"model": "claude", "input": "x", "previous_response_id": "resp_missing"}); status != 400 {
		t.Errorf("missing previous response: %d %s", status, body)
	}
}

func TestResponsesStream(t *testing.T) {
	_, ts := newTestServer(t, nil)
	status, body := post(t, ts, "/v1/responses", "", obj{
		"model": "claude", "input": "CALL_TOOL", "stream": true,
		"tools": []obj{{"type": "function", "name": "get_weather", "parameters": obj{"type": "object"}}},
	})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var types []string
	var last obj
	for i, e := range sseEvents(body) {
		var ev obj
		json.Unmarshal([]byte(e), &ev)
		if int(ev["sequence_number"].(float64)) != i {
			t.Errorf("sequence_number of event %d is %v", i, ev["sequence_number"])
		}
		types = append(types, ev["type"].(string))
		last = ev
	}
	want := "response.created response.in_progress response.output_item.added response.function_call_arguments.delta " +
		"response.function_call_arguments.delta response.function_call_arguments.done response.output_item.done response.completed"
	if got := strings.Join(types, " "); got != want {
		t.Errorf("events:\n got %s\nwant %s", got, want)
	}
	out := last["response"].(obj)["output"].([]any)[0].(obj)
	if out["type"] != "function_call" || out["arguments"] != `{"city":"Rome"}` || out["name"] != "get_weather" {
		t.Errorf("output = %v", out)
	}
}

func TestWebSearch(t *testing.T) {
	_, ts := newTestServer(t, nil)
	status, body := post(t, ts, "/v1/responses", "", obj{
		"model": "claude", "input": "SEARCH", "tools": []obj{{"type": "web_search"}}, "include": []string{"web_search_call.action.sources"},
	})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var resp Response
	json.Unmarshal(body, &resp)
	if len(resp.Output) != 2 || resp.Output[0].Type != "web_search_call" || resp.Output[0].Action.Query != "go release" || len(resp.Output[0].Action.Sources) != 1 {
		t.Fatalf("output = %s", body)
	}
	anns := (*resp.Output[1].Content)[0].Annotations
	if len(anns) != 1 || anns[0].URL != "https://go.dev/doc/go1.27" || anns[0].StartIndex != 4 {
		t.Errorf("annotations = %+v", anns)
	}
}

func TestMessagesAPI(t *testing.T) {
	_, ts := newTestServer(t, nil)
	tool := obj{"name": "get_weather", "description": "Weather", "input_schema": obj{"type": "object"}}
	status, body := post(t, ts, "/v1/messages", "", obj{
		"model": "claude/sonnet", "max_tokens": 100, "system": "Be brief.",
		"messages": []obj{{"role": "user", "content": []obj{{"type": "text", "text": "CALL_TOOL"}}}},
		"tools":    []obj{tool},
	})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var resp MessagesResponse
	json.Unmarshal(body, &resp)
	if *resp.StopReason != "tool_use" || len(resp.Content) != 1 || resp.Content[0].Name != "get_weather" || string(resp.Content[0].Input) != `{"city":"Rome"}` {
		t.Fatalf("response = %s", body)
	}

	// The call and its result come back as a conversation.
	status, body = post(t, ts, "/v1/messages", "", obj{
		"model": "claude", "max_tokens": 100, "stream": true,
		"messages": []obj{
			{"role": "user", "content": "weather?"},
			{"role": "assistant", "content": []obj{{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": obj{"city": "Rome"}}}},
			{"role": "user", "content": []obj{{"type": "tool_result", "tool_use_id": "call_1", "content": "sunny"}, {"type": "text", "text": "ECHO"}}},
		},
		"tools": []obj{tool},
	})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var types []string
	var text string
	for _, e := range sseEvents(body) {
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		json.Unmarshal([]byte(e), &ev)
		types = append(types, ev.Type)
		text += ev.Delta.Text
	}
	if got := strings.Join(types, " "); got != "message_start content_block_start content_block_delta content_block_stop message_delta message_stop" {
		t.Errorf("events = %s", got)
	}
	for _, want := range []string{"called the function get_weather", `<tool_result tool_call_id="call_1" name="get_weather">` + "\nsunny"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt lacks %q:\n%s", want, text)
		}
	}

	status, body = post(t, ts, "/v1/messages", "", obj{"model": "nope", "messages": user("hi")})
	if status != 400 || !strings.Contains(string(body), `"type":"error"`) {
		t.Errorf("error shape: %d %s", status, body)
	}
}

func TestMCP(t *testing.T) {
	srv, ts := newTestServer(t, nil)
	url, unpublish := srv.PublishFunctions([]FunctionDefinition{{Name: "f", Description: "d"}})
	path := strings.TrimPrefix(url, srv.cfg.SelfURL)

	_, body := post(t, ts, path, "", obj{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": obj{"protocolVersion": "2025-03-26"}})
	if !strings.Contains(string(body), `"protocolVersion":"2025-03-26"`) {
		t.Errorf("initialize: %s", body)
	}
	if status, _ := post(t, ts, path, "", obj{"jsonrpc": "2.0", "method": "notifications/initialized"}); status != 202 {
		t.Errorf("notification: status %d", status)
	}
	_, body = post(t, ts, path, "", obj{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if !strings.Contains(string(body), `"name":"f"`) || !strings.Contains(string(body), `"inputSchema":{"type":"object","properties":{}}`) {
		t.Errorf("tools/list: %s", body)
	}
	_, body = post(t, ts, path, "", obj{"jsonrpc": "2.0", "id": 3, "method": "nope"})
	if !strings.Contains(string(body), "-32601") {
		t.Errorf("unknown method: %s", body)
	}
	unpublish()
	if status, _ := post(t, ts, path, "", obj{"jsonrpc": "2.0", "id": 4, "method": "tools/list"}); status != 404 {
		t.Errorf("after unpublish: status %d", status)
	}
}

func TestWarmPool(t *testing.T) {
	srv, ts := newTestServer(t, func(c *Config) { c.Warm = 1 })
	claude := srv.backends["claude"].(*ClaudeBackend)
	waitIdle := func() {
		t.Helper()
		for i := 0; i < 100; i++ {
			if idle, _, _ := claude.pool.stats(); idle == 1 {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("no warm process")
	}
	req := obj{"model": "claude", "messages": user("hi")}
	if status, body := post(t, ts, "/v1/chat/completions", "", req); status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	waitIdle()
	if status, body := post(t, ts, "/v1/chat/completions", "", req); status != 200 || *chatOf(t, body).Choices[0].Message.Content != "Hello, world" {
		t.Fatalf("status %d: %s", status, body)
	}
	if _, hits, _ := claude.pool.stats(); hits != 1 {
		t.Errorf("warm hits = %d", hits)
	}
	// A different prompt configuration does not take the waiting process.
	waitIdle()
	post(t, ts, "/v1/chat/completions", "", obj{"model": "claude/opus", "messages": user("hi")})
	if _, hits, _ := claude.pool.stats(); hits != 1 {
		t.Errorf("warm hits after another model = %d", hits)
	}
}

func TestQueueFullAndTimeout(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) {
		c.MaxConcurrency, c.MaxQueue, c.RequestTimeout = 1, 0, 2
	})
	done := make(chan int)
	go func() {
		status, _ := post(t, ts, "/v1/chat/completions", "", obj{"model": "claude", "messages": user("SLOW")})
		done <- status
	}()
	time.Sleep(500 * time.Millisecond)
	if status, body := post(t, ts, "/v1/chat/completions", "", obj{"model": "claude", "messages": user("hi")}); status != 503 {
		t.Errorf("queue full: status %d: %s", status, body)
	}
	if status := <-done; status != 504 {
		t.Errorf("timeout: status %d", status)
	}
}

func TestUpstream(t *testing.T) {
	var got obj
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer up-key" {
			http.Error(w, `{"error":{"message":"bad key"}}`, 401)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"choices":[{"delta":{"role":"assistant","content":"Hi "}}]}`,
			`{"choices":[{"delta":{"content":"there"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Rome\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`,
			`[DONE]`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
	defer upstream.Close()

	_, ts := newTestServer(t, func(c *Config) {
		c.Upstreams = []UpstreamConfig{{Name: "local", BaseURL: upstream.URL + "/v1/", APIKey: "up-key", Models: []string{"llama"}}}
	})
	status, body := post(t, ts, "/v1/chat/completions", "", obj{
		"model": "local/llama", "messages": user("hi"), "tools": []obj{weatherTool},
	})
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	resp := chatOf(t, body)
	msg := resp.Choices[0].Message
	if *msg.Content != "Hi there" || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_9" || msg.ToolCalls[0].Function.Arguments != `{"city":"Rome"}` {
		t.Errorf("message = %s", body)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 10 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if got["model"] != "llama" || got["tools"] == nil || len(got["messages"].([]any)) != 1 {
		t.Errorf("upstream request = %v", got)
	}
}

func TestHealthAndMetrics(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) { c.APIKey = "k" })
	for _, path := range []string{"/health", "/metrics"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: status %d", path, resp.StatusCode)
		}
		want := `"status":"ok"`
		if path == "/metrics" {
			want = "llm_gateway_inflight_requests 0"
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("%s lacks %q: %s", path, want, body)
		}
	}
}
