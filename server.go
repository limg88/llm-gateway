package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxBody = 64 << 20

// Server is the HTTP API in front of the backends.
type Server struct {
	ledgerLock   *os.File
	cache        responseCache
	budgetDay    string
	spent        map[string]float64
	ledgerFailed bool
	ledgerMu     sync.Mutex
	cfg          Config
	backends     map[string]Backend
	keys         []*apiKey
	sem          chan struct{} // one token per running backend request
	queued       atomic.Int64
	inflight     atomic.Int64
	mcp          *mcpRegistry
	store        *conversationStore
	metrics      *Metrics
	started      time.Time
}

type apiKey struct {
	digest [32]byte
	KeyConfig
	limiter *rateLimiter
	allowed map[string]bool // nil for every backend
}

var anonymous = &apiKey{KeyConfig: KeyConfig{Name: "anonymous", Project: "personal"}}

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
			c := &CodexBackend{Bin: cfg.CodexBin, WorkDir: cfg.WorkDir, ExtraArgs: cfg.CodexExtraArgs, WebSearch: cfg.CodexWebSearch}
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
		key := &apiKey{KeyConfig: k, digest: sha256.Sum256([]byte(k.Key))}
		key.Key = ""
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
	if s.ledgerLock != nil {
		defer s.ledgerLock.Close()
	}
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
		mux.Handle("POST "+prefix+"/embeddings", s.auth(s.native("embeddings", nil)))
		if s.cfg.Ledger != "" {
			mux.Handle("GET "+prefix+"/usage", s.auth(http.HandlerFunc(s.handleUsage)))
		}
		mux.Handle("GET "+prefix+"/models", s.auth(http.HandlerFunc(s.handleModels)))
		mux.Handle("POST "+prefix+"/chat/completions", s.auth(s.native("chat/completions", s.handleChatCompletions)))
		mux.Handle("POST "+prefix+"/responses", s.auth(s.native("responses", s.handleResponses)))
		mux.Handle("POST "+prefix+"/messages", s.auth(s.native("messages", s.handleMessages)))
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
		writeError := writeAPIErr
		if strings.HasSuffix(r.URL.Path, "/messages") {
			writeError = writeAnthropicAPIErr
		}
		key := anonymous
		if len(s.keys) > 0 {
			given := r.Header.Get("x-api-key")
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				given = strings.TrimPrefix(h, "Bearer ")
			}
			key = nil
			digest := sha256.Sum256([]byte(given))
			for _, k := range s.keys {
				// Every key is compared, in constant time.
				if subtle.ConstantTimeCompare(digest[:], k.digest[:]) == 1 {
					key = k
				}
			}
			if key == nil {
				s.metrics.Inc("llm_gateway_rejected_total", "reason", "unauthorized")
				writeError(w, &apiErr{status: http.StatusUnauthorized, typ: "authentication_error", code: "authentication_error", msg: "invalid API key"})
				return
			}
		}
		if key.limiter != nil {
			if ok, wait := key.limiter.allow(); !ok {
				s.metrics.Inc("llm_gateway_rejected_total", "reason", "rate_limited")
				writeError(w, &apiErr{status: http.StatusTooManyRequests, typ: "rate_limit_error", code: "gateway_rate_limit",
					msg: fmt.Sprintf("rate limit of %d requests per minute exceeded", key.RPM), retryAfter: wait})
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
				slog.Error("request panic")
				if sw.status == 0 {
					writeAPIErr(sw, &apiErr{status: http.StatusInternalServerError, typ: "server_error", code: "internal_error", msg: "internal error"})
				}
			}
			level := slog.LevelInfo
			if r.URL.Path == "/health" || r.URL.Path == "/metrics" || strings.HasPrefix(r.URL.Path, "/mcp/") {
				level = slog.LevelDebug
			}
			slog.Log(r.Context(), level, "request", "method", r.Method, "route", r.Pattern, "status", sw.status,
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

// resolveModel turns a model name into the targets to try, in order: the
// name of a backend, backend/model, or an alias of the configuration.
func (s *Server) resolveModel(name string, key *apiKey) ([]target, *apiErr) {
	names := []string{name}
	if alias, ok := s.cfg.Models[name]; ok {
		names = alias
	}
	if routes, ok := key.Routes[name]; ok {
		names = routes
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
		if !key.permits(b) {
			denied = true
			continue
		}
		targets = append(targets, target{backend: b, model: model, label: n})
	}
	if len(targets) == 0 {
		if denied {
			return nil, &apiErr{status: http.StatusForbidden, typ: "permission_error", code: "permission_denied", msg: fmt.Sprintf("this API key cannot use model %q", name)}
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
		if !key.permits(s.backends[name]) {
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
	aliases := map[string]bool{}
	for a := range s.cfg.Models {
		aliases[a] = true
	}
	for a := range key.Routes {
		aliases[a] = true
	}
	for _, alias := range sortedKeys(aliases) {
		if _, err := s.resolveModel(alias, key); err == nil {
			list.Data = append(list.Data, Model{ID: alias, Object: "model", Created: created, OwnedBy: "llm-gateway"})
		}
	}
	for i, m := range list.Data {
		list.Data[i].Discovery = s.cfg.Discovery[m.ID]
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
	project  string
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
	if base.MaxTokens, base.MaxTokensField, aerr = req.budget(); aerr != nil {
		return nil, aerr
	}
	if ws := bytes.TrimSpace(req.WebSearchOptions); len(ws) > 0 && !bytes.Equal(ws, []byte("null")) {
		base.WebSearch = true
	}
	j := &job{project: key.Project, endpoint: endpoint, model: req.Model, stream: req.Stream, start: time.Now()}
	for i, t := range targets {
		p := base
		emulated, err := applyTools(&p, req, t.backend.NativeFunctions())
		if err == nil {
			err = t.backend.CheckImages(p.Images)
		}
		if checker, ok := t.backend.(interface{ CheckPrompt(Prompt) error }); ok && err == nil {
			err = checker.CheckPrompt(p)
		}
		if err == nil && !emulated && req.ResponseFormat != nil {
			err = applyFormat(&p, req.ResponseFormat)
		}
		if err != nil {
			if i == 0 {
				return nil, badRequest("%s", err)
			}
			slog.Warn("fallback target incompatible")
			continue
		}
		j.targets = append(j.targets, prepared{target: t, prompt: p, emulated: emulated})
	}
	slog.Info("completion", "endpoint", endpoint, "key", key.Name, "targets", len(j.targets),
		"stream", req.Stream, "functions", len(req.Tools), "emulated", j.targets[0].emulated,
		"web_search", base.WebSearch, "images", len(base.Images), "max_tokens", base.MaxTokens)
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
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	out := make(chan Event, 16)
	go func() {
		defer close(out)
		defer s.release()
		id := requestID()
		for i, t := range j.targets {
			j.used = t.label
			last := i == len(j.targets)-1
			entry := auditEntry{Project: j.project, Request: id, Target: t.label, Attempt: i + 1, Outcome: "started"}
			if err := s.audit(entry); err != nil {
				out <- Event{Err: errors.New("accounting unavailable")}
				return
			}
			meter := func(u *Usage, failed error) error {
				entry.Outcome = "completed"
				if failed != nil {
					entry.Outcome = "failed"
				}
				if u != nil {
					entry.Units = &Units{Input: int64(u.PromptTokens), Output: int64(u.CompletionTokens)}
				}
				return s.audit(entry)
			}
			err := forward(ctx, t.backend.Name(), t.backend.Run(ctx, t.model, t.prompt), out, t.emulated, !last, meter)
			if err == nil {
				return
			}
			s.metrics.Inc("llm_gateway_fallbacks_total", "from", t.backend.Name())
			slog.Warn("backend failed, trying the next target", append([]any{"target", t.label}, logFailure(err)...)...)
		}
	}()
	return out, nil
}

// forward copies a backend's events to out, reading them to the end. With
// emulated functions the reply is a JSON object, turned here into text and
// calls. An answer that ends without saying how is incomplete, and so is an
// empty one that claims to be whole.
//
// If the backend fails before anything was sent and fallback is set, the
// error is returned instead of sent, for the next target to be tried. Once
// something was sent the answer belongs to this backend: its failure goes
// to the client, never mixed with another backend's output.
func forward(ctx context.Context, name string, in <-chan Event, out chan<- Event, emulated, fallback bool, meter func(*Usage, error) error) error {
	var (
		sent   bool
		failed error
		finish string
		buf    strings.Builder
		usage  *Usage
	)
	for ev := range in {
		if failed != nil || finish != "" && ev.Usage == nil {
			continue // drain
		}
		switch {
		case ev.Err != nil:
			failed = ev.Err
		case ev.Usage != nil:
			usage = ev.Usage
		case ev.Finish != "":
			finish = ev.Finish
		case emulated && ev.Text != "":
			buf.WriteString(ev.Text)
		default:
			sent = true
			out <- ev
		}
	}
	if failed == nil && finish == "" {
		failed = newBackendError(name, failIncomplete, "the answer ended without a finish reason")
		if ctx.Err() != nil {
			failed = ctx.Err()
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
	if failed == nil && !sent && finish == finishStop {
		failed = errNoOutput
	}
	if err := meter(usage, failed); err != nil {
		out <- Event{Err: errors.New("accounting unavailable")}
		return nil
	}
	if failed != nil && fallback && !sent && ctx.Err() == nil && canFallback(failed) {
		return failed
	}
	// The tokens were spent even when the answer is refused.
	if usage != nil {
		out <- Event{Usage: usage}
	}
	if failed != nil {
		out <- Event{Err: failed}
		return nil
	}
	out <- Event{Finish: finish}
	return nil
}

// answer gathers the events of a reply.
type answer struct {
	text     strings.Builder
	calls    []ToolCall
	usage    *Usage
	searches []SearchEvent // in the order they started
	finish   string
	err      error
}

// reason is the finish reason of the reply: one that ended normally with
// calls ended to have them run.
func (a *answer) reason() string {
	if a.finish == finishStop && len(a.calls) > 0 {
		return finishToolCalls
	}
	return a.finish
}

func (a *answer) add(ev Event) {
	switch {
	case ev.Err != nil:
		a.err = ev.Err
	case ev.Usage != nil:
		a.usage = ev.Usage
	case ev.Finish != "":
		a.finish = ev.Finish
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
		"calls", len(a.calls), "searches", len(a.searches), "finish", a.reason()}
	if a.usage != nil {
		s.metrics.Add("llm_gateway_tokens_total", float64(a.usage.PromptTokens), "backend", backend, "kind", "input")
		s.metrics.Add("llm_gateway_tokens_total", float64(a.usage.CompletionTokens), "backend", backend, "kind", "output")
		attrs = append(attrs, "input_tokens", a.usage.PromptTokens, "output_tokens", a.usage.CompletionTokens)
	}
	if a.err != nil {
		slog.Error("completion failed", append(attrs, logFailure(a.err)...)...)
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
			return &apiErr{status: http.StatusRequestEntityTooLarge, typ: "invalid_request_error", code: "request_too_large", msg: "the request body is too large"}
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
	id := "chatcmpl-" + randomID()
	created := time.Now().Unix()
	var a answer
	defer func() {
		drain(events)
		s.observe(j, &a)
	}()

	if req.Stream {
		first, err := early(ctx, events, &a)
		if err != nil {
			writeAPIErr(w, publicError(err))
			return
		}
		s.streamChat(w, replay(first, events), &a, id, created, req)
		return
	}
	for ev := range events {
		a.add(ev)
	}
	if a.err != nil {
		writeAPIErr(w, publicError(a.err))
		return
	}
	msg := ResponseMessage{Role: "assistant", ToolCalls: a.calls}
	if text := a.text.String(); text != "" || len(a.calls) == 0 {
		msg.Content = &text
		msg.Annotations = chatAnnotations(citations(text, searchSources(a.searches)))
	}
	writeJSON(w, http.StatusOK, ChatCompletionResponse{
		ID: id, Object: "chat.completion", Created: created, Model: req.Model,
		Choices: []Choice{{Message: msg, FinishReason: a.reason()}},
		Usage:   a.usage,
	})
}

// streamGrace is how long a streaming request waits for its first event
// before the event stream begins. A request that fails at once, as most
// failures do, gets its status code and Retry-After as a plain response;
// one that fails later can only say so inside the stream.
var streamGrace = 5 * time.Second

// early waits for the first event of a streaming job. It returns the error
// of a job that failed before producing anything, recorded in a, or else
// the events read so far, to be replayed in front of the others.
func early(ctx context.Context, events <-chan Event, a *answer) ([]Event, error) {
	timer := time.NewTimer(streamGrace)
	defer timer.Stop()
	var first []Event
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return first, nil
			}
			first = append(first, ev)
			switch {
			case ev.Err != nil:
				for _, ev := range first {
					a.add(ev)
				}
				return nil, ev.Err
			case ev.Usage != nil:
				// The usage of a refused answer comes before its error:
				// wait for what follows, within the same limits.
				continue
			}
			return first, nil
		case <-timer.C:
			return first, nil
		case <-ctx.Done():
			return first, nil
		}
	}
}

func replay(first []Event, rest <-chan Event) iter.Seq[Event] {
	return func(yield func(Event) bool) {
		for _, ev := range first {
			if !yield(ev) {
				return
			}
		}
		for ev := range rest {
			if !yield(ev) {
				return
			}
		}
	}
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

func (s *Server) streamChat(w http.ResponseWriter, events iter.Seq[Event], a *answer, id string, created int64, req ChatCompletionRequest) {
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
			// The stream ends on the error, with no finish reason and no
			// [DONE]: what was sent is not a complete answer.
			sse.send("", APIError{Error: publicError(ev.Err).body()})
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
	finish := a.reason()
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func (s *Server) acquire(ctx context.Context) *apiErr {
	select {
	case s.sem <- struct{}{}:
	default:
		if int(s.queued.Add(1)) > s.cfg.MaxQueue {
			s.queued.Add(-1)
			s.metrics.Inc("llm_gateway_rejected_total", "reason", "queue_full")
			return busy("the server is at capacity, retry later")
		}
		select {
		case s.sem <- struct{}{}:
			s.queued.Add(-1)
		case <-ctx.Done():
			s.queued.Add(-1)
			s.metrics.Inc("llm_gateway_rejected_total", "reason", "queue_timeout")
			return busy("timed out waiting for a free slot, retry later")
		}
	}
	s.inflight.Add(1)

	return nil
}
func (s *Server) release() { s.inflight.Add(-1); <-s.sem }

func (k *apiKey) permits(b Backend) bool {
	if k.allowed != nil && !k.allowed[b.Name()] {
		return false
	}
	if k.DataClass == "" {
		return true
	}
	up, ok := b.(*UpstreamBackend)
	return ok && classLevel(up.cfg.MaxDataClass) >= classLevel(k.DataClass)
}
