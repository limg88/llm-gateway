package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// The tests run the gateway against fake CLIs: the test binary itself,
// which behaves as claude or codex when fakeEnv is set. What the fake does
// depends on keywords in the prompt.

const fakeEnv = "LLM_GATEWAY_FAKE_CLI"

// fakeSecret stands for a credential a backend lets slip in its errors.
const fakeSecret = "sk-test-0123456789abcdefSECRET"

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) != "" && len(os.Args) > 1 {
		switch os.Args[1] {
		case "-p":
			fakeClaude(os.Args[1:])
		case "exec":
			fakeCodex(os.Args[1:])
		case "features":
			fmt.Print("shell_tool       stable             true\n" +
				"code_mode_host   stable             true\n" +
				"new_agent_thing  under development  true\n" +
				"quiet_feature    under development  false\n")
		}
		os.Exit(0)
	}
	os.Setenv(fakeEnv, "1")
	if os.Getenv("TEST_LOGS") == "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	os.Exit(m.Run())
}

func argValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func say(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func streamEvent(ev map[string]any) {
	say(map[string]any{"type": "stream_event", "event": ev})
}

func fakeClaude(args []string) {
	say(map[string]any{"type": "system", "subtype": "init"})
	line, err := bufio.NewReaderSize(os.Stdin, 1<<20).ReadString('\n')
	if err != nil && line == "" {
		return // stopped while waiting for a prompt
	}
	var in struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	json.Unmarshal([]byte(line), &in)
	var prompt string
	images := 0
	for _, c := range in.Message.Content {
		if c.Type == "image" {
			images++
		}
		prompt += c.Text
	}
	system, _ := argValue(args, "--system-prompt")
	usage := map[string]any{"input_tokens": 10, "cache_creation_input_tokens": 1, "cache_read_input_tokens": 2, "output_tokens": 5}
	text := func(parts ...string) {
		streamEvent(map[string]any{"type": "message_start"})
		streamEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"}})
		for _, p := range parts {
			streamEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": p}})
		}
		streamEvent(map[string]any{"type": "content_block_stop", "index": 0})
		streamEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": usage})
	}
	result := func(extra map[string]any) {
		r := map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "", "usage": usage}
		for k, v := range extra {
			r[k] = v
		}
		say(r)
	}

	switch {
	case strings.Contains(prompt, "FAIL_CLAUDE"):
		fmt.Fprintln(os.Stderr, "boom: not logged in")
		os.Exit(1)
	case strings.Contains(prompt, "SECRET_FAIL"):
		fmt.Fprintln(os.Stderr, "401 Unauthorized: Authorization: Bearer "+fakeSecret+" was refused for the prompt SECRET_FAIL")
		os.Exit(1)
	case strings.Contains(prompt, "TRUNCATE"):
		// The provider cuts the generation; the real CLI then asks the
		// model to resume and reports the whole as a success, whose
		// result is the tail alone.
		streamEvent(map[string]any{"type": "message_start"})
		streamEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "The first half"}})
		streamEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "max_tokens"}, "usage": usage})
		text(" and the TAIL")
		result(map[string]any{"result": " and the TAIL"})
		time.Sleep(30 * time.Second)
	case strings.Contains(prompt, "LIMIT_RESULT"):
		// As observed: a "success" that is an error, then exit code 1.
		result(map[string]any{"is_error": true, "stop_reason": "stop_sequence",
			"result": "API Error: Claude's response exceeded the 2200 output token maximum. To configure this behavior, set the CLAUDE_CODE_MAX_OUTPUT_TOKENS environment variable."})
		os.Exit(1)
	case strings.Contains(prompt, "USAGE_LIMIT"):
		say(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{
			"status": "rejected", "resetsAt": time.Now().Add(time.Hour).Unix(), "rateLimitType": "five_hour"}})
		say(map[string]any{"type": "assistant", "error": "rate_limit", "message": map[string]any{"content": []any{}}})
		result(map[string]any{"is_error": true, "result": "Claude usage limit reached"})
		os.Exit(1)
	case strings.Contains(prompt, "TOO_LONG"):
		say(map[string]any{"type": "assistant", "error": "invalid_request", "message": map[string]any{"content": []any{}}})
		result(map[string]any{"is_error": true, "result": "Prompt is too long"})
		os.Exit(1)
	case strings.Contains(prompt, "REFUSE"):
		streamEvent(map[string]any{"type": "message_start"})
		streamEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "I can"}})
		streamEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "refusal"}, "usage": usage})
		result(nil)
	case strings.Contains(prompt, "STOP_AS:"):
		// A result the CLI calls a success, after a last message that
		// stopped for the given reason ("none" for no reason at all).
		_, stop, _ := strings.Cut(prompt, "STOP_AS:")
		stop = strings.Fields(stop)[0]
		streamEvent(map[string]any{"type": "message_start"})
		streamEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Half an"}})
		extra := map[string]any{}
		switch {
		case stop == "none":
		case strings.HasPrefix(stop, "result="):
			// Only the result names the stop reason.
			extra["stop_reason"] = strings.TrimPrefix(stop, "result=")
		default:
			streamEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": usage})
		}
		if _, ok := argValue(args, "--json-schema"); ok {
			extra["structured_output"] = map[string]any{"ok": true}
		}
		result(extra)
		if stop == "model_context_window_exceeded" {
			time.Sleep(30 * time.Second) // the gateway must not wait for it
		}
	case strings.Contains(prompt, "DIE_MIDWAY"):
		streamEvent(map[string]any{"type": "message_start"})
		streamEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Half an"}})
	case strings.Contains(prompt, "SLOW"):
		time.Sleep(30 * time.Second)
	case strings.Contains(prompt, "CALL_TOOL"):
		allowed, _ := argValue(args, "--allowedTools")
		tool := strings.Split(allowed, ",")[0]
		streamEvent(map[string]any{"type": "message_start"})
		streamEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": tool}})
		streamEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"city":`}})
		streamEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `"Rome"}`}})
		streamEvent(map[string]any{"type": "content_block_stop", "index": 0})
		streamEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": usage})
		// The real CLI would now run the tool: the gateway must stop it.
		io.Copy(io.Discard, os.Stdin)
		time.Sleep(30 * time.Second)
	case strings.Contains(prompt, "TEXT_CALL"):
		text("Let me check.\n\n<inv", `oke name="get_weather">`+"\n"+`<parameter name="city">Rome</parameter>`+"\n</invoke>", "\nignored")
		result(nil)
	case strings.Contains(prompt, "SEARCH"):
		say(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_use", "id": "srv_1", "name": "WebSearch", "input": map[string]any{"query": "go release"}},
		}}})
		say(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "srv_1", "content": "Links: [Go 1.27](https://go.dev/doc/go1.27)"},
		}}})
		text("See ", "[Go 1.27](https://go.dev/doc/go1.27).")
		result(nil)
	case strings.Contains(prompt, "ECHO"):
		text(fmt.Sprintf("system=%q images=%d budget=%q prompt=%s", system, images, os.Getenv("CLAUDE_CODE_MAX_OUTPUT_TOKENS"), prompt))
		result(nil)
	default:
		if _, ok := argValue(args, "--json-schema"); ok {
			text("thinking aloud")
			result(map[string]any{"structured_output": map[string]any{"ok": true}})
			return
		}
		text("Hello", ", ", "world")
		result(map[string]any{"result": "Hello, world"})
	}
}

func fakeCodex(args []string) {
	b, _ := io.ReadAll(os.Stdin)
	prompt := string(b)
	say(map[string]any{"type": "thread.started"})
	say(map[string]any{"type": "turn.started"})
	message := func(text string) {
		say(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_0", "type": "agent_message", "text": text}})
		say(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 7, "output_tokens": 3}})
	}
	_, schema := argValue(args, "--output-schema")
	switch {
	case strings.Contains(prompt, "FAIL_CODEX"):
		say(map[string]any{"type": "turn.failed", "error": map[string]any{"message": "model overloaded"}})
	case strings.Contains(prompt, "RATE_CODEX"):
		say(map[string]any{"type": "error", "message": "Rate limit reached for requests. Please try again in 20s."})
		say(map[string]any{"type": "turn.failed", "error": map[string]any{"message": "Rate limit reached for requests. Please try again in 20s."}})
	case strings.Contains(prompt, "DIE_MIDWAY"):
		say(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_0", "type": "agent_message", "text": "Half an"}})
	case strings.Contains(prompt, "ECHO"):
		message("home=" + os.Getenv("CODEX_HOME") + " args=" + strings.Join(args, " ") + " prompt=" + prompt)
	case schema && strings.Contains(prompt, "CALL_TOOL"):
		message(`{"content":null,"tool_calls":[{"name":"get_weather","arguments":"{\"city\":\"Rome\"}"}]}`)
	case schema:
		message(`{"content":"no call needed","tool_calls":[]}`)
	default:
		message("codex says hi")
	}
}
