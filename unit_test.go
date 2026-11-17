package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ns builds a namespaced markup from a plain one: <invoke ...> becomes the
// "antml:" form of the same elements.
func ns(markup string) string {
	const prefix = "antml" + ":"
	r := strings.NewReplacer(
		"<invoke", "<"+prefix+"invoke", "</invoke", "</"+prefix+"invoke",
		"<parameter", "<"+prefix+"parameter", "</parameter", "</"+prefix+"parameter",
		"<function_calls", "<"+prefix+"function_calls", "</function_calls", "</"+prefix+"function_calls",
	)
	return r.Replace(markup)
}

func TestTextCalls(t *testing.T) {
	defs := []FunctionDefinition{{
		Name:       "get_weather",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"},"days":{"type":"integer"}}}`),
	}}
	invoke := `<invoke name="get_weather"><parameter name="city">Rome</parameter><parameter name="days">3</parameter></invoke>`
	wrapped := "<function_calls>\n" + `<invoke name="get_weather">` + "\n" + `<parameter name="city">12</parameter>` + "\n</invoke>\n</function_calls>"
	cases := []struct {
		name, in string
		text     string
		args     string // empty: no call
	}{
		{"plain", "a < b and <b>bold</b>", "a < b and <b>bold</b>", ""},
		{"invoke", "Sure." + invoke + " tail", "Sure.", `{"city":"Rome","days":3}`},
		{"namespaced invoke", "Sure." + ns(invoke), "Sure.", `{"city":"Rome","days":3}`},
		{"wrapped, string kept as string", wrapped, "", `{"city":"12"}`},
		{"namespaced wrapper", ns(wrapped), "", `{"city":"12"}`},
		{"tool_call json", `<tool_call>{"name":"get_weather","arguments":{"city":"Rome"}}</tool_call>`, "", `{"city":"Rome"}`},
		{"tool_call string arguments", `<tool_call>{"name":"get_weather","arguments":"{\"city\":\"Rome\"}"}</tool_call>`, "", `{"city":"Rome"}`},
		{"unknown function", `<invoke name="other"><parameter name="a">1</parameter></invoke>`, `<invoke name="other"><parameter name="a">1</parameter></invoke>`, ""},
		// A reply cut before the closing tag still makes its call.
		{"unclosed call at the end", `see <invoke name="get_weather"><parameter name="city">Rome</parameter>`, "see", `{"city":"Rome"}`},
		{"unclosed json is text", `see <tool_call>{"name":"get_weather","argu`, `see <tool_call>{"name":"get_weather","argu`, ""},
		{"lookalike tag", "<invoked> <tool_calls>", "<invoked> <tool_calls>", ""},
	}
	for _, c := range cases {
		// Whatever the size of the deltas, the outcome is the same.
		for _, size := range []int{1, 3, 7, len(c.in)} {
			f := newTextCalls(defs)
			var text strings.Builder
			var call *FunctionCall
			collect := func(pieces []textPiece) {
				for _, p := range pieces {
					if p.call != nil {
						call = p.call
					}
					text.WriteString(p.text)
				}
			}
			for i := 0; i < len(c.in); i += size {
				pieces, stop := f.feed(c.in[i:min(i+size, len(c.in))])
				collect(pieces)
				if stop {
					break
				}
			}
			collect(f.flush())
			if text.String() != c.text {
				t.Errorf("%s (deltas of %d): text = %q, want %q", c.name, size, text.String(), c.text)
			}
			switch {
			case c.args == "" && call != nil:
				t.Errorf("%s (deltas of %d): unexpected call %+v", c.name, size, call)
			case c.args != "" && (call == nil || call.Arguments != c.args || call.Name != "get_weather"):
				t.Errorf("%s (deltas of %d): call = %+v, want arguments %s", c.name, size, call, c.args)
			}
		}
	}
}

func TestBuildPrompt(t *testing.T) {
	msg := func(role, content string) ChatMessage {
		return ChatMessage{Role: role, Content: jsonString(content)}
	}
	p, err := BuildPrompt([]ChatMessage{msg("system", "A"), msg("developer", "B"), msg("user", "only")})
	if err != nil || p.System != "A\n\nB" || p.Text != "only" {
		t.Errorf("single turn: %+v %v", p, err)
	}

	p, err = BuildPrompt([]ChatMessage{
		msg("user", "weather in Rome?"),
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"Rome"}`}}}},
		{Role: "tool", ToolCallID: "call_1", Content: jsonString("sunny")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Below is a conversation transcript.",
		"<user>\nweather in Rome?\n</user>",
		`The assistant called the function get_weather through its tool (call id call_1), with the arguments {"city":"Rome"}`,
		`<tool_result tool_call_id="call_1" name="get_weather">` + "\nsunny\n</tool_result>",
	} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("transcript lacks %q:\n%s", want, p.Text)
		}
	}

	if _, err := BuildPrompt([]ChatMessage{{Role: "assistant", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]`)}}); err == nil {
		t.Error("an image in an assistant message was accepted")
	}
}

func TestImages(t *testing.T) {
	if _, err := parseDataURL("data:image/png;base64,aGVsbG8gd29ybGQ=", 1); err == nil || !strings.Contains(err.Error(), "unsupported image type") {
		t.Errorf("text passed as png: %v", err)
	}
	if _, err := parseDataURL("data:image/png;base64,@@@", 1); err == nil {
		t.Error("invalid base64 accepted")
	}
	big := Image{MediaType: "image/png", Data: make([]byte, maxImageBase64)}
	if err := checkImages([]Image{big}); err == nil {
		t.Error("oversized images accepted")
	}
	_, _, err := parseContent(json.RawMessage(`[{"type":"image_url","file_id":"file_1"}]`), "user", 0)
	if err == nil || !strings.Contains(err.Error(), "file_id is not supported") {
		t.Errorf("file_id: %v", err)
	}
}

func TestToolChoiceAndSchema(t *testing.T) {
	for in, want := range map[string][2]string{
		``:           {"auto", ""},
		`"none"`:     {"none", ""},
		`"required"`: {"required", ""},
		`{"type":"function","function":{"name":"f"}}`: {"required", "f"},
		`{"type":"function","name":"f"}`:              {"required", "f"},
		`{"type":"tool","name":"f"}`:                  {"required", "f"},
		`{"type":"any"}`:                              {"required", ""},
	} {
		mode, forced, err := parseToolChoice(json.RawMessage(in))
		if err != nil || mode != want[0] || forced != want[1] {
			t.Errorf("parseToolChoice(%s) = %q %q %v", in, mode, forced, err)
		}
	}
	if _, _, err := parseToolChoice(json.RawMessage(`"sometimes"`)); err == nil {
		t.Error("invalid tool_choice accepted")
	}

	var schema struct {
		Properties struct {
			ToolCalls struct {
				MinItems, MaxItems int
			} `json:"tool_calls"`
		}
		Required             []string
		AdditionalProperties *bool
	}
	json.Unmarshal(toolSchema([]string{"f"}, true, true), &schema)
	if schema.Properties.ToolCalls.MinItems != 1 || schema.Properties.ToolCalls.MaxItems != 1 || len(schema.Required) != 2 || schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Errorf("schema = %+v", schema)
	}

	content, calls := parseToolOutput("```json\n" + `{"content":"hi","tool_calls":[{"name":"f","arguments":"not json"}]}` + "\n```")
	if content != "hi" || len(calls) != 1 || calls[0].Function.Arguments != "{}" {
		t.Errorf("parseToolOutput = %q %+v", content, calls)
	}
	if content, calls := parseToolOutput("just text"); content != "just text" || calls != nil {
		t.Errorf("plain text = %q %+v", content, calls)
	}
}

func TestCitations(t *testing.T) {
	text := "Però: [Go](https://go.dev) and [X](https://x.dev/a)."
	got := citations(text, []Source{{URL: "https://go.dev", Title: "The Go Language"}})
	if len(got) != 2 {
		t.Fatalf("citations = %+v", got)
	}
	// Indexes count characters, not bytes: "Però: " is six of them.
	if got[0].StartIndex != 6 || got[0].EndIndex != 6+len("[Go](https://go.dev)") || got[0].Title != "The Go Language" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Title != "X" || got[1].URL != "https://x.dev/a" {
		t.Errorf("second = %+v", got[1])
	}
	srcs := claudeSearchSources(json.RawMessage(`"see https://a.example/x."`), json.RawMessage(`{"results":[{"content":[{"title":"B","url":"https://b.example"}]}]}`))
	if len(srcs) != 2 || srcs[0].URL != "https://b.example" || srcs[1].URL != "https://a.example/x" {
		t.Errorf("sources = %+v", srcs)
	}
}

type fakeProc struct {
	done chan struct{}
}

func newFakeProc() *fakeProc { return &fakeProc{done: make(chan struct{})} }

func (p *fakeProc) kill() {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}
func (p *fakeProc) exited() <-chan struct{} { return p.done }

func TestWarmPoolUnit(t *testing.T) {
	pool := newWarmPool[*fakeProc](2, 80*time.Millisecond)
	start := func() (*fakeProc, error) { return newFakeProc(), nil }
	waitIdle := func(n int) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if idle, _, _ := pool.stats(); idle == n {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		idle, _, _ := pool.stats()
		t.Fatalf("idle = %d, want %d", idle, n)
	}

	pool.add("a", start)
	waitIdle(1)
	pool.add("a", start) // one per key
	time.Sleep(20 * time.Millisecond)
	waitIdle(1)
	if _, ok := pool.take("b"); ok {
		t.Error("took a process of another key")
	}
	a, ok := pool.take("a")
	if !ok {
		t.Fatal("no process for a")
	}
	select {
	case <-a.exited():
		t.Error("the taken process was killed")
	default:
	}

	// The oldest goes when the pool is full.
	pool.add("a", start)
	waitIdle(1)
	pool.add("b", start)
	waitIdle(2)
	pool.add("c", start)
	time.Sleep(30 * time.Millisecond)
	if _, ok := pool.take("a"); ok {
		t.Error("the oldest process was not replaced")
	}

	// Idle processes are stopped.
	waitIdle(0)

	// A process that dies while waiting leaves the pool.
	dying := newFakeProc()
	pool.add("d", func() (*fakeProc, error) { return dying, nil })
	waitIdle(1)
	dying.kill()
	waitIdle(0)

	pool.close()
	pool.add("e", start)
	time.Sleep(20 * time.Millisecond)
	waitIdle(0)
}

func TestConversationStore(t *testing.T) {
	s := newConversationStore(50*time.Millisecond, 2)
	msgs := []ChatMessage{{Role: "user"}}
	s.put("r1", "alice", msgs)
	if _, ok := s.get("r1", "bob"); ok {
		t.Error("another key read the conversation")
	}
	if _, ok := s.get("r1", "alice"); !ok {
		t.Error("the owner cannot read the conversation")
	}
	s.put("r2", "alice", msgs)
	s.put("r3", "alice", msgs)
	if _, ok := s.get("r1", "alice"); ok || s.len() != 2 {
		t.Errorf("the oldest was not dropped: len %d", s.len())
	}
	time.Sleep(70 * time.Millisecond)
	if _, ok := s.get("r3", "alice"); ok {
		t.Error("an expired conversation was returned")
	}
	s.put("r4", "alice", msgs)
	if s.len() != 1 {
		t.Errorf("expired entries were not swept: len %d", s.len())
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(2)
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow(); !ok {
			t.Fatalf("request %d refused", i)
		}
	}
	ok, wait := l.allow()
	if ok || wait <= 0 || wait > 30*time.Second {
		t.Errorf("third request: ok=%v wait=%v", ok, wait)
	}
}

func TestMetricsRender(t *testing.T) {
	m := NewMetrics()
	m.Inc("requests_total", "backend", `a"b`)
	m.Observe("duration_seconds", 0.7, "backend", "x")
	m.Gauge("up", "Whether it is up.", func() float64 { return 1 })
	var b bytes.Buffer
	m.Render(&b)
	for _, want := range []string{
		"# HELP up Whether it is up.\n# TYPE up gauge\nup 1\n",
		`requests_total{backend="a\"b"} 1`,
		`duration_seconds_bucket{backend="x",le="0.5"} 0`,
		`duration_seconds_bucket{backend="x",le="1"} 1`,
		`duration_seconds_bucket{backend="x",le="+Inf"} 1`,
		`duration_seconds_count{backend="x"} 1`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, b.String())
		}
	}
}

func TestClaudeArgs(t *testing.T) {
	c := NewClaudeBackend("claude", "/tmp", 0, time.Second)
	args := c.args("opus", Prompt{
		WebSearch: true, Effort: "minimal", Schema: json.RawMessage(`{"type":"object"}`),
		Functions: []FunctionDefinition{{Name: "f"}, {Name: "g"}},
	}, "http://127.0.0.1:1/mcp/x")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-p --input-format stream-json --output-format stream-json --verbose --include-partial-messages",
		"--tools WebSearch --strict-mcp-config",
		"--system-prompt " + defaultSystemPrompt,
		"--setting-sources project,local",
		`--mcp-config {"mcpServers":{"fn":{"type":"http","url":"http://127.0.0.1:1/mcp/x"}}}`,
		"--allowedTools WebSearch,mcp__fn__f,mcp__fn__g",
		"--model opus --effort low --json-schema",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q:\n%s", want, joined)
		}
	}
	// Fetching URLs from this machine is the operator's choice.
	c.WebFetch = true
	if got := strings.Join(c.args("", Prompt{WebSearch: true}, ""), " "); !strings.Contains(got, "--tools WebSearch,WebFetch ") || !strings.Contains(got, "--allowedTools WebSearch,WebFetch") {
		t.Errorf("with web fetch: %s", got)
	}
	// No tools at all without web search: the value is an empty argument.
	plain := c.args("", Prompt{}, "")
	for i, a := range plain {
		if a == "--tools" && plain[i+1] != "" {
			t.Errorf("--tools %q, want empty", plain[i+1])
		}
		if a == "--model" || a == "--allowedTools" {
			t.Errorf("unexpected %s", a)
		}
	}
	if poolKey("a", Prompt{System: "x"}) == poolKey("a", Prompt{System: "y"}) || poolKey("a", Prompt{Text: "1"}) != poolKey("a", Prompt{Text: "2"}) {
		t.Error("the pool key must depend on the command line only")
	}
	// The output budget is in the environment of the process.
	if poolKey("a", Prompt{MaxTokens: 100}) == poolKey("a", Prompt{MaxTokens: 200}) || poolKey("a", Prompt{}) == poolKey("a", Prompt{MaxTokens: 100}) {
		t.Error("the pool key must depend on the output budget")
	}
}

func TestConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{
		"listen_addr": "127.0.0.1:9000",
		"max_concurrency": 8,
		"api_keys": [{"name": "team", "key_env": "TEST_TEAM_KEY", "rpm": 30}],
		"models": {"fast": ["claude/haiku", "codex"]},
		"upstreams": [{"name": "ollama", "base_url": "http://localhost:11434/v1"}]
	}`), 0o600)
	t.Setenv("TEST_TEAM_KEY", "secret")
	t.Setenv("MAX_CONCURRENCY", "6")
	t.Setenv("WARM", "3")

	cfg, err := loadConfig([]string{"-config", path, "-warm", "2", "-api-key", "cli-key"}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	// Flag over environment over file over default.
	if cfg.Warm != 2 || cfg.MaxConcurrency != 6 || cfg.ListenAddr != "127.0.0.1:9000" || cfg.RequestTimeout != 300 {
		t.Errorf("precedence: %+v", cfg)
	}
	if cfg.SelfURL != "http://127.0.0.1:9000" || !cfg.loopback() {
		t.Errorf("self url = %q", cfg.SelfURL)
	}
	if len(cfg.APIKeys) != 2 || cfg.APIKeys[0].Name != "default" || cfg.APIKeys[1].Key != "secret" {
		t.Errorf("keys = %+v", cfg.APIKeys)
	}

	for name, body := range map[string]string{
		"unknown field":    `{"listen_adr": ":1"}`,
		"reserved name":    `{"upstreams": [{"name": "claude", "base_url": "http://x"}]}`,
		"empty alias":      `{"models": {"x": []}}`,
		"unknown backend":  `{"backends": ["gemini"]}`,
		"budget field":     `{"upstreams": [{"name": "x", "base_url": "http://x", "max_tokens_field": "max_output_tokens"}]}`,
		"zero concurrency": `{"max_concurrency": 0}`,
		"unsafe codex arg": `{"codex_extra_args": ["--sandbox=danger-full-access"]}`,
		"codex bypass":     `{"codex_extra_args": ["--dangerously-bypass-approvals-and-sandbox"]}`,
	} {
		os.WriteFile(path, []byte(body), 0o600)
		t.Setenv("MAX_CONCURRENCY", "")
		if _, err := loadConfig([]string{"-config=" + path}, os.Stderr); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
