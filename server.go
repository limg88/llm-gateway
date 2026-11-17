package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const maxBody = 64 << 20

// Server is the HTTP API in front of the backends.
type Server struct {
	cfg      Config
	backends map[string]Backend
	keys     []*apiKey
	sem      chan struct{} // one token per running backend request
	queued   atomic.Int64
	inflight atomic.Int64
	mcp      *mcpRegistry
	store    *conversationStore
	metrics  *Metrics
	started  time.Time
}

type apiKey struct {
	KeyConfig
	limiter *rateLimiter
	allowed map[string]bool // nil for every backend
}

var anonymous = &apiKey{KeyConfig: KeyConfig{Name: "anonymous"}}

func NewServer(cfg Config) *Server {
	s := &Server{
		cfg:      cfg,
		backends: map[string]Backend{},
		sem:      make(chan struct{}, cfg.MaxConcurrency),
		mcp:      newMCPRegistry(),
		store:    newConversationStore(time.Duration(cfg.ResponseTTL)*time.Second, cfg.ResponseMax),
		metrics:  NewMetrics(),
		started:  time.Now(),
	}
	for _, name := range cfg.Backends {
		switch name {
		case "claude":
			c := NewClaudeBackend(cfg.ClaudeBin, cfg.WorkDir, cfg.Warm, time.Duration(cfg.WarmIdle)*time.Second)
			c.Publish = s.PublishFunctions
			c.ExtraArgs = cfg.ClaudeExtraArgs
			c.WebFetch = cfg.ClaudeWebFetch
			if cfg.ClaudeIsolate != nil {
				c.Isolate = *cfg.ClaudeIsolate
			}
			s.backends[name] = c
			s.metrics.Gauge("llm_gateway_warm_processes", "Claude processes waiting idle for a request.", func() float64 {
				idle, _, _ := c.pool.stats()
				return float64(idle)
			})
		case "codex":
			c := &CodexBackend{Bin: cfg.CodexBin, WorkDir: cfg.WorkDir, ExtraArgs: cfg.CodexExtraArgs}
			if cfg.CodexIsolate == nil || *cfg.CodexIsolate {
				home, err := newCodexHome()
				if err != nil {
					// Never fall back to the unprotected mode silently.
					slog.Error("codex: cannot create the isolated home, the backend is disabled", "err", err)
					continue
				}
				c.Home = home
			} else {
				slog.Warn("codex runs with the user's own configuration: clients can make it read files and use its tools")
			}
			s.backends[name] = c
		}
	}
	for _, u := range cfg.Upstreams {
		s.backends[u.Name] = NewUpstreamBackend(u)
	}
	for _, k := range cfg.APIKeys {
		key := &apiKey{KeyConfig: k}
		if k.RPM > 0 {
			key.limiter = newRateLimiter(k.RPM)
		}
		if len(k.Backends) > 0 {
			key.allowed = map[string]bool{}
			for _, b := range k.Backends {
				key.allowed[b] = true
			}
		}
		s.keys = append(s.keys, key)
	}
	s.metrics.Gauge("llm_gateway_inflight_requests", "Requests being answered by a backend.", func() float64 { return float64(s.inflight.Load()) })
	s.metrics.Gauge("llm_gateway_queued_requests", "Requests waiting for a free slot.", func() float64 { return float64(s.queued.Load()) })
	s.metrics.Gauge("llm_gateway_stored_responses", "Conversations kept for previous_response_id.", func() float64 { return float64(s.store.len()) })
	s.metrics.Describe("llm_gateway_requests_total", "Requests by endpoint, backend and outcome.")
	s.metrics.Describe("llm_gateway_request_duration_seconds", "Time to answer a request, by backend.")
	s.metrics.Describe("llm_gateway_tokens_total", "Tokens reported by the backends.")
	s.metrics.Describe("llm_gateway_fallbacks_total", "Requests passed on to the next target of a model alias.")
	s.metrics.Describe("llm_gateway_rejected_total", "Requests refused before reaching a backend.")
	return s
}

func (s *Server) Close() {
	for _, b := range s.backends {
		b.Close()
	}
}

func (s *Server) backendNames() []string {
	return sortedKeys(s.backends)
}

// Handler routes the API. Every endpoint is served with and without /v1.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	metrics := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.metrics.Render(w)
	})
	if s.cfg.MetricsPublic != nil && !*s.cfg.MetricsPublic {
		mux.Handle("GET /metrics", s.auth(metrics))
	} else {
		mux.Handle("GET /metrics", metrics)
	}
	mux.HandleFunc("POST /mcp/{id}", s.handleMCP)
	for _, prefix := range []string{"", "/v1"} {
		mux.Handle("GET "+prefix+"/models", s.auth(http.HandlerFunc(s.handleModels)))
		mux.Handle("POST "+prefix+"/chat/completions", s.auth(http.HandlerFunc(s.handleChatCompletions)))
		mux.Handle("POST "+prefix+"/responses", s.auth(http.HandlerFunc(s.handleResponses)))
		mux.Handle("POST "+prefix+"/messages", s.auth(http.HandlerFunc(s.handleMessages)))
	}
	return logRequests(mux)
}

type keyCtx struct{}

func keyOf(r *http.Request) *apiKey {
	if k, ok := r.Context().Value(keyCtx{}).(*apiKey); ok {
		return k
	}
	return anonymous
}

// auth identifies the API key, from "Authorization: Bearer" or "x-api-key",
// and applies its rate limit. Without configured keys everyone is let in.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := anonymous
		if len(s.keys) > 0 {
			given := r.Header.Get("x-api-key")
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				given = strings.TrimPrefix(h, "Bearer ")
			}
			key = nil
			for _, k := range s.keys {
				// Every key is compared, in constant time.
				if subtle.ConstantTimeCompare([]byte(given), []byte(k.Key)) == 1 {
					key = k
				}
			}
			if key == nil {
				s.metrics.Inc("llm_gateway_rejected_total", "reason", "unauthorized")
				writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid API key")
				return
			}
		}
		if key.limiter != nil {
			if ok, wait := key.limiter.allow(); !ok {
				s.metrics.Inc("llm_gateway_rejected_total", "reason", "rate_limited")
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				writeError(w, http.StatusTooManyRequests, "rate_limit_error", fmt.Sprintf("rate limit of %d requests per minute exceeded", key.RPM))
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyCtx{}, key)))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic", "path", r.URL.Path, "err", v)
				if sw.status == 0 {
					writeError(sw, http.StatusInternalServerError, "server_error", "internal error")
				}
			}
			level := slog.LevelInfo
			if r.URL.Path == "/health" || r.URL.Path == "/metrics" || strings.HasPrefix(r.URL.Path, "/mcp/") {
				level = slog.LevelDebug
			}
			slog.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
				"duration", time.Since(start).Round(time.Millisecond))
		}()
		next.ServeHTTP(sw, r)
	})
}

// ---- models ----

// target is one backend and model a request can be answered by.
type target struct {
	backend Backend
	model   string // as the backend knows it, possibly empty for its default
	label   string // backend/model
}

type apiErr struct {
	status int
	typ    string
	msg    string
}

func badRequest(format string, args ...any) *apiErr {
	return &apiErr{http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(format, args...)}
}

// resolveModel turns a model name into the targets to try, in order: the
// name of a backend, backend/model, or an alias of the configuration.
func (s *Server) resolveModel(name string, key *apiKey) ([]target, *apiErr) {
	names := []string{name}
	if alias, ok := s.cfg.Models[name]; ok {
		names = alias
	}
	var targets []target
	denied := false
	for _, n := range names {
		backendName, model, _ := strings.Cut(n, "/")
		b := s.backends[backendName]
		if b == nil {
			if len(names) == 1 {
				return nil, badRequest("model %q not supported: use %s, optionally followed by /<model>%s",
					name, strings.Join(s.backendNames(), ", "), s.aliasHint())
			}
			slog.Warn("model alias names an unknown backend", "alias", name, "target", n)
			continue
		}
		if key.allowed != nil && !key.allowed[backendName] {
			denied = true
			continue
		}
		targets = append(targets, target{backend: b, model: model, label: n})
	}
	if len(targets) == 0 {
		if denied {
			return nil, &apiErr{http.StatusForbidden, "permission_error", fmt.Sprintf("this API key cannot use model %q", name)}
		}
		return nil, badRequest("model %q has no usable target", name)
	}
	return targets, nil
}

func (s *Server) aliasHint() string {
	if len(s.cfg.Models) == 0 {
		return ""
	}
	return ", or one of the aliases " + strings.Join(sortedKeys(s.cfg.Models), ", ")
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	key := keyOf(r)
	created := s.started.Unix()
	list := ModelList{Object: "list", Data: []Model{}}
	for _, name := range s.backendNames() {
		if key.allowed != nil && !key.allowed[name] {
			continue
		}
		owner := name
		switch name {
		case "claude":
			owner = "anthropic"
		case "codex":
			owner = "openai"
		}
		b := s.backends[name]
		if _, upstream := b.(*UpstreamBackend); !upstream {
			list.Data = append(list.Data, Model{ID: name, Object: "model", Created: created, OwnedBy: owner})
		}
		for _, m := range b.Models() {
			list.Data = append(list.Data, Model{ID: name + "/" + m, Object: "model", Created: created, OwnedBy: owner})
		}
	}
	for _, alias := range sortedKeys(s.cfg.Models) {
		if _, err := s.resolveModel(alias, key); err == nil {
			list.Data = append(list.Data, Model{ID: alias, Object: "model", Created: created, OwnedBy: "llm-gateway"})
		}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	backends := map[string]any{}
	for name, b := range s.backends {
		st := b.Status()
		if bin, ok := st["bin"].(string); ok {
			_, err := exec.LookPath(bin)
			st["available"] = err == nil
		}
		backends[name] = st
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"version":          version,
		"uptime_seconds":   int(time.Since(s.started).Seconds()),
		"inflight":         s.inflight.Load(),
		"queued":           s.queued.Load(),
		"max_concurrency":  s.cfg.MaxConcurrency,
		"stored_responses": s.store.len(),
		"backends":         backends,
	})
}

// ---- running a request ----

// prepared is a target with the prompt built for it.
type prepared struct {
	target
	prompt   Prompt
	emulated bool
}

// job is a request ready to run.
type job struct {
	endpoint string
	model    string // as the client named it
	stream   bool
	targets  []prepared
	used     string // label of the target that answered; set when the events end
	start    time.Time
}

// prepare validates a request and builds its prompt for every target. An
// error on the first target is the client's; a later target that cannot
// take the request (say, its images) is left out.
func (s *Server) prepare(endpoint string, req ChatCompletionRequest, key *apiKey) (*job, *apiErr) {
	if req.Model == "" {
		return nil, badRequest("model is required")
	}
	targets, aerr := s.resolveModel(req.Model, key)
	if aerr != nil {
		return nil, aerr
	}
	base, err := BuildPrompt(req.Messages)
	if err != nil {
		return nil, badRequest("%s", err)
	}
	if err := checkImages(base.Images); err != nil {
		return nil, badRequest("%s", err)
	}
	base.Effort = req.ReasoningEffort
	if ws := bytes.TrimSpace(req.WebSearchOptions); len(ws) > 0 && !bytes.Equal(ws, []byte("null")) {
		base.WebSearch = true
	}
	j := &job{endpoint: endpoint, model: req.Model, stream: req.Stream, start: time.Now()}
	for i, t := range targets {
		p := base
		emulated, err := applyTools(&p, req, t.backend.NativeFunctions())
		if err == nil {
			err = t.backend.CheckImages(p.Images)
		}
		if err == nil && !emulated && req.ResponseFormat != nil {
			err = applyFormat(&p, req.ResponseFormat)
		}
		if err != nil {
			if i == 0 {
				return nil, badRequest("%s", err)
			}
			slog.Warn("fallback target left out", "target", t.label, "err", err)
			continue
		}
		j.targets = append(j.targets, prepared{target: t, prompt: p, emulated: emulated})
	}
	slog.Info("completion", "endpoint", endpoint, "key", key.Name, "model", req.Model, "targets", len(j.targets),
		"stream", req.Stream, "functions", len(req.Tools), "emulated", j.targets[0].emulated,
		"web_search", base.WebSearch, "effort", req.ReasoningEffort, "images", len(base.Images))
	return j, nil
}

func applyFormat(p *Prompt, f *ResponseFormat) error {
	switch f.Type {
	case "", "text":
	case "json_object":
		if p.System != "" {
			p.System += "\n\n"
		}
		p.System += "Reply with a single valid JSON object and nothing else: no prose, no code fences."
	case "json_schema":
		if f.JSONSchema == nil || len(bytes.TrimSpace(f.JSONSchema.Schema)) == 0 {
			return errors.New("response_format.json_schema.schema is required")
		}
		p.Schema = f.JSONSchema.Schema
	default:
		return fmt.Errorf("response_format type %q is not supported", f.Type)
	}
	return nil
}

// run waits for a free slot and starts the job. The events must be read to
// the end: the slot is released when they are.
func (s *Server) run(ctx context.Context, j *job) (<-chan Event, *apiErr) {
	select {
	case s.sem <- struct{}{}:
	default:
		if int(s.queued.Load()) >= s.cfg.MaxQueue {
			s.metrics.Inc("llm_gateway_rejected_total", "reason", "queue_full")
			return nil, &apiErr{http.StatusServiceUnavailable, "overloaded_error", "the server is at capacity, retry later"}
		}
		s.queued.Add(1)
		select {
		case s.sem <- struct{}{}:
			s.queued.Add(-1)
		case <-ctx.Done():
			s.queued.Add(-1)
			s.metrics.Inc("llm_gateway_rejected_total", "reason", "queue_timeout")
			return nil, &apiErr{http.StatusServiceUnavailable, "overloaded_error", "timed out waiting for a free slot, retry later"}
		}
	}
	s.inflight.Add(1)
	out := make(chan Event, 16)
	go func() {
		defer close(out)
		defer func() {
			s.inflight.Add(-1)
			<-s.sem
		}()
		for i, t := range j.targets {
			j.used = t.label
			last := i == len(j.targets)-1
			err := forward(ctx, t.backend.Run(ctx, t.model, t.prompt), out, t.emulated, !last)
			if err == nil {
				return
			}
			s.metrics.Inc("llm_gateway_fallbacks_total", "from", t.backend.Name())
			slog.Warn("backend failed, trying the next target", "target", t.label, "err", err)
		}
	}()
	return out, nil
}

// forward copies a backend's events to out, reading them to the end. With
// emulated functions the reply is a JSON object, turned here into text and
// calls. If the backend fails before anything was sent and fallback is set,
// the error is returned instead of sent, for the next target to be tried.
func forward(ctx context.Context, in <-chan Event, out chan<- Event, emulated, fallback bool) error {
	var (
		sent   bool
		failed error
		buf    strings.Builder
		usage  *Usage
	)
	for ev := range in {
		if failed != nil {
			continue // drain
		}
		switch {
		case ev.Err != nil:
			failed = ev.Err
		case ev.Usage != nil:
			usage = ev.Usage
		case emulated && ev.Text != "":
			buf.WriteString(ev.Text)
		default:
			sent = true
			out <- ev
		}
	}
	if failed == nil && emulated {
		content, calls := parseToolOutput(buf.String())
		if content != "" {
			sent = true
			out <- Event{Text: content}
		}
		for i, c := range calls {
			sent = true
			out <- Event{Call: &CallEvent{Index: i, ID: c.ID, Name: c.Function.Name, Args: c.Function.Arguments, Done: true}}
		}
	}
	if failed == nil && !sent {
		failed = errNoOutput
	}
	if failed != nil {
		if fallback && !sent && ctx.Err() == nil {
			return failed
		}
		out <- Event{Err: failed}
		return nil
	}
	if usage != nil {
		out <- Event{Usage: usage}
	}
	return nil
}

// answer gathers the events of a reply.
type answer struct {
	text     strings.Builder
	calls    []ToolCall
	usage    *Usage
	searches []SearchEvent // in the order they started
	err      error
}

func (a *answer) add(ev Event) {
	switch {
	case ev.Err != nil:
		a.err = ev.Err
	case ev.Usage != nil:
		a.usage = ev.Usage
	case ev.Call != nil:
		c := ev.Call
		for len(a.calls) <= c.Index {
			a.calls = append(a.calls, ToolCall{Type: "function"})
		}
		tc := &a.calls[c.Index]
		if c.ID != "" {
			tc.ID = c.ID
		}
		if c.Name != "" {
			tc.Function.Name = c.Name
		}
		tc.Function.Arguments += c.Args
		if c.Done && strings.TrimSpace(tc.Function.Arguments) == "" {
			tc.Function.Arguments = "{}"
		}
	case ev.Search != nil:
		for i := range a.searches {
			if a.searches[i].ID == ev.Search.ID {
				a.searches[i].Done = ev.Search.Done
				a.searches[i].Sources = ev.Search.Sources
				if ev.Search.Action != nil {
					a.searches[i].Action = ev.Search.Action
				}
				return
			}
		}
		a.searches = append(a.searches, *ev.Search)
	default:
		a.text.WriteString(ev.Text)
	}
}

// message is the reply as an assistant message of the conversation.
func (a *answer) message() ChatMessage {
	m := ChatMessage{Role: "assistant", ToolCalls: a.calls}
	if a.text.Len() > 0 || len(a.calls) == 0 {
		m.Content, _ = json.Marshal(a.text.String())
	}
	return m
}

func drain(events <-chan Event) {
	for range events {
	}
}

// observe records the outcome of a job in the metrics and the log.
func (s *Server) observe(j *job, a *answer) {
	backend, _, _ := strings.Cut(j.used, "/")
	status := "ok"
	if a.err != nil {
		status = "error"
		if errors.Is(a.err, context.DeadlineExceeded) {
			status = "timeout"
		} else if errors.Is(a.err, context.Canceled) {
			status = "canceled"
		}
	}
	elapsed := time.Since(j.start)
	s.metrics.Inc("llm_gateway_requests_total", "endpoint", j.endpoint, "backend", backend, "status", status)
	s.metrics.Observe("llm_gateway_request_duration_seconds", elapsed.Seconds(), "backend", backend)
	attrs := []any{"endpoint", j.endpoint, "target", j.used, "status", status, "duration", elapsed.Round(time.Millisecond),
		"calls", len(a.calls), "searches", len(a.searches)}
	if a.usage != nil {
		s.metrics.Add("llm_gateway_tokens_total", float64(a.usage.PromptTokens), "backend", backend, "kind", "input")
		s.metrics.Add("llm_gateway_tokens_total", float64(a.usage.CompletionTokens), "backend", backend, "kind", "output")
		attrs = append(attrs, "input_tokens", a.usage.PromptTokens, "output_tokens", a.usage.CompletionTokens)
	}
	if a.err != nil {
		slog.Error("completion failed", append(attrs, "err", a.err)...)
		return
	}
	slog.Info("completion done", attrs...)
}

// decodeBody decodes the body of a request into v.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) *apiErr {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return &apiErr{http.StatusRequestEntityTooLarge, "invalid_request_error", "the request body is too large"}
		}
		return badRequest("cannot read the request body")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return badRequest("invalid JSON body: %s", err)
	}
	return nil
}

// ---- chat completions ----

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req ChatCompletionRequest
	if aerr := decodeBody(w, r, &req); aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	j, aerr := s.prepare("chat", req, keyOf(r))
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.RequestTimeout)*time.Second)
	defer cancel()
	events, aerr := s.run(ctx, j)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	defer drain(events)

	id := "chatcmpl-" + randomID()
	created := time.Now().Unix()
	var a answer
	defer func() {
		drain(events)
		s.observe(j, &a)
	}()

	if req.Stream {
		s.streamChat(w, events, &a, id, created, req)
		return
	}
	for ev := range events {
		a.add(ev)
	}
	if a.err != nil {
		writeBackendError(w, a.err)
		return
	}
	msg := ResponseMessage{Role: "assistant", ToolCalls: a.calls}
	finish := "stop"
	if len(a.calls) > 0 {
		finish = "tool_calls"
	}
	if text := a.text.String(); text != "" || len(a.calls) == 0 {
		msg.Content = &text
		msg.Annotations = chatAnnotations(citations(text, searchSources(a.searches)))
	}
	writeJSON(w, http.StatusOK, ChatCompletionResponse{
		ID: id, Object: "chat.completion", Created: created, Model: req.Model,
		Choices: []Choice{{Message: msg, FinishReason: finish}},
		Usage:   a.usage,
	})
}

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func startSSE(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	s := &sseWriter{w: w, flusher: f}
	s.flush()
	return s
}

func (s *sseWriter) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// send writes one event; event is the SSE event name, empty for none.
func (s *sseWriter) send(event string, v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	if event != "" {
		fmt.Fprintf(s.w, "event: %s\n", event)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", bytes.TrimSpace(b.Bytes()))
	s.flush()
}

func (s *sseWriter) done() {
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	s.flush()
}

func (s *Server) streamChat(w http.ResponseWriter, events <-chan Event, a *answer, id string, created int64, req ChatCompletionRequest) {
	sse := startSSE(w)
	chunk := func(d Delta, finish *string) {
		sse.send("", ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []ChunkChoice{{Delta: d, FinishReason: finish}},
		})
	}
	chunk(Delta{Role: "assistant"}, nil)
	for ev := range events {
		a.add(ev)
		switch {
		case ev.Err != nil:
			slog.Error("backend error during stream", "err", ev.Err)
			sse.send("", APIError{Error: APIErrorBody{Message: backendMessage(ev.Err), Type: "backend_error"}})
			sse.done()
			return
		case ev.Text != "":
			chunk(Delta{Content: ev.Text}, nil)
		case ev.Call != nil:
			c := ev.Call
			if c.ID == "" && c.Name == "" && c.Args == "" {
				continue
			}
			idx := c.Index
			tc := ToolCall{Index: &idx, ID: c.ID, Function: FunctionCall{Name: c.Name, Arguments: c.Args}}
			if c.ID != "" {
				tc.Type = "function"
			}
			chunk(Delta{ToolCalls: []ToolCall{tc}}, nil)
		}
	}
	if anns := chatAnnotations(citations(a.text.String(), searchSources(a.searches))); len(anns) > 0 {
		chunk(Delta{Annotations: anns}, nil)
	}
	finish := "stop"
	if len(a.calls) > 0 {
		finish = "tool_calls"
	}
	chunk(Delta{}, &finish)
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage && a.usage != nil {
		sse.send("", ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []ChunkChoice{}, Usage: a.usage,
		})
	}
	sse.done()
}

// ---- helpers ----

func backendMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "backend timed out"
	}
	return err.Error()
}

func writeBackendError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, "timeout_error", "backend timed out")
		return
	}
	writeError(w, http.StatusBadGateway, "backend_error", err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, APIError{Error: APIErrorBody{Message: msg, Type: typ}})
}

func writeAPIErr(w http.ResponseWriter, e *apiErr) {
	writeError(w, e.status, e.typ, e.msg)
}
