package main

import (
	"errors"
	"fmt"
	"strings"
)

const transcriptIntro = "Below is a conversation transcript. Reply as the assistant to the last message. " +
	"Write only the reply itself, without role tags.\n\n"

// BuildPrompt flattens a conversation into a system prompt and one text.
// The CLIs take a single user turn, so a longer conversation is sent as a
// transcript.
func BuildPrompt(msgs []ChatMessage) (Prompt, error) {
	type turn struct{ role, text string }
	var (
		p      Prompt
		system []string
		turns  []turn
		users  int
		names  = map[string]string{} // tool call id -> function name
	)
	for _, m := range msgs {
		text, imgs, err := parseContent(m.Content, m.Role, len(p.Images))
		if err != nil {
			return p, err
		}
		if len(imgs) > 0 && m.Role != "user" && m.Role != "tool" && m.Role != "function" {
			return p, fmt.Errorf("images are supported only in user messages and function outputs, not in a %q message", m.Role)
		}
		p.Images = append(p.Images, imgs...)
		switch m.Role {
		case "system", "developer":
			if text != "" {
				system = append(system, text)
			}
		case "user":
			users++
			turns = append(turns, turn{"user", text})
		case "assistant":
			var b strings.Builder
			b.WriteString(text)
			for _, tc := range m.ToolCalls {
				names[tc.ID] = tc.Function.Name
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				fmt.Fprintf(&b, "<call_record>\nThe assistant called the function %s through its tool (call id %s), with the arguments %s\n</call_record>",
					tc.Function.Name, tc.ID, tc.Function.Arguments)
			}
			turns = append(turns, turn{"assistant", b.String()})
		case "tool", "function":
			name := m.Name
			if name == "" {
				name = names[m.ToolCallID]
			}
			turns = append(turns, turn{"tool", fmt.Sprintf("<tool_result tool_call_id=%q name=%q>\n%s\n</tool_result>", m.ToolCallID, name, text)})
		default:
			return p, fmt.Errorf("unsupported role %q", m.Role)
		}
	}
	if users == 0 {
		return p, errors.New("messages must contain at least one user message")
	}
	p.System = strings.Join(system, "\n\n")
	p.Messages = msgs
	if len(turns) == 1 {
		p.Text = turns[0].text
		return p, nil
	}
	var b strings.Builder
	b.WriteString(transcriptIntro)
	for i, t := range turns {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if t.role == "tool" {
			b.WriteString(t.text)
			continue
		}
		fmt.Fprintf(&b, "<%s>\n%s\n</%s>", t.role, t.text, t.role)
	}
	p.Text = b.String()
	return p, nil
}
