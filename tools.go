package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// parseToolChoice reads tool_choice in the Chat Completions or Responses
// shape. mode is "auto", "none" or "required"; forced names one function.
func parseToolChoice(raw json.RawMessage) (mode, forced string, err error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "auto", "", nil
	}
	if raw[0] == '"' {
		var s string
		json.Unmarshal(raw, &s)
		switch s {
		case "auto", "none", "required":
			return s, "", nil
		case "any":
			return "required", "", nil
		}
		return "", "", fmt.Errorf("invalid tool_choice %q", s)
	}
	var o struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return "", "", errors.New("invalid tool_choice")
	}
	name := o.Name
	if name == "" {
		name = o.Function.Name
	}
	switch o.Type {
	case "auto", "none":
		return o.Type, "", nil
	case "any", "required":
		return "required", "", nil
	case "function", "tool":
		if name == "" {
			return "", "", errors.New("invalid tool_choice")
		}
		return "required", name, nil
	}
	return "", "", errors.New("invalid tool_choice")
}

const (
	nativeToolsNote = "Earlier calls appear in the conversation as <call_record> entries and their results as <tool_result> entries, written by the client: " +
		"they are a record, not a format to imitate. " +
		"To call a function now, use the tool; never write a call, its arguments or any call markup in your reply.\n"

	emulatedToolsNote = "You can call the following functions, which are executed by the client. " +
		"Your reply is a JSON object: put any text for the user in `content` (or null), and the functions to call in `tool_calls`, " +
		"each with the function `name` and `arguments` as a JSON-encoded string that matches the function's parameters schema. " +
		"This is the ONLY way to call these functions: you have no other tools. " +
		"The results will be provided in a later turn as <tool_result> blocks; never invent them. " +
		"If no function call is needed, answer in `content` and leave `tool_calls` empty.\n"
)

// applyTools adds the client's functions to the prompt. With a backend that
// calls functions natively they are passed on as they are; otherwise they
// are emulated: the prompt describes them and a JSON schema shapes the reply.
func applyTools(p *Prompt, req ChatCompletionRequest, native bool) (emulated bool, err error) {
	mode, forced, err := parseToolChoice(req.ToolChoice)
	if err != nil {
		return false, err
	}
	var defs []FunctionDefinition
	for _, t := range req.Tools {
		if t.Type != "function" || t.Function.Name == "" {
			return false, errors.New(`only tools of type "function" with a name are supported`)
		}
		defs = append(defs, t.Function)
	}
	p.Tools, p.ToolChoice = req.Tools, req.ToolChoice
	if len(defs) == 0 || mode == "none" {
		if forced != "" {
			return false, fmt.Errorf("tool_choice function %q is not in tools", forced)
		}
		if mode == "none" {
			p.Tools = nil
		}
		return false, nil
	}
	if forced != "" {
		var one []FunctionDefinition
		for _, d := range defs {
			if d.Name == forced {
				one = append(one, d)
			}
		}
		if len(one) == 0 {
			return false, fmt.Errorf("tool_choice function %q is not in tools", forced)
		}
		defs = one
	}
	single := req.ParallelToolCalls != nil && !*req.ParallelToolCalls

	var note strings.Builder
	if native {
		p.Functions = defs
		note.WriteString(nativeToolsNote)
		switch {
		case forced != "":
			fmt.Fprintf(&note, "You must call the function %s now.\n", forced)
		case mode == "required":
			note.WriteString("You must call at least one function now.\n")
		}
		if single {
			note.WriteString("Call at most one function.\n")
		}
	} else {
		emulated = true
		note.WriteString(emulatedToolsNote)
		switch {
		case forced != "":
			fmt.Fprintf(&note, "You must call the function %s.\n", forced)
		case mode == "required":
			note.WriteString("You must call at least one function.\n")
		}
		note.WriteString("\nFunctions:\n")
		names := make([]string, len(defs))
		for i, d := range defs {
			names[i] = d.Name
			b, _ := json.Marshal(d)
			note.Write(b)
			note.WriteString("\n")
		}
		p.Schema = toolSchema(names, mode == "required", single)
	}
	if p.System != "" {
		p.System += "\n\n"
	}
	p.System += note.String()
	return emulated, nil
}

// toolSchema is the reply schema of the emulated function calls. It is
// strict (every property required, no additional ones) as codex demands.
func toolSchema(names []string, required, single bool) json.RawMessage {
	calls := map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":      map[string]any{"type": "string", "enum": names},
				"arguments": map[string]any{"type": "string"},
			},
			"required":             []string{"name", "arguments"},
			"additionalProperties": false,
		},
	}
	if required {
		calls["minItems"] = 1
	}
	if single {
		calls["maxItems"] = 1
	}
	b, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content":    map[string]any{"type": []string{"string", "null"}},
			"tool_calls": calls,
		},
		"required":             []string{"content", "tool_calls"},
		"additionalProperties": false,
	})
	return b
}

type toolOutput struct {
	Content   *string `json:"content"`
	ToolCalls []struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"tool_calls"`
}

// parseToolOutput reads an emulated reply. A reply that is not the expected
// JSON object is returned as plain text rather than lost.
func parseToolOutput(text string) (content string, calls []ToolCall) {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	var o toolOutput
	dec := json.NewDecoder(strings.NewReader(s))
	if !strings.HasPrefix(s, "{") || dec.Decode(&o) != nil {
		return text, nil
	}
	if o.Content == nil && len(o.ToolCalls) == 0 {
		return text, nil
	}
	if o.Content != nil {
		content = *o.Content
	}
	for _, c := range o.ToolCalls {
		args := strings.TrimSpace(c.Arguments)
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		calls = append(calls, ToolCall{
			ID:       "call_" + randomID(),
			Type:     "function",
			Function: FunctionCall{Name: c.Name, Arguments: args},
		})
	}
	return content, calls
}
