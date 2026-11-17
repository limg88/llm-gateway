package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// A model that has functions sometimes writes a call as text instead of
// using its tool. textCalls scans the streamed text for the usual markups
// and turns such a call into a real one, so that the client never sees it
// as text.
//
// Recognized forms, with or without the "antml:" namespace and with or
// without a <function_calls> wrapper:
//
//	<tool_call>{"name": "f", "arguments": {...}}</tool_call>
//	<invoke name="f"><parameter name="a">1</parameter></invoke>

var (
	callTags    = []string{"tool_call", "invoke", "antml:invoke"}
	wrapperTags = []string{"function_calls", "antml:function_calls"}
	paramRE     = regexp.MustCompile(`(?s)<(?:antml:)?parameter\s+name="([^"]*)"\s*>(.*?)</(?:antml:)?parameter>`)
	nameAttrRE  = regexp.MustCompile(`\bname\s*=\s*"([^"]*)"`)
)

// maxMarkup bounds how much text is held back while a markup is incomplete.
const maxMarkup = 256 << 10

type textPiece struct {
	text string
	call *FunctionCall
}

type markupStatus int

const (
	notMarkup markupStatus = iota
	incompleteMarkup
	completeMarkup
)

type textCalls struct {
	funcs  map[string]FunctionDefinition
	buf    string // text not yet examined, or an incomplete markup
	held   string // whitespace before the incomplete markup
	called bool
}

func newTextCalls(defs []FunctionDefinition) *textCalls {
	f := &textCalls{funcs: map[string]FunctionDefinition{}}
	for _, d := range defs {
		f.funcs[d.Name] = d
	}
	return f
}

// feed adds streamed text and returns what can be emitted. stop is set once
// a call was found: the rest of the reply is to be dropped.
func (f *textCalls) feed(delta string) (out []textPiece, stop bool) {
	if f.called {
		return nil, true
	}
	f.buf += delta
	return f.scan(false)
}

// flush returns the text still held back at the end of the reply.
func (f *textCalls) flush() []textPiece {
	if f.called {
		return nil
	}
	out, _ := f.scan(true)
	return out
}

func (f *textCalls) scan(final bool) (out []textPiece, stop bool) {
	var text strings.Builder
	text.WriteString(f.held)
	f.held = ""
	flush := func() {
		if text.Len() > 0 {
			out = append(out, textPiece{text: text.String()})
			text.Reset()
		}
	}
	for {
		i := strings.IndexByte(f.buf, '<')
		if i < 0 {
			text.WriteString(f.buf)
			f.buf = ""
			if !final {
				// Trailing whitespace waits: a call may follow it.
				s := text.String()
				body := strings.TrimRight(s, " \t\r\n")
				f.held = s[len(body):]
				text.Reset()
				text.WriteString(body)
			}
			break
		}
		text.WriteString(f.buf[:i])
		f.buf = f.buf[i:]
		n, call, status := f.markup(f.buf, final)
		switch {
		case status == completeMarkup && call != nil:
			// A call ends the reply: drop the whitespace before it.
			s := strings.TrimRight(text.String(), " \t\r\n")
			text.Reset()
			text.WriteString(s)
			flush()
			out = append(out, textPiece{call: call})
			f.buf, f.called = "", true
			return out, true
		case status == incompleteMarkup && !final && len(f.buf) < maxMarkup:
			// The whitespace before a call is not part of the reply: it
			// waits with the markup.
			s := text.String()
			body := strings.TrimRight(s, " \t\r\n")
			f.held = s[len(body):]
			text.Reset()
			text.WriteString(body)
			flush()
			return out, false
		case status == completeMarkup:
			text.WriteString(f.buf[:n])
			f.buf = f.buf[n:]
		default:
			text.WriteString("<")
			f.buf = f.buf[1:]
		}
	}
	flush()
	return out, false
}

// markup reads the markup at the start of s, which begins with '<'.
func (f *textCalls) markup(s string, final bool) (int, *FunctionCall, markupStatus) {
	name, rest, status := tagName(s, append(append([]string{}, callTags...), wrapperTags...), final)
	if status != completeMarkup {
		return 0, nil, status
	}
	if oneOf(name, wrapperTags) {
		gt := strings.IndexByte(rest, '>')
		if gt < 0 {
			return 0, nil, incompleteMarkup
		}
		inner := strings.TrimLeft(rest[gt+1:], " \t\r\n")
		if inner == "" {
			return 0, nil, incompleteMarkup
		}
		if inner[0] != '<' {
			return 0, nil, notMarkup
		}
		n, call, st := f.markup(inner, final)
		if st != completeMarkup || call == nil {
			if st == completeMarkup {
				st = notMarkup
			}
			return 0, nil, st
		}
		return len(s) - len(inner) + n, call, completeMarkup
	}

	gt := strings.IndexByte(rest, '>')
	if gt < 0 {
		return 0, nil, incompleteMarkup
	}
	attrs, body := rest[:gt], rest[gt+1:]
	closer := "</" + name + ">"
	end := strings.Index(body, closer)
	consumed := len(s) - len(body)
	if end < 0 {
		if !final {
			return 0, nil, incompleteMarkup
		}
		end = len(body)
		consumed += end
	} else {
		consumed += end + len(closer)
	}
	body = body[:end]

	if name == "tool_call" {
		var o struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(body)), &o) != nil {
			return 0, nil, notMarkup
		}
		if _, ok := f.funcs[o.Name]; !ok {
			return 0, nil, notMarkup
		}
		args := bytes.TrimSpace(o.Arguments)
		if len(args) > 0 && args[0] == '"' {
			var inner string
			json.Unmarshal(args, &inner)
			args = []byte(inner)
		}
		if len(args) == 0 || !json.Valid(args) {
			args = []byte("{}")
		}
		var compact bytes.Buffer
		json.Compact(&compact, args)
		return consumed, &FunctionCall{Name: o.Name, Arguments: compact.String()}, completeMarkup
	}

	m := nameAttrRE.FindStringSubmatch(attrs)
	if m == nil {
		return 0, nil, notMarkup
	}
	def, ok := f.funcs[m[1]]
	if !ok {
		return 0, nil, notMarkup
	}
	return consumed, &FunctionCall{Name: def.Name, Arguments: paramArgs(def, body)}, completeMarkup
}

// tagName reads the tag name after '<'. A text that could still become one
// of the known tags is incomplete; any other is not markup.
func tagName(s string, known []string, final bool) (name, rest string, status markupStatus) {
	body := s[1:]
	end := strings.IndexAny(body, " \t\r\n>")
	if end < 0 {
		if !final {
			for _, k := range known {
				if strings.HasPrefix(k, body) {
					return "", "", incompleteMarkup
				}
			}
		}
		return "", "", notMarkup
	}
	name = body[:end]
	if !oneOf(name, known) {
		return "", "", notMarkup
	}
	return name, body[end:], completeMarkup
}

func oneOf(name string, list []string) bool {
	for _, s := range list {
		if s == name {
			return true
		}
	}
	return false
}

// paramArgs builds the JSON arguments from <parameter> elements. A value is
// kept as a string where the schema says so, and read as JSON otherwise.
func paramArgs(def FunctionDefinition, body string) string {
	var schema struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
	}
	json.Unmarshal(def.Parameters, &schema)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range paramRE.FindAllStringSubmatch(body, -1) {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(m[1])
		b.Write(key)
		b.WriteByte(':')
		value := strings.TrimSpace(m[2])
		isString := false
		if p, ok := schema.Properties[m[1]]; ok {
			isString = p.Type == "string"
		}
		if !isString && json.Valid([]byte(value)) {
			var compact bytes.Buffer
			json.Compact(&compact, []byte(value))
			b.Write(compact.Bytes())
		} else {
			v, _ := json.Marshal(m[2])
			b.Write(v)
		}
	}
	b.WriteByte('}')
	return b.String()
}
