package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests cover what a client relies on to tell a whole answer from a
// cut or failed one: the output budget, the finish reason and the errors,
// through the HTTP endpoints and the real adapters, against the fake CLIs
// and a fake upstream.

// fakeUpstream is an OpenAI-compatible API whose behaviour depends on a
// keyword in the last message.
type fakeUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []obj // the requests received
}

func (u *fakeUpstream) last(t *testing.T) obj {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		t.Fatal("the upstream was not called")
	}
	return u.bodies[len(u.bodies)-1]
}

func (u *fakeUpstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	u := &fakeUpstream{}
	u.Server = testHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body obj
		json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.bodies = append(u.bodies, body)
		u.mu.Unlock()
		prompt := string(raw)
		fail := func(status int, payload string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, payload)
		}
		stream := func(chunks ...string) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
			}
			w.(http.Flusher).Flush()
		}
		text := `{"choices":[{"delta":{"content":"Half an"}}]}`
		usage := `{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`
		switch {
		case strings.Contains(prompt, "UP_RATE"):
			w.Header().Set("Retry-After", "17")
			fail(429, `{"error":{"message":"Rate limit reached for key `+fakeSecret+`","type":"requests","code":"rate_limit_exceeded"}}`)
		case strings.Contains(prompt, "UP_QUOTA"):
			fail(429, `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","code":"insufficient_quota"}}`)
		case strings.Contains(prompt, "UP_AUTH"):
			fail(401, `{"error":{"message":"Incorrect API key provided: `+fakeSecret+`","code":"invalid_api_key"}}`)
		case strings.Contains(prompt, "UP_BIG"):
			fail(400, `{"error":{"message":"This model's maximum context length is 8192 tokens.","code":"context_length_exceeded"}}`)
		case strings.Contains(prompt, "UP_BAD"):
			fail(400, `{"error":{"message":"Unsupported parameter","code":400}}`)
		case strings.Contains(prompt, "UP_DOWN"):
			fail(500, `oops`)
		case strings.Contains(prompt, "UP_SLOW"):
			<-r.Context().Done()
		case strings.Contains(prompt, "UP_LENGTH"):
			stream(text, `{"choices":[{"delta":{},"finish_reason":"length"}]}`, usage, `[DONE]`)
		case strings.Contains(prompt, "UP_FILTER"):
			stream(text, `{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`, usage, `[DONE]`)
		case strings.Contains(prompt, "UP_WEIRD"):
			stream(text, `{"choices":[{"delta":{},"finish_reason":"gave_up"}]}`, `[DONE]`)
		case strings.Contains(prompt, "UP_CUT"):
			stream(text) // and the connection ends, cleanly
		case strings.Contains(prompt, "UP_DONE_ONLY"):
			stream(text, `[DONE]`) // no finish reason at all
		case strings.Contains(prompt, "UP_ABORT"):
			stream(text)
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		case strings.Contains(prompt, "UP_MIDERR"):
			stream(text, `{"error":{"message":"Rate limit reached, try again in 3s","code":"rate_limit_exceeded"}}`)
		default:
			stream(`{"choices":[{"delta":{"content":"Hi there"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, usage, `[DONE]`)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

// contractServer is a gateway with the fake CLIs and a fake upstream "up".
func contractServer(t *testing.T, edit func(*Config)) (*Server, *httptest.Server, *fakeUpstream) {
	up := newFakeUpstream(t)
	srv, ts := newTestServer(t, func(c *Config) {
		c.Upstreams = []UpstreamConfig{{Name: "up", BaseURL: up.URL + "/v1", Models: []string{"m"}}}
		if edit != nil {
			edit(c)
		}
	})
	return srv, ts, up
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

func (r reply) String() string { return fmt.Sprintf("%d %s", r.status, r.body) }

// code is the machine code of an error, in either error shape.
func (r reply) code() string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(r.body, &e)
	return e.Error.Code
}

func ask(t *testing.T, ts *httptest.Server, path string, body obj) reply {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header, out}
}

// The three APIs, asking the same thing: how each names the budget, and how
// to read the text and the finish of its reply.
var apis = []struct {
	name, path, budget string
	request            func(model, prompt string) obj
	// read returns the text and the way the answer ended, as the API puts it.
	read func(body []byte) (text, finish string)
	// readStream does the same for an event stream; failed is the code of
	// an error event, ended whether the stream closed as a whole answer.
	readStream func(body []byte) (text, finish, failed string, ended bool)
}{
	{
		name: "chat", path: "/v1/chat/completions", budget: "max_completion_tokens",
		request: func(model, prompt string) obj { return obj{"model": model, "messages": user(prompt)} },
		read: func(body []byte) (string, string) {
			var r ChatCompletionResponse
			json.Unmarshal(body, &r)
			if len(r.Choices) != 1 || r.Choices[0].Message.Content == nil {
				return "", ""
			}
			return *r.Choices[0].Message.Content, r.Choices[0].FinishReason
		},
		readStream: func(body []byte) (text, finish, failed string, ended bool) {
			for _, e := range sseEvents(body) {
				if e == "[DONE]" {
					ended = true
					continue
				}
				var c struct {
					ChatCompletionChunk
					Error *APIErrorBody `json:"error"`
				}
				json.Unmarshal([]byte(e), &c)
				if c.Error != nil {
					failed = c.Error.Code
				}
				for _, ch := range c.Choices {
					text += ch.Delta.Content
					if ch.FinishReason != nil {
						finish = *ch.FinishReason
					}
				}
			}
			return
		},
	},
	{
		name: "responses", path: "/v1/responses", budget: "max_output_tokens",
		request: func(model, prompt string) obj { return obj{"model": model, "input": prompt} },
		read:    func(body []byte) (string, string) { return readResponse(body) },
		readStream: func(body []byte) (text, finish, failed string, ended bool) {
			for _, e := range sseEvents(body) {
				var ev struct {
					Type     string          `json:"type"`
					Delta    string          `json:"delta"`
					Response json.RawMessage `json:"response"`
				}
				json.Unmarshal([]byte(e), &ev)
				switch ev.Type {
				case "response.output_text.delta":
					text += ev.Delta
				case "response.completed", "response.incomplete":
					_, finish = readResponse(ev.Response)
					ended = true
				case "response.failed":
					var r Response
					json.Unmarshal(ev.Response, &r)
					failed = r.Error.Code
				}
			}
			return
		},
	},
	{
		name: "messages", path: "/v1/messages", budget: "max_tokens",
		request: func(model, prompt string) obj { return obj{"model": model, "messages": user(prompt)} },
		read: func(body []byte) (string, string) {
			var r MessagesResponse
			json.Unmarshal(body, &r)
			if len(r.Content) == 0 || r.StopReason == nil {
				return "", ""
			}
			return r.Content[0].Text, *r.StopReason
		},
		readStream: func(body []byte) (text, finish, failed string, ended bool) {
			for _, e := range sseEvents(body) {
				var ev struct {
					Type  string `json:"type"`
					Delta struct {
						Text       string `json:"text"`
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				json.Unmarshal([]byte(e), &ev)
				text += ev.Delta.Text
				switch ev.Type {
				case "message_delta":
					finish = ev.Delta.StopReason
				case "message_stop":
					ended = true
				case "error":
					failed = ev.Error.Code
				}
			}
			return
		},
	},
}

// readResponse reads a Responses object: its status, with the reason when
// it is incomplete.
func readResponse(body []byte) (text, finish string) {
	var r Response
	json.Unmarshal(body, &r)
	for _, item := range r.Output {
		if item.Content != nil && len(*item.Content) > 0 {
			text += (*item.Content)[0].Text
			if item.Status != r.Status {
				finish = "item " + item.Status + " in a response "
			}
		}
	}
	finish += r.Status
	if r.IncompleteDetails != nil {
		finish += ":" + r.IncompleteDetails.Reason
	}
	return text, finish
}

func TestBudgetReachesTheBackends(t *testing.T) {
	_, ts, up := contractServer(t, func(c *Config) {
		c.Upstreams = append(c.Upstreams, UpstreamConfig{Name: "strict", BaseURL: c.Upstreams[0].BaseURL, MaxTokensField: "max_completion_tokens"})
	})
	for _, api := range apis {
		// claude: the budget is the CLI's environment variable.
		req := api.request("claude", "ECHO")
		req[api.budget] = 321
		r := ask(t, ts, api.path, req)
		if text, finish := api.read(r.body); r.status != 200 || !strings.Contains(text, `budget="321"`) || finish == "" {
			t.Errorf("%s, claude with a budget: %s", api.name, r)
		}
		r = ask(t, ts, api.path, api.request("claude", "ECHO"))
		if text, _ := api.read(r.body); r.status != 200 || !strings.Contains(text, `budget=""`) {
			t.Errorf("%s, claude without a budget: %s", api.name, r)
		}

		// upstream: forwarded under the client's name on chat, max_tokens
		// otherwise, or the name the configuration fixes.
		want := "max_tokens"
		if api.name == "chat" {
			want = api.budget
		}
		if r := ask(t, ts, api.path, req2(req, "up/m")); r.status != 200 {
			t.Fatalf("%s, upstream: %s", api.name, r)
		}
		if got := up.last(t); got[want] != float64(321) || len(budgetFields(got)) != 1 {
			t.Errorf("%s: upstream got %v, want %s=321", api.name, budgetFields(got), want)
		}
		ask(t, ts, api.path, req2(req, "strict/m"))
		if got := up.last(t); got["max_completion_tokens"] != float64(321) || len(budgetFields(got)) != 1 {
			t.Errorf("%s: strict upstream got %v", api.name, budgetFields(got))
		}
		ask(t, ts, api.path, api.request("up/m", "hi"))
		if got := budgetFields(up.last(t)); len(got) != 0 {
			t.Errorf("%s: a budget was invented: %v", api.name, got)
		}

		// Not a positive integer: refused before any backend runs.
		for _, bad := range []any{0, -5, 1.5, "100", true} {
			req := api.request("claude", "hi")
			req[api.budget] = bad
			if r := ask(t, ts, api.path, req); r.status != 400 || r.code() != "invalid_request_error" {
				t.Errorf("%s with %s=%v: %s", api.name, api.budget, bad, r)
			}
		}
		// null is no budget.
		req = api.request("claude", "ECHO")
		req[api.budget] = nil
		if r := ask(t, ts, api.path, req); r.status != 200 {
			t.Errorf("%s with a null budget: %s", api.name, r)
		}
	}

	// Both names of Chat Completions: fine when they agree.
	both := obj{"model": "up/m", "messages": user("hi"), "max_tokens": 50, "max_completion_tokens": 50}
	if r := ask(t, ts, "/v1/chat/completions", both); r.status != 200 {
		t.Errorf("agreeing budgets: %s", r)
	}
	if got := up.last(t); got["max_completion_tokens"] != float64(50) || len(budgetFields(got)) != 1 {
		t.Errorf("agreeing budgets: upstream got %v", budgetFields(got))
	}
	both["max_tokens"] = 60
	if r := ask(t, ts, "/v1/chat/completions", both); r.status != 400 || !strings.Contains(string(r.body), "differ") {
		t.Errorf("conflicting budgets: %s", r)
	}
	old := obj{"model": "up/m", "messages": user("hi"), "max_tokens": 70}
	ask(t, ts, "/v1/chat/completions", old)
	if got := up.last(t); got["max_tokens"] != float64(70) || len(budgetFields(got)) != 1 {
		t.Errorf("max_tokens: upstream got %v", budgetFields(got))
	}
}

func req2(req obj, model string) obj {
	out := obj{}
	for k, v := range req {
		out[k] = v
	}
	out["model"] = model
	return out
}

func budgetFields(body obj) obj {
	out := obj{}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	return out
}

// Codex cannot be given a budget: the answer is checked against it once the
// usage is known, and refused whole when over. The fake reports 3 tokens.
func TestCodexBudgetIsCheckedAfterwards(t *testing.T) {
	srv, ts, up := contractServer(t, func(c *Config) {
		c.Models = map[string][]string{"auto": {"codex", "up/m"}}
	})
	for _, api := range apis {
		for _, stream := range []bool{false, true} {
			req := api.request("codex", "hi")
			req["stream"] = stream
			req[api.budget] = 2
			r := ask(t, ts, api.path, req)
			if r.status != 422 || r.code() != "output_limit_exceeded" || strings.Contains(string(r.body), "codex says hi") {
				t.Errorf("%s stream=%v, over the budget: %s", api.name, stream, r)
			}
			req[api.budget] = 3
			r = ask(t, ts, api.path, req)
			text, _ := api.read(r.body)
			if stream {
				text, _, _, _ = api.readStream(r.body)
			}
			if r.status != 200 || text != "codex says hi" {
				t.Errorf("%s stream=%v, within the budget: %s", api.name, stream, r)
			}
		}
	}
	// The tokens of the refused answers are still counted.
	var metrics bytes.Buffer
	srv.metrics.Render(&metrics)
	if want := `llm_gateway_tokens_total{backend="codex",kind="output"} 36`; !strings.Contains(metrics.String(), want) {
		t.Errorf("metrics lack %q:\n%s", want, metrics.String())
	}
	// An answer over the budget is not asked again of the next target.
	r := ask(t, ts, "/v1/chat/completions", obj{"model": "auto", "messages": user("hi"), "max_tokens": 2})
	if r.status != 422 || up.calls() != 0 {
		t.Errorf("over the budget with a fallback: %s, upstream calls %d", r, up.calls())
	}
}

func TestFinishReasons(t *testing.T) {
	_, ts, _ := contractServer(t, nil)
	cases := []struct {
		model, prompt, text string
		finish              [3]string // chat, responses, messages
	}{
		{"up/m", "hi", "Hi there", [3]string{"stop", "completed", "end_turn"}},
		{"claude", "hi", "Hello, world", [3]string{"stop", "completed", "end_turn"}},
		{"codex", "hi", "codex says hi", [3]string{"stop", "completed", "end_turn"}},
		{"up/m", "UP_LENGTH", "Half an", [3]string{"length", "incomplete:max_output_tokens", "max_tokens"}},
		{"up/m", "UP_FILTER", "Half an", [3]string{"content_filter", "incomplete:content_filter", "refusal"}},
		{"claude", "TRUNCATE", "The first half", [3]string{"length", "incomplete:max_output_tokens", "max_tokens"}},
		{"claude", "REFUSE", "I can", [3]string{"content_filter", "incomplete:content_filter", "refusal"}},
		// The stop reasons of claude that end a turn for good.
		{"claude", "STOP_AS:end_turn", "Half an", [3]string{"stop", "completed", "end_turn"}},
		{"claude", "STOP_AS:stop_sequence", "Half an", [3]string{"stop", "completed", "end_turn"}},
		{"claude", "STOP_AS:result=end_turn", "Half an", [3]string{"stop", "completed", "end_turn"}},
		{"claude", "STOP_AS:refusal", "Half an", [3]string{"content_filter", "incomplete:content_filter", "refusal"}},
		{"claude", "STOP_AS:result=max_tokens", "Half an", [3]string{"length", "incomplete:max_output_tokens", "max_tokens"}},
		{"claude", "STOP_AS:model_context_window_exceeded", "Half an", [3]string{"length", "incomplete:max_output_tokens", "max_tokens"}},
	}
	for _, c := range cases {
		for i, api := range apis {
			name := fmt.Sprintf("%s %s %s", api.name, c.model, c.prompt)
			req := api.request(c.model, c.prompt)
			start := time.Now()
			r := ask(t, ts, api.path, req)
			if text, finish := api.read(r.body); r.status != 200 || text != c.text || finish != c.finish[i] {
				t.Errorf("%s: text %q finish %q, want %q %q: %s", name, text, finish, c.text, c.finish[i], r)
			}
			req["stream"] = true
			r = ask(t, ts, api.path, req)
			text, finish, failed, ended := api.readStream(r.body)
			if r.status != 200 || text != c.text || finish != c.finish[i] || failed != "" || !ended {
				t.Errorf("%s, stream: text %q finish %q failed %q ended %v, want %q %q: %s", name, text, finish, failed, ended, c.text, c.finish[i], r.body)
			}
			if time.Since(start) > 10*time.Second {
				t.Errorf("%s: the CLI was not stopped at the cut", name)
			}
		}
	}
}

// An answer that stops before its end is never returned as a whole one:
// without streaming it is an error, in a stream the text already sent is
// followed by an error, not by a finish.
func TestInterruptedAnswers(t *testing.T) {
	_, ts, _ := contractServer(t, nil)
	cases := []struct{ model, prompt, code string }{
		{"up/m", "UP_CUT", "backend_incomplete_response"},
		{"up/m", "UP_ABORT", "backend_incomplete_response"},
		{"up/m", "UP_DONE_ONLY", "backend_incomplete_response"},
		{"up/m", "UP_WEIRD", "backend_incomplete_response"}, // an unknown finish reason
		{"up/m", "UP_MIDERR", "backend_rate_limit"},
		{"claude", "DIE_MIDWAY", "backend_incomplete_response"},
		{"codex", "DIE_MIDWAY", "backend_incomplete_response"},
		// A result the CLI calls a success does not make these whole: a
		// paused turn nobody resumed, a tool use left there, a reason
		// unknown here, no reason at all.
		{"claude", "STOP_AS:pause_turn", "backend_incomplete_response"},
		{"claude", "STOP_AS:result=pause_turn", "backend_incomplete_response"},
		{"claude", "STOP_AS:tool_use", "backend_incomplete_response"},
		{"claude", "STOP_AS:compaction", "backend_incomplete_response"},
		{"claude", "STOP_AS:something_new", "backend_incomplete_response"},
		{"claude", "STOP_AS:none", "backend_incomplete_response"},
	}
	for _, c := range cases {
		for _, api := range apis {
			name := fmt.Sprintf("%s %s %s", api.name, c.model, c.prompt)
			req := api.request(c.model, c.prompt)
			r := ask(t, ts, api.path, req)
			wantStatus := 502
			if c.code == "backend_rate_limit" {
				wantStatus = 429
			}
			if r.status != wantStatus || r.code() != c.code || strings.Contains(string(r.body), "Half an") {
				t.Errorf("%s: %s", name, r)
			}
			if c.prompt == "UP_MIDERR" && (r.header.Get("Retry-After") != "3" || !strings.Contains(string(r.body), `"retry_after":3`)) {
				t.Errorf("%s: Retry-After %q: %s", name, r.header.Get("Retry-After"), r)
			}

			req["stream"] = true
			r = ask(t, ts, api.path, req)
			text, finish, failed, ended := api.readStream(r.body)
			if r.status != 200 || text != "Half an" || failed != c.code || finish != "" || ended {
				t.Errorf("%s, stream: text %q finish %q failed %q ended %v: %s", name, text, finish, failed, ended, r.body)
			}
			// A rate limit met after the stream began: the status is
			// already 200 and there is no header left, so the last event
			// carries the code and the wait, and nothing says "finished".
			if c.prompt == "UP_MIDERR" {
				events := sseEvents(r.body)
				last := events[len(events)-1]
				for _, want := range []string{`"code":"backend_rate_limit"`, `"retry_after":3`} {
					if !strings.Contains(last, want) {
						t.Errorf("%s, stream: the last event lacks %s: %s", name, want, last)
					}
				}
				for _, lie := range []string{"[DONE]", "finish_reason", `"completed"`, "message_stop", "stop_reason"} {
					if strings.Contains(last, lie) || r.header.Get("Retry-After") != "" {
						t.Errorf("%s, stream: the last event claims an end (%s): %s", name, lie, last)
					}
				}
			}
		}
	}
}

func TestBackendErrors(t *testing.T) {
	// The log is checked too: it names the failure without repeating it.
	var logged bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&syncWriter{w: &logged}, nil)))
	defer func() {
		slog.SetDefault(old)
		for _, leak := range []string{fakeSecret, "SECRET_FAIL", "UP_RATE", "detail="} {
			if strings.Contains(logged.String(), leak) {
				t.Errorf("the log leaks %q", leak)
			}
		}
		for _, want := range []string{"code=backend_authentication_error", "code=backend_rate_limit", "target=up/m"} {
			if !strings.Contains(logged.String(), want) {
				t.Errorf("the log lacks %q", want)
			}
		}
	}()
	_, ts, _ := contractServer(t, nil)
	cases := []struct {
		model, prompt string
		status        int
		code          string
		retry         int // Retry-After in seconds, 0 for none
	}{
		{"up/m", "UP_RATE", 429, "backend_rate_limit", 17},
		{"up/m", "UP_QUOTA", 429, "backend_quota_exceeded", 0},
		{"up/m", "UP_AUTH", 502, "backend_authentication_error", 0},
		{"up/m", "UP_BIG", 413, "context_length_exceeded", 0},
		{"up/m", "UP_BAD", 400, "backend_invalid_request", 0},
		{"up/m", "UP_DOWN", 502, "backend_error", 0},
		{"claude", "FAIL_CLAUDE", 502, "backend_authentication_error", 0},
		{"claude", "SECRET_FAIL", 502, "backend_authentication_error", 0},
		{"claude", "USAGE_LIMIT", 429, "backend_quota_exceeded", 3600},
		{"claude", "TOO_LONG", 413, "context_length_exceeded", 0},
		{"claude", "LIMIT_RESULT", 422, "output_limit_exceeded", 0},
		{"codex", "FAIL_CODEX", 503, "backend_overloaded", 0},
		{"codex", "RATE_CODEX", 429, "backend_rate_limit", 20},
	}
	for _, c := range cases {
		for _, api := range apis {
			// A request that fails before any output gets the same status
			// and headers whether it asked for a stream or not.
			for _, stream := range []bool{false, true} {
				name := fmt.Sprintf("%s %s %s stream=%v", api.name, c.model, c.prompt, stream)
				req := api.request(c.model, c.prompt)
				req["stream"] = stream
				r := ask(t, ts, api.path, req)
				if r.status != c.status || r.code() != c.code {
					t.Errorf("%s: want %d %s, got %s", name, c.status, c.code, r)
				}
				retry, _ := strconv.Atoi(r.header.Get("Retry-After"))
				if retry > c.retry || retry < c.retry-2 {
					t.Errorf("%s: Retry-After %q, want %d", name, r.header.Get("Retry-After"), c.retry)
				}
				// Nothing of what the backend said gets out.
				for _, leak := range []string{fakeSecret, "SECRET_FAIL", "boom", "billing details"} {
					if strings.Contains(string(r.body), leak) {
						t.Errorf("%s: the error leaks %q: %s", name, leak, r.body)
					}
				}
			}
		}
	}
	// The Messages API keeps its own error shape and types.
	r := ask(t, ts, "/v1/messages", obj{"model": "up/m", "messages": user("UP_RATE")})
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	json.Unmarshal(r.body, &e)
	if e.Type != "error" || e.Error.Type != "rate_limit_error" {
		t.Errorf("messages error shape: %s", r)
	}
	if r := ask(t, ts, "/v1/messages", obj{"model": "up/m", "messages": user("UP_AUTH")}); !strings.Contains(string(r.body), `"type":"api_error"`) {
		t.Errorf("messages backend error type: %s", r)
	}
}

func TestGatewayErrors(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) {
		c.APIKeys = []KeyConfig{{Name: "limited", Key: "limited-key", RPM: 1}, {Name: "codex-only", Key: "codex-key", Backends: []string{"codex"}}}
	})
	send := func(key string) reply {
		b, _ := json.Marshal(obj{"model": "claude", "messages": user("hi")})
		req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return reply{resp.StatusCode, resp.Header, out}
	}
	// The client's own key is a 401; a backend's login never is.
	if r := send("wrong"); r.status != 401 || r.code() != "authentication_error" {
		t.Errorf("wrong key: %s", r)
	}
	if r := send("codex-key"); r.status != 403 || r.code() != "permission_denied" {
		t.Errorf("restricted key: %s", r)
	}
	send("limited-key")
	if r := send("limited-key"); r.status != 429 || r.code() != "gateway_rate_limit" || r.header.Get("Retry-After") == "" {
		t.Errorf("gateway rate limit: %s %v", r, r.header)
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	srv, ts, _ := contractServer(t, func(c *Config) {
		c.MaxConcurrency, c.MaxQueue, c.RequestTimeout = 1, 0, 2
	})
	idle := func(what string) {
		t.Helper()
		for i := 0; srv.inflight.Load() != 0; i++ {
			if i == 250 {
				t.Fatalf("%s: the slot was not released", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	for _, api := range apis {
		for _, model := range []string{"claude", "up/m"} {
			done := make(chan reply)
			go func() { done <- ask(t, ts, api.path, api.request(model, "SLOW UP_SLOW")) }()
			time.Sleep(300 * time.Millisecond)
			// The slot is taken: another request is told to come back.
			if r := ask(t, ts, api.path, api.request("claude", "hi")); r.status != 503 || r.code() != "gateway_busy" || r.header.Get("Retry-After") != "1" {
				t.Errorf("%s, busy: %s", api.name, r)
			}
			if r := <-done; r.status != 504 || r.code() != "backend_timeout" {
				t.Errorf("%s %s, timeout: %s", api.name, model, r)
			}
			idle("timeout")
		}
	}

	// A client that goes away stops the backend and frees the slot.
	for _, model := range []string{"claude", "up/m"} {
		ctx, cancel := context.WithCancel(context.Background())
		b, _ := json.Marshal(obj{"model": model, "messages": user("SLOW UP_SLOW"), "stream": true})
		req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/chat/completions", bytes.NewReader(b))
		errc := make(chan error)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
			errc <- err
		}()
		time.Sleep(300 * time.Millisecond)
		if srv.inflight.Load() != 1 {
			t.Fatalf("%s: the request is not running", model)
		}
		start := time.Now()
		cancel()
		<-errc
		idle("cancellation")
		if time.Since(start) > time.Second {
			t.Errorf("%s: the backend was stopped by the timeout, not by the cancellation", model)
		}
	}
	var metrics bytes.Buffer
	srv.metrics.Render(&metrics)
	for _, want := range []string{`backend="claude",status="canceled"} 1`, `backend="up",status="canceled"} 1`, `endpoint="responses",backend="up",status="timeout"} 1`, `endpoint="messages",backend="claude",status="timeout"} 1`} {
		if !strings.Contains(metrics.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, metrics.String())
		}
	}
}

// Another target is tried only while the first has sent nothing: an answer
// is never made of two backends' output.
func TestFallbackKeepsAnswersWhole(t *testing.T) {
	_, ts, up := contractServer(t, func(c *Config) {
		c.Models = map[string][]string{"auto": {"up/m", "codex"}}
	})
	for _, api := range apis {
		// Failed before any output: the next target answers.
		for _, prompt := range []string{"UP_RATE", "UP_DOWN"} {
			r := ask(t, ts, api.path, api.request("auto", prompt))
			if text, _ := api.read(r.body); r.status != 200 || text != "codex says hi" {
				t.Errorf("%s %s: no fallback: %s", api.name, prompt, r)
			}
		}
		if r := ask(t, ts, api.path, api.request("auto", "UP_AUTH")); r.status != 502 || r.code() != "backend_authentication_error" {
			t.Fatalf("credential error replayed: %s", r)
		}
		// Failed after some output: the failure is the answer.
		before := up.calls()
		r := ask(t, ts, api.path, api.request("auto", "UP_CUT"))
		if r.status != 502 || r.code() != "backend_incomplete_response" || strings.Contains(string(r.body), "codex") {
			t.Errorf("%s: after partial output: %s", api.name, r)
		}
		req := api.request("auto", "UP_CUT")
		req["stream"] = true
		r = ask(t, ts, api.path, req)
		text, finish, failed, ended := api.readStream(r.body)
		if text != "Half an" || failed != "backend_incomplete_response" || finish != "" || ended {
			t.Errorf("%s, stream after partial output: text %q finish %q failed %q ended %v", api.name, text, finish, failed, ended)
		}
		if up.calls() != before+2 {
			t.Errorf("%s: the failed target was asked %d times, want 2", api.name, up.calls()-before)
		}
	}
}

func TestClassifyAndRedact(t *testing.T) {
	kinds := map[string]failKind{
		"boom: not logged in":                                             failAuth,
		"Invalid API key · Please run /login":                             failAuth,
		"API Error: 401 unauthorized":                                     failAuth,
		"Claude usage limit reached":                                      failQuota,
		"You've hit your usage limit. Try again in 3 hours 2 minutes.":    failQuota,
		"Credit balance is too low":                                       failQuota,
		"Rate limit reached for gpt-5. Please try again in 1.898s.":       failRateLimit,
		"429 Too Many Requests":                                           failRateLimit,
		"Prompt is too long":                                              failTooLarge,
		"Your input exceeds the context window of this model":             failTooLarge,
		"Claude's response exceeded the 2200 output token maximum.":       failOutputLimit,
		"model overloaded":                                                failOverloaded,
		"stream disconnected: request timed out":                          failTimeout,
		"something else entirely":                                         failGeneric,
		"error_during_execution":                                          failGeneric,
		"error_max_turns":                                                 failGeneric,
		"exit status 1":                                                   failGeneric,
		"unexpected status 500 Internal Server Error: upstream went away": failGeneric,
	}
	for text, want := range kinds {
		if got := classify(text); got != want {
			t.Errorf("classify(%q) = %s, want %s", text, failures[got].code, failures[want].code)
		}
	}
	waits := map[string]time.Duration{
		"Please try again in 1.898s.":                  1898 * time.Millisecond,
		"Try again in 3 hours 2 minutes.":              3*time.Hour + 2*time.Minute,
		"try again in 1 day, 4 hours and 30 minutes":   28*time.Hour + 30*time.Minute,
		"retry after 30":                               30 * time.Second,
		"Retry-After: 12":                              12 * time.Second,
		"try again in 250ms":                           250 * time.Millisecond,
		"try again later":                              0,
		"nothing about waiting, 500 words, 3 hours in": 0,
	}
	for text, want := range waits {
		if got := retryIn(text); got != want {
			t.Errorf("retryIn(%q) = %s, want %s", text, got, want)
		}
	}
	if got := retryAfterHeader(time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)); got < 85*time.Second || got > 90*time.Second {
		t.Errorf("Retry-After as a date = %s", got)
	}

	dummyValue := strings.Repeat("abc123", 2)
	raw := "Authorization: Bearer " + fakeSecret + "\n  api_key=" + dummyValue + " \"token\": \"tok_live_1\" key " + fakeSecret + " " + strings.Repeat("the prompt ", 100)
	got := redact(raw)
	for _, leak := range []string{fakeSecret, dummyValue, "tok_live_1", "\n"} {
		if strings.Contains(got, leak) {
			t.Errorf("redact leaves %q in %q", leak, got)
		}
	}
	if len([]rune(got)) > maxDetail+1 {
		t.Errorf("redact leaves %d characters", len([]rune(got)))
	}
	// Debug logs omit provider error text and credentials.
	var logged bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	slog.Error("failed", logFailure(textFailure("claude", raw))...)
	slog.SetDefault(old)
	if strings.Contains(logged.String(), "detail=") || strings.Contains(logged.String(), fakeSecret) {
		t.Errorf("debug log = %s", logged.String())
	}
	// The public text of an error never comes from the backend.
	if msg := publicError(textFailure("claude", raw)).msg; strings.Contains(msg, "prompt") || strings.Contains(msg, "Bearer") {
		t.Errorf("public message = %q", msg)
	}
}

// A turn of claude that ends on a tool use is whole only when the tool is
// the one that hands over the structured output of the result.
func TestClaudeStructuredOutputEndsOnAToolUse(t *testing.T) {
	_, ts, _ := contractServer(t, nil)
	r := ask(t, ts, "/v1/chat/completions", obj{
		"model": "claude", "messages": user("STOP_AS:tool_use"),
		"response_format": obj{"type": "json_schema", "json_schema": obj{"name": "r", "schema": obj{"type": "object"}}},
	})
	if text, finish := apis[0].read(r.body); r.status != 200 || text != `{"ok":true}` || finish != "stop" {
		t.Errorf("structured output: %s", r)
	}
}

// The wait for the first event of a stream is bounded as a whole, and ends
// with the request, also while the event after a usage is awaited.
func TestEarlyIsBoundedAndCancellable(t *testing.T) {
	old := streamGrace
	defer func() { streamGrace = old }()
	usage := Event{Usage: &Usage{CompletionTokens: 9}}
	run := func(grace time.Duration, ctx context.Context, feed func(chan<- Event)) (first []Event, err error, a answer, took time.Duration) {
		streamGrace = grace
		events := make(chan Event)
		go feed(events)
		start := time.Now()
		first, err = early(ctx, events, &a)
		return first, err, a, time.Since(start)
	}
	bg := context.Background()

	// A usage, then an event that takes longer than the grace: the stream
	// starts on time with what there is.
	first, err, _, took := run(200*time.Millisecond, bg, func(c chan<- Event) {
		c <- usage
		time.Sleep(2 * time.Second)
	})
	if err != nil || len(first) != 1 || first[0].Usage == nil || took < 150*time.Millisecond || took > time.Second {
		t.Errorf("late event after a usage: %d events, err %v, after %s", len(first), err, took)
	}
	// The grace covers the whole wait, not each event.
	first, err, _, took = run(300*time.Millisecond, bg, func(c chan<- Event) {
		for {
			time.Sleep(200 * time.Millisecond)
			c <- usage
		}
	})
	if err != nil || len(first) != 1 || took > 600*time.Millisecond {
		t.Errorf("slow usages: %d events, err %v, after %s", len(first), err, took)
	}
	// A usage, then the error of a refused answer, in time: a plain
	// error, whose tokens are counted.
	refused := newBackendError("codex", failOutputLimit, "")
	first, err, a, _ := run(time.Second, bg, func(c chan<- Event) {
		c <- usage
		time.Sleep(100 * time.Millisecond)
		c <- Event{Err: refused}
	})
	if err != refused || first != nil || a.err != refused || a.usage == nil || a.usage.CompletionTokens != 9 {
		t.Errorf("error after a usage: err %v, answer %+v", err, a)
	}
	// A usage, then what follows it in a normal answer: both are replayed.
	first, err, _, _ = run(time.Second, bg, func(c chan<- Event) {
		c <- usage
		c <- Event{Finish: finishLength}
	})
	if err != nil || len(first) != 2 || first[1].Finish != finishLength {
		t.Errorf("finish after a usage: %+v, err %v", first, err)
	}
	// The request is cancelled while the next event is awaited, or before
	// any came: the wait ends at once.
	for _, sendUsage := range []bool{true, false} {
		ctx, cancel := context.WithCancel(bg)
		first, err, _, took = run(30*time.Second, ctx, func(c chan<- Event) {
			if sendUsage {
				c <- usage
			}
			time.Sleep(100 * time.Millisecond)
			cancel()
		})
		if err != nil || took > 2*time.Second || sendUsage != (len(first) == 1) {
			t.Errorf("cancelled, usage=%v: %d events, err %v, after %s", sendUsage, len(first), err, took)
		}
	}
	// The channel closes: nothing more to wait for.
	if first, err, _, took = run(30*time.Second, bg, func(c chan<- Event) { c <- usage; close(c) }); err != nil || len(first) != 1 || took > 2*time.Second {
		t.Errorf("closed after a usage: %d events, err %v, after %s", len(first), err, took)
	}
}
