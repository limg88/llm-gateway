package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	randv2 "math/rand/v2"
	"mime"
	"net/http"
	"time"
)

func requestID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func nativeError(w http.ResponseWriter, endpoint string, status int, code string) {
	if endpoint == "messages" {
		writeJSON(w, status, map[string]any{"type": "error", "error": map[string]string{"type": code, "message": http.StatusText(status)}})
		return
	}
	writeAPIErr(w, &apiErr{status: status, typ: code, code: code, msg: http.StatusText(status)})
}

// Explicit native protocols bypass the legacy CLI prompt and event translation.
func (s *Server) native(endpoint string, legacy http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.RequestTimeout)*time.Second)
		defer cancel()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			nativeError(w, endpoint, 413, "request_too_large")
			return
		}
		var fields map[string]json.RawMessage
		var model string
		if json.Unmarshal(body, &fields) != nil || json.Unmarshal(fields["model"], &model) != nil || model == "" {
			nativeError(w, endpoint, 400, "invalid_request_error")
			return
		}
		key := keyOf(r)
		targets, problem := s.resolveModel(model, key)
		if problem != nil {
			if endpoint == "messages" {
				writeAnthropicAPIErr(w, problem)
			} else {
				writeAPIErr(w, problem)
			}
			return
		}
		primary, ok := targets[0].backend.(*UpstreamBackend)
		if !ok || primary.cfg.Protocol == "" {
			if legacy == nil {
				nativeError(w, endpoint, 400, "unsupported_endpoint")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			legacy(w, r)
			return
		}
		if endpoint == "responses" {
			if _, ok := fields["previous_response_id"]; ok {
				nativeError(w, endpoint, 400, "stateful_native_responses_unsupported")
				return
			}
			if _, ok := fields["conversation"]; ok {
				nativeError(w, endpoint, 400, "stateful_native_responses_unsupported")
				return
			}
			if raw, ok := fields["store"]; ok && string(raw) != "false" {
				nativeError(w, endpoint, 400, "stateful_native_responses_unsupported")
				return
			}
			fields["store"] = json.RawMessage("false")
		}
		stream := false
		if raw := fields["stream"]; len(raw) > 0 && json.Unmarshal(raw, &stream) != nil {
			nativeError(w, endpoint, 400, "invalid_request_error")
			return
		}
		if problem := s.acquire(ctx); problem != nil {
			if endpoint == "messages" {
				writeAnthropicAPIErr(w, problem)
			} else {
				writeAPIErr(w, problem)
			}
			return
		}
		defer s.release()
		id := requestID()
		w.Header().Set("X-Request-Id", id)
		attempt, lastStatus := 0, 400
		previousProvider := ""
		for _, t := range targets {
			u, ok := t.backend.(*UpstreamBackend)
			if !ok || u.cfg.Protocol == "" {
				continue
			}
			if (endpoint == "messages") != (u.cfg.Protocol == "anthropic") {
				continue
			}
			if len(u.cfg.Models) > 0 && !oneOf(t.model, u.cfg.Models) {
				continue
			}
			if t.model == "" {
				continue
			}
			payload, invalid := u.nativePayload(fields, t.model, endpoint, stream)
			if invalid != nil {
				nativeError(w, endpoint, 400, invalid.code)
				return
			}
			var ck *[32]byte
			if s.cfg.CacheTTL > 0 && cacheEligible(endpoint, stream, fields) {
				digest := cacheKey(key, t.label, payload)
				ck = &digest
				if cached := s.cache.get(digest); cached != nil {
					err := s.audit(auditEntry{Project: key.Project, Request: id, Target: t.label, Outcome: "cached"})
					if err != nil {
						nativeError(w, endpoint, 503, "accounting_unavailable")
						return
					}
					w.Header().Set("X-Gateway-Cache", "hit")
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(cached)
					return
				}
			}
			if previousProvider != "" {
				s.metrics.Inc("llm_gateway_fallbacks_total", "from", previousProvider)
			}
			previousProvider = u.Name()
			for retry := 0; retry <= s.cfg.Retries; retry++ {
				attempt++
				entry := auditEntry{Request: id, Project: key.Project, Target: t.label, Attempt: attempt, Outcome: "started", BudgetDay: time.Now().UTC().Format(time.DateOnly)}
				if err := s.audit(entry); err != nil {
					if errors.Is(err, errBudget) {
						nativeError(w, endpoint, 403, "project_budget_exhausted")
						return
					}
					nativeError(w, endpoint, 503, "accounting_unavailable")
					return
				}
				started := time.Now()
				response, err := u.nativeRequest(ctx, r, endpoint, payload)
				status := 502
				var after time.Duration
				if err == nil {
					status = response.StatusCode
					entry.Status = status
					after = retryAfterHeader(response.Header.Get("Retry-After"))
				}
				if err == nil && status >= 200 && status < 300 {
					outcome := s.finishNative(w, endpoint, stream, response, entry, ck)
					s.metrics.Inc("llm_gateway_requests_total", "endpoint", endpoint, "backend", u.Name(), "status", outcome)
					s.metrics.Observe("llm_gateway_request_duration_seconds", time.Since(started).Seconds(), "backend", u.Name())
					return
				}
				if response != nil {
					// Failed attempts can also report billed usage.
					b, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
					var m meter
					m.observe(b, endpoint)
					entry.Units = m.result()
					response.Body.Close()
				}
				entry.Outcome = "failed"
				s.metrics.Inc("llm_gateway_requests_total", "endpoint", endpoint, "backend", u.Name(), "status", "error")
				if s.audit(entry) != nil {
					nativeError(w, endpoint, 503, "accounting_unavailable")
					return
				}
				if ctx.Err() != nil {
					nativeError(w, endpoint, 504, "timeout")
					return
				}
				lastStatus = status
				if status < 400 || status > 599 {
					lastStatus = 502
				}
				if !transient(status) {
					nativeError(w, endpoint, lastStatus, "upstream_error")
					return
				}
				if retry < s.cfg.Retries {
					delay := time.Duration(100*(1<<retry)) * time.Millisecond
					delay += time.Duration(randv2.Int64N(int64(delay / 2)))
					if after > delay {
						delay = after
					}
					if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
						break
					}
					select {
					case <-ctx.Done():
						nativeError(w, endpoint, 504, "timeout")
						return
					case <-time.After(delay):
					}
				}
			}
			s.metrics.Inc("llm_gateway_native_exhausted_total", "backend", u.Name())
		}
		nativeError(w, endpoint, lastStatus, "no_compatible_upstream")
	}
}

// Rewrite only transport-specific fields; retain the raw JSON of tool schemas
// and provider extensions on each independent retry target.
func (u *UpstreamBackend) nativePayload(original map[string]json.RawMessage, model, endpoint string, stream bool) ([]byte, *apiErr) {
	fields := maps.Clone(original)
	if dest := u.cfg.MaxTokensField; endpoint == "chat/completions" && dest != "" {
		other := "max_tokens"
		if dest == other {
			other = "max_completion_tokens"
		}
		if val, present := fields[other]; present {
			if _, dup := fields[dest]; dup {
				return nil, badRequest("conflicting output limits")
			}
			fields[dest] = val
			delete(fields, other)
		}
	}
	fields["model"], _ = json.Marshal(model)
	if endpoint == "chat/completions" && stream {
		var options map[string]any
		if raw := fields["stream_options"]; len(raw) != 0 && json.Unmarshal(raw, &options) != nil {
			return nil, badRequest("invalid stream_options")
		}
		if options == nil {
			options = map[string]any{}
		}
		options["include_usage"] = true
		fields["stream_options"], _ = json.Marshal(options)
	}
	payload, _ := json.Marshal(fields)
	return payload, nil
}
func transient(s int) bool {
	return s == 429 || s == 500 || s == 502 || s == 503 || s == 504 || s == 529
}
func (u *UpstreamBackend) nativeRequest(ctx context.Context, in *http.Request, endpoint string, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", u.cfg.BaseURL+"/"+endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	switch u.cfg.Protocol {
	case "anthropic":
		req.Header.Set("anthropic-version", in.Header.Get("anthropic-version"))
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		for _, beta := range in.Header.Values("anthropic-beta") {
			req.Header.Add("anthropic-beta", beta)
		}
		req.Header.Set("x-api-key", u.key)
	case "azure":
		req.Header.Set("api-key", u.key)
	default:
		if u.key != "" {
			req.Header.Set("Authorization", "Bearer "+u.key)
		}
	}
	return u.client.Do(req)
}
func (s *Server) finishNative(w http.ResponseWriter, endpoint string, stream bool, res *http.Response, entry auditEntry, ck *[32]byte) string {
	defer res.Body.Close()
	m := meter{}
	entry.Outcome = "completed"
	if stream {
		media, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
		if media != "text/event-stream" {
			entry.Outcome = "invalid_stream"
			s.audit(entry)
			nativeError(w, endpoint, 502, "invalid_stream")
			return "error"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		err := relayEvents(w, res.Body, &m, endpoint)
		if err != nil || !m.done || m.failed {
			entry.Outcome = "incomplete"
		}
		entry.Units = m.result()
		if entry.Outcome != "completed" {
			entry.Units = nil
			if !m.failed {
				if endpoint == "messages" {
					_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream stream interrupted\"}}\n\n")
				} else {
					_, _ = io.WriteString(w, "data: {\"error\":{\"code\":\"upstream_stream_interrupted\"}}\n\n")
				}
			}
		}
		if err := s.audit(entry); err != nil {
			slog.Error("usage ledger write failed", "request_id", entry.Request)
			return "error"
		}
		if entry.Outcome == "completed" {
			return "ok"
		}
		return "error"
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil || len(b) > maxBody || !json.Valid(b) {
		entry.Outcome = "invalid_response"
		s.audit(entry)
		nativeError(w, endpoint, 502, "invalid_response")
		return "error"
	}
	m.observe(b, endpoint)
	entry.Units = m.result()
	if err := s.audit(entry); err != nil {
		nativeError(w, endpoint, 503, "accounting_unavailable")
		return "error"
	}
	w.Header().Set("Content-Type", "application/json")
	if ck != nil {
		s.cache.put(*ck, b, time.Duration(s.cfg.CacheTTL)*time.Second)
	}
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(b)
	return "ok"
}

// Forward a complete SSE frame as soon as it arrives, including tool
// argument fragments and provider-specific events. Keep memory bounded.
func relayEvents(w http.ResponseWriter, body io.Reader, m *meter, endpoint string) error {
	scan := bufio.NewScanner(body)
	scan.Buffer(make([]byte, 4096), 8<<20)
	var frame, data bytes.Buffer
	for scan.Scan() {
		line := scan.Bytes()
		if frame.Len()+len(line) > 8<<20 {
			return errors.New("oversized SSE frame")
		}
		frame.Write(line)
		frame.WriteByte('\n')
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data.Write(bytes.TrimPrefix(rest, []byte(" ")))
			data.WriteByte('\n')
		}
		if len(line) != 0 {
			continue
		}
		m.observe(bytes.TrimSpace(data.Bytes()), endpoint)
		if _, err := w.Write(frame.Bytes()); err != nil {
			return err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if m.done || m.failed {
			return nil
		}
		frame.Reset()
		data.Reset()
	}
	if frame.Len() != 0 {
		return io.ErrUnexpectedEOF
	}
	return scan.Err()
}
