package main

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"strings"
	"time"
)

// The Anthropic Messages API, served by the same backends.

type MessagesRequest struct {
	Model    string          `json:"model"`
	System   json.RawMessage `json:"system"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream    bool `json:"stream"`
	MaxTokens *int `json:"max_tokens"`
	Tools     []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
	ToolChoice   json.RawMessage `json:"tool_choice"`
	OutputConfig *struct {
		Effort string `json:"effort"`
		Format *struct {
			Type   string          `json:"type"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"output_config"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Source    json.RawMessage `json:"source,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type MessagesResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []anthropicBlock `json:"content"`
	StopReason   *string          `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        anthropicUsage   `json:"usage"`
}

// chatRequest converts a Messages request to the chat form.
func (req MessagesRequest) chatRequest() (ChatCompletionRequest, *apiErr) {
	chat := ChatCompletionRequest{Model: req.Model, Stream: req.Stream, MaxTokens: req.MaxTokens}

	if sys := bytes.TrimSpace(req.System); len(sys) > 0 && !bytes.Equal(sys, []byte("null")) {
		text, _, err := parseContent(sys, "system", 0)
		if err != nil {
			return chat, badRequest("invalid system prompt")
		}
		chat.Messages = append(chat.Messages, ChatMessage{Role: "system", Content: jsonString(text)})
	}

	for _, m := range req.Messages {
		content := bytes.TrimSpace(m.Content)
		if len(content) == 0 || content[0] != '[' {
			chat.Messages = append(chat.Messages, ChatMessage{Role: m.Role, Content: content})
			continue
		}
		var blocks []anthropicBlock
		if err := json.Unmarshal(content, &blocks); err != nil {
			return chat, badRequest("invalid content for role %q", m.Role)
		}
		var parts []anthropicBlock // text and images, kept as they are
		msg := ChatMessage{Role: m.Role}
		for _, b := range blocks {
			switch b.Type {
			case "text", "image":
				parts = append(parts, b)
			case "tool_use":
				args := string(bytes.TrimSpace(b.Input))
				if args == "" {
					args = "{}"
				}
				msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: b.ID, Type: "function", Function: FunctionCall{Name: b.Name, Arguments: args}})
			case "tool_result":
				chat.Messages = append(chat.Messages, ChatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: b.Content})
			case "thinking", "redacted_thinking":
			default:
				return chat, badRequest("content block type %q is not supported", b.Type)
			}
		}
		if len(parts) > 0 {
			msg.Content, _ = json.Marshal(parts)
		}
		if len(parts) > 0 || len(msg.ToolCalls) > 0 {
			chat.Messages = append(chat.Messages, msg)
		}
	}

	for _, t := range req.Tools {
		switch {
		case strings.HasPrefix(t.Type, "web_search"), strings.HasPrefix(t.Type, "web_fetch"):
			chat.WebSearchOptions = json.RawMessage("{}")
		case t.Type == "" || t.Type == "custom":
			chat.Tools = append(chat.Tools, Tool{Type: "function", Function: FunctionDefinition{
				Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
			}})
		default:
			return chat, badRequest("tool type %q is not supported", t.Type)
		}
	}
	if tc := bytes.TrimSpace(req.ToolChoice); len(tc) > 0 {
		chat.ToolChoice = tc
		var o struct {
			DisableParallel bool `json:"disable_parallel_tool_use"`
		}
		if json.Unmarshal(tc, &o) == nil && o.DisableParallel {
			no := false
			chat.ParallelToolCalls = &no
		}
	}
	if oc := req.OutputConfig; oc != nil {
		chat.ReasoningEffort = oc.Effort
		if oc.Format != nil && oc.Format.Type == "json_schema" {
			chat.ResponseFormat = &ResponseFormat{Type: "json_schema", JSONSchema: &struct {
				Name   string          `json:"name"`
				Schema json.RawMessage `json:"schema"`
			}{Name: "response", Schema: oc.Format.Schema}}
		}
	}
	return chat, nil
}

// anthropicStop is a finish reason in the words of the Messages API.
func anthropicStop(finish string) string {
	switch finish {
	case finishLength:
		return "max_tokens"
	case finishToolCalls:
		return "tool_use"
	case finishFilter:
		return "refusal"
	}
	return "end_turn"
}

func toolUseBlock(tc ToolCall) anthropicBlock {
	input := json.RawMessage(tc.Function.Arguments)
	if !json.Valid(input) {
		input = json.RawMessage("{}")
	}
	return anthropicBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input}
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req MessagesRequest
	if aerr := decodeBody(w, r, &req); aerr != nil {
		writeAnthropicAPIErr(w, aerr)
		return
	}
	chat, aerr := req.chatRequest()
	if aerr != nil {
		writeAnthropicAPIErr(w, aerr)
		return
	}
	j, aerr := s.prepare("messages", chat, keyOf(r))
	if aerr != nil {
		writeAnthropicAPIErr(w, aerr)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.RequestTimeout)*time.Second)
	defer cancel()
	events, aerr := s.run(ctx, j)
	if aerr != nil {
		writeAnthropicAPIErr(w, aerr)
		return
	}
	var a answer
	defer func() {
		drain(events)
		s.observe(j, &a)
	}()

	resp := MessagesResponse{ID: "msg_" + randomID(), Type: "message", Role: "assistant", Model: req.Model, Content: []anthropicBlock{}}
	if req.Stream {
		first, err := early(ctx, events, &a)
		if err != nil {
			writeAnthropicAPIErr(w, publicError(err))
			return
		}
		s.streamMessages(w, replay(first, events), &a, resp)
		return
	}
	for ev := range events {
		a.add(ev)
	}
	if a.err != nil {
		writeAnthropicAPIErr(w, publicError(a.err))
		return
	}
	if text := a.text.String(); text != "" || len(a.calls) == 0 {
		resp.Content = append(resp.Content, anthropicBlock{Type: "text", Text: text})
	}
	for _, tc := range a.calls {
		resp.Content = append(resp.Content, toolUseBlock(tc))
	}
	stop := anthropicStop(a.reason())
	resp.StopReason = &stop
	if a.usage != nil {
		resp.Usage = anthropicUsage{InputTokens: a.usage.PromptTokens, OutputTokens: a.usage.CompletionTokens}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) streamMessages(w http.ResponseWriter, events iter.Seq[Event], a *answer, resp MessagesResponse) {
	sse := startSSE(w)
	send := func(typ string, fields map[string]any) {
		fields["type"] = typ
		sse.send(typ, fields)
	}
	send("message_start", map[string]any{"message": resp})

	index := 0        // next content block
	textOpen := false // the block index-1 is an open text block
	closeText := func() {
		if textOpen {
			send("content_block_stop", map[string]any{"index": index - 1})
			textOpen = false
		}
	}
	// A tool_use block is written whole once its call is complete, as the
	// blocks of a message cannot interleave.
	written := map[int]bool{}
	writeCall := func(i int) {
		if written[i] || i >= len(a.calls) {
			return
		}
		written[i] = true
		closeText()
		block := toolUseBlock(a.calls[i])
		args := string(block.Input)
		block.Input = json.RawMessage("{}")
		send("content_block_start", map[string]any{"index": index, "content_block": block})
		send("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}})
		send("content_block_stop", map[string]any{"index": index})
		index++
	}

	for ev := range events {
		a.add(ev)
		switch {
		case ev.Err != nil:
			// No message_delta and no message_stop: the message is not whole.
			send("error", map[string]any{"error": publicError(ev.Err).anthropicBody()})
			return
		case ev.Text != "":
			if !textOpen {
				send("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "text", "text": ""}})
				index++
				textOpen = true
			}
			send("content_block_delta", map[string]any{"index": index - 1, "delta": map[string]any{"type": "text_delta", "text": ev.Text}})
		case ev.Call != nil && ev.Call.Done:
			writeCall(ev.Call.Index)
		}
	}
	for i := range a.calls {
		writeCall(i)
	}
	if index == 0 {
		send("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		index, textOpen = 1, true
	}
	closeText()
	stop := anthropicStop(a.reason())
	usage := anthropicUsage{}
	if a.usage != nil {
		usage = anthropicUsage{InputTokens: a.usage.PromptTokens, OutputTokens: a.usage.CompletionTokens}
	}
	send("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": usage})
	send("message_stop", map[string]any{})
}
