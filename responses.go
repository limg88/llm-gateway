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

// chatRequest converts a Responses request to the chat form every backend
// path shares. sources reports whether the client asked for the sources of
// the web searches.
func (req ResponsesRequest) chatRequest() (chat ChatCompletionRequest, sources bool, aerr *apiErr) {
	chat = ChatCompletionRequest{
		Model:             req.Model,
		Stream:            req.Stream,
		ToolChoice:        req.ToolChoice,
		ParallelToolCalls: req.ParallelToolCalls,
	}
	if _, aerr := outputBudget("max_output_tokens", req.MaxOutputTokens); aerr != nil {
		return chat, false, aerr
	}
	chat.MaxTokens = req.MaxOutputTokens
	if req.Reasoning != nil {
		chat.ReasoningEffort = req.Reasoning.Effort
	}
	for _, inc := range req.Include {
		if inc == "web_search_call.action.sources" {
			sources = true
		}
	}
	if req.Instructions != "" {
		chat.Messages = append(chat.Messages, ChatMessage{Role: "system", Content: jsonString(req.Instructions)})
	}

	input := bytes.TrimSpace(req.Input)
	switch {
	case len(input) > 0 && input[0] == '"':
		chat.Messages = append(chat.Messages, ChatMessage{Role: "user", Content: input})
	case len(input) > 0 && input[0] == '[':
		var items []InputItem
		if err := json.Unmarshal(input, &items); err != nil {
			return chat, false, badRequest("input must be a string or an array of items")
		}
		for _, it := range items {
			switch it.Type {
			case "", "message":
				role := it.Role
				if role == "" {
					role = "user"
				}
				chat.Messages = append(chat.Messages, ChatMessage{Role: role, Content: it.Content})
			case "function_call":
				call := ToolCall{ID: it.CallID, Type: "function", Function: FunctionCall{Name: it.Name, Arguments: it.Arguments}}
				// Consecutive calls belong to one assistant turn.
				if n := len(chat.Messages); n > 0 && chat.Messages[n-1].Role == "assistant" && len(chat.Messages[n-1].ToolCalls) > 0 {
					chat.Messages[n-1].ToolCalls = append(chat.Messages[n-1].ToolCalls, call)
				} else {
					chat.Messages = append(chat.Messages, ChatMessage{Role: "assistant", ToolCalls: []ToolCall{call}})
				}
			case "function_call_output":
				chat.Messages = append(chat.Messages, ChatMessage{Role: "tool", ToolCallID: it.CallID, Content: it.Output})
			case "reasoning", "web_search_call":
				// Items of an earlier response sent back: nothing to replay.
			default:
				return chat, false, badRequest("input item type %q is not supported", it.Type)
			}
		}
	default:
		return chat, false, badRequest("input must be a string or an array of items")
	}

	for _, t := range req.Tools {
		switch {
		case t.Type == "function":
			chat.Tools = append(chat.Tools, Tool{Type: "function", Function: FunctionDefinition{
				Name: t.Name, Description: t.Description, Parameters: t.Parameters,
			}})
		case strings.HasPrefix(t.Type, "web_search"):
			chat.WebSearchOptions = json.RawMessage("{}")
		default:
			return chat, false, badRequest("tool type %q is not supported", t.Type)
		}
	}
	if req.Text != nil && req.Text.Format != nil {
		f := req.Text.Format
		chat.ResponseFormat = &ResponseFormat{Type: f.Type}
		if f.Type == "json_schema" {
			chat.ResponseFormat.JSONSchema = &struct {
				Name   string          `json:"name"`
				Schema json.RawMessage `json:"schema"`
			}{Name: f.Name, Schema: f.Schema}
		}
	}
	return chat, sources, nil
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request, trace requestTrace) {
	var req ResponsesRequest
	if aerr := decodeBody(w, r, &req); aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	key := keyOf(r)
	chat, sources, aerr := req.chatRequest()
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	if req.PreviousResponseID != "" {
		prev, ok := s.store.get(req.PreviousResponseID, key.Name)
		if !ok {
			writeAPIErr(w, badRequest("previous response %q not found: it expired, was not stored, or belongs to another key", req.PreviousResponseID))
			return
		}
		// The instructions of this request replace the earlier ones.
		var msgs []ChatMessage
		for _, m := range prev {
			if m.Role != "system" || req.Instructions == "" {
				msgs = append(msgs, m)
			}
		}
		var system, rest []ChatMessage
		for _, m := range chat.Messages {
			if m.Role == "system" {
				system = append(system, m)
			} else {
				rest = append(rest, m)
			}
		}
		chat.Messages = append(append(system, msgs...), rest...)
	}
	j, aerr := s.prepare("responses", chat, key)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.RequestTimeout)*time.Second)
	defer cancel()
	events, aerr := s.run(ctx, j, trace)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	resp := &Response{
		ID: "resp_" + randomID(), Object: "response", CreatedAt: time.Now().Unix(),
		Status: "in_progress", Model: req.Model, Output: []OutputItem{},
		PreviousResponseID: req.PreviousResponseID,
	}
	var a answer
	defer func() {
		drain(events)
		s.observe(j, &a)
		if a.err == nil && (req.Store == nil || *req.Store) {
			s.store.put(resp.ID, key.Name, append(chat.Messages, a.message()))
		}
	}()

	if req.Stream {
		first, err := early(ctx, events, &a)
		if err != nil {
			writeAPIErr(w, publicError(err))
			return
		}
		s.streamResponse(w, replay(first, events), &a, resp, sources)
		return
	}
	for ev := range events {
		a.add(ev)
	}
	if a.err != nil {
		writeAPIErr(w, publicError(a.err))
		return
	}
	resp.end(&a)
	for _, ws := range a.searches {
		ws.Done = true
		resp.Output = append(resp.Output, searchItem(ws, sources))
	}
	if text := a.text.String(); text != "" || len(a.calls) == 0 {
		item := messageItem(text, resp.Status)
		(*item.Content)[0].Annotations = annotationsOf(text, a.searches)
		resp.Output = append(resp.Output, item)
	}
	for _, tc := range a.calls {
		resp.Output = append(resp.Output, functionCallItem(tc, resp.Status))
	}
	writeJSON(w, http.StatusOK, resp)
}

// end sets the final status of a response from the way its answer ended:
// an answer cut short is "incomplete", with the reason.
func (resp *Response) end(a *answer) {
	resp.Status = "completed"
	switch a.reason() {
	case finishLength:
		resp.Status = "incomplete"
		resp.IncompleteDetails = &IncompleteDetails{Reason: "max_output_tokens"}
	case finishFilter:
		resp.Status = "incomplete"
		resp.IncompleteDetails = &IncompleteDetails{Reason: "content_filter"}
	}
	resp.Usage = responsesUsage(a.usage)
}

func annotationsOf(text string, searches []SearchEvent) []URLCitation {
	anns := citations(text, searchSources(searches))
	if anns == nil {
		anns = []URLCitation{}
	}
	return anns
}

func responsesUsage(u *Usage) *ResponsesUsage {
	if u == nil {
		return nil
	}
	return &ResponsesUsage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
}

func messageItem(text, status string) OutputItem {
	content := []OutputText{{Type: "output_text", Text: text, Annotations: []URLCitation{}}}
	return OutputItem{Type: "message", ID: "msg_" + randomID(), Status: status, Role: "assistant", Content: &content}
}

func searchItem(ws SearchEvent, sources bool) OutputItem {
	status := "in_progress"
	if ws.Done {
		status = "completed"
	}
	action := &WebSearchAction{Type: "search"}
	if ws.Action != nil {
		copied := *ws.Action
		action = &copied
	}
	if sources && ws.Done {
		for _, src := range ws.Sources {
			action.Sources = append(action.Sources, WebSearchSource{Type: "url", URL: src.URL})
		}
	}
	return OutputItem{Type: "web_search_call", ID: "ws_" + searchID(ws.ID), Status: status, Action: action}
}

// searchID keeps the ids of the search items in a safe alphabet.
func searchID(id string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, id)
}

func functionCallItem(tc ToolCall, status string) OutputItem {
	args := tc.Function.Arguments
	return OutputItem{Type: "function_call", ID: "fc_" + randomID(), Status: status, CallID: tc.ID, Name: tc.Function.Name, Arguments: &args}
}

// responseSSE writes the event stream of a response. Output items are
// opened as the backend's events arrive and closed, in order, when another
// kind of item begins or the response ends.
type responseSSE struct {
	sse     *sseWriter
	resp    *Response
	seq     int
	sources bool

	msg    *OutputItem // open message item
	msgIdx int
	buf    strings.Builder

	ws map[string]int // search id -> output index of the open search item

	status string // of the items closed from now on: completed or incomplete

	fc     map[int]*OutputItem // call index -> open function call item
	fcIdx  map[int]int
	fcArgs map[int]*strings.Builder
}

func (r *responseSSE) send(typ string, fields map[string]any) {
	fields["type"] = typ
	fields["sequence_number"] = r.seq
	r.seq++
	r.sse.send(typ, fields)
}

// add appends an item to the output and announces it.
func (r *responseSSE) add(item OutputItem) int {
	idx := len(r.resp.Output)
	r.resp.Output = append(r.resp.Output, item)
	r.send("response.output_item.added", map[string]any{"output_index": idx, "item": item})
	return idx
}

func (r *responseSSE) done(idx int, item OutputItem) {
	r.resp.Output[idx] = item
	r.send("response.output_item.done", map[string]any{"output_index": idx, "item": item})
}

func (r *responseSSE) search(ev *SearchEvent, all []SearchEvent) {
	idx, open := r.ws[ev.ID]
	if !open {
		r.endText(nil)
		started := *ev
		started.Done = false
		item := searchItem(started, r.sources)
		idx = r.add(item)
		r.ws[ev.ID] = idx
		r.send("response.web_search_call.in_progress", map[string]any{"output_index": idx, "item_id": item.ID})
		r.send("response.web_search_call.searching", map[string]any{"output_index": idx, "item_id": item.ID})
	}
	if !ev.Done {
		return
	}
	// The gathered event has the action of the start and the sources of the end.
	full := *ev
	for _, s := range all {
		if s.ID == ev.ID {
			full = s
		}
	}
	full.Done = true
	item := searchItem(full, r.sources)
	delete(r.ws, ev.ID)
	r.send("response.web_search_call.completed", map[string]any{"output_index": idx, "item_id": item.ID})
	r.done(idx, item)
}

func (r *responseSSE) text(delta string) {
	if r.msg == nil {
		item := messageItem("", "in_progress")
		empty := []OutputText{}
		shown := item
		shown.Content = &empty
		r.msgIdx = r.add(shown)
		r.msg = &item
		r.buf.Reset()
		r.send("response.content_part.added", map[string]any{
			"item_id": item.ID, "output_index": r.msgIdx, "content_index": 0, "part": (*item.Content)[0],
		})
	}
	r.buf.WriteString(delta)
	r.send("response.output_text.delta", map[string]any{
		"item_id": r.msg.ID, "output_index": r.msgIdx, "content_index": 0, "delta": delta,
	})
}

// endText closes the open message item, if any.
func (r *responseSSE) endText(searches []SearchEvent) {
	if r.msg == nil {
		return
	}
	item := *r.msg
	text := r.buf.String()
	part := OutputText{Type: "output_text", Text: text, Annotations: annotationsOf(text, searches)}
	for i, ann := range part.Annotations {
		r.send("response.output_text.annotation.added", map[string]any{
			"item_id": item.ID, "output_index": r.msgIdx, "content_index": 0, "annotation_index": i, "annotation": ann,
		})
	}
	r.send("response.output_text.done", map[string]any{
		"item_id": item.ID, "output_index": r.msgIdx, "content_index": 0, "text": text,
	})
	r.send("response.content_part.done", map[string]any{
		"item_id": item.ID, "output_index": r.msgIdx, "content_index": 0, "part": part,
	})
	item.Status = r.status
	item.Content = &[]OutputText{part}
	r.done(r.msgIdx, item)
	r.msg = nil
}

func (r *responseSSE) call(c *CallEvent) {
	item := r.fc[c.Index]
	if item == nil {
		r.endText(nil)
		it := functionCallItem(ToolCall{ID: c.ID, Function: FunctionCall{Name: c.Name}}, "in_progress")
		item = &it
		r.fc[c.Index] = item
		r.fcArgs[c.Index] = &strings.Builder{}
		r.fcIdx[c.Index] = r.add(it)
	}
	idx := r.fcIdx[c.Index]
	if c.Args != "" {
		r.fcArgs[c.Index].WriteString(c.Args)
		r.send("response.function_call_arguments.delta", map[string]any{"item_id": item.ID, "output_index": idx, "delta": c.Args})
	}
	if c.Done {
		r.endCall(c.Index)
	}
}

func (r *responseSSE) endCall(index int) {
	item := r.fc[index]
	if item == nil {
		return
	}
	idx := r.fcIdx[index]
	args := r.fcArgs[index].String()
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	r.send("response.function_call_arguments.done", map[string]any{"item_id": item.ID, "output_index": idx, "arguments": args})
	done := *item
	done.Status = r.status
	done.Arguments = &args
	r.done(idx, done)
	delete(r.fc, index)
}

func (s *Server) streamResponse(w http.ResponseWriter, events iter.Seq[Event], a *answer, resp *Response, sources bool) {
	r := &responseSSE{
		sse: startSSE(w), resp: resp, sources: sources, status: "completed",
		ws: map[string]int{}, fc: map[int]*OutputItem{}, fcIdx: map[int]int{}, fcArgs: map[int]*strings.Builder{},
	}
	r.send("response.created", map[string]any{"response": resp})
	r.send("response.in_progress", map[string]any{"response": resp})
	for ev := range events {
		a.add(ev)
		switch {
		case ev.Err != nil:
			e := publicError(ev.Err)
			resp.Status = "failed"
			resp.Error = &ResponseError{Code: e.code, Message: e.msg, RetryAfter: e.retrySeconds()}
			r.send("response.failed", map[string]any{"response": resp})
			return
		case ev.Search != nil:
			r.search(ev.Search, a.searches)
		case ev.Call != nil:
			r.call(ev.Call)
		case ev.Text != "":
			r.text(ev.Text)
		}
	}
	resp.end(a)
	// Close whatever the backend left open. What was open when the answer
	// was cut short is incomplete, like the response.
	final := resp.Status
	for id, idx := range r.ws {
		for _, ws := range a.searches {
			if ws.ID == id {
				ws.Done = true
				item := searchItem(ws, sources)
				r.send("response.web_search_call.completed", map[string]any{"output_index": idx, "item_id": item.ID})
				r.done(idx, item)
			}
		}
	}
	r.status = final
	if len(resp.Output) == 0 {
		r.text("")
	}
	r.endText(a.searches)
	for index := range r.fc {
		r.endCall(index)
	}
	r.send("response."+final, map[string]any{"response": resp})
}
