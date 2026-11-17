package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// UpstreamConfig describes an OpenAI-compatible HTTP API (OpenAI, Ollama,
// vLLM, LM Studio, OpenRouter, another gateway...) used as a backend.
type UpstreamConfig struct {
	Name      string   `json:"name"`
	BaseURL   string   `json:"base_url"`    // up to and including /v1
	APIKey    string   `json:"api_key"`     // prefer api_key_env
	APIKeyEnv string   `json:"api_key_env"` // environment variable holding the key
	Models    []string `json:"models"`      // advertised by /v1/models
	// MaxTokensField names the output budget for this API: max_tokens or
	// max_completion_tokens. Empty to use the name the client used, and
	// max_tokens for a budget that came through another API.
	MaxTokensField string `json:"max_tokens_field"`
}

// UpstreamBackend forwards the conversation, in its original shape, to an
// OpenAI-compatible chat completions endpoint and reads its stream. The
// output budget is forwarded too: the upstream applies it and says, with
// its finish reason, whether the answer was cut.
type UpstreamBackend struct {
	cfg    UpstreamConfig
	key    string
	client *http.Client
}

func NewUpstreamBackend(cfg UpstreamConfig) *UpstreamBackend {
	key := cfg.APIKey
	if cfg.APIKeyEnv != "" {
		key = os.Getenv(cfg.APIKeyEnv)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &UpstreamBackend{cfg: cfg, key: key, client: &http.Client{}}
}

func (u *UpstreamBackend) Name() string              { return u.cfg.Name }
func (u *UpstreamBackend) NativeFunctions() bool     { return true }
func (u *UpstreamBackend) CheckImages([]Image) error { return nil }
func (u *UpstreamBackend) Models() []string          { return u.cfg.Models }
func (u *UpstreamBackend) Close()                    { u.client.CloseIdleConnections() }
func (u *UpstreamBackend) Status() map[string]any {
	return map[string]any{"base_url": u.cfg.BaseURL}
}

func (u *UpstreamBackend) Run(ctx context.Context, model string, p Prompt) <-chan Event {
	out := make(chan Event, 16)
	go func() {
		defer close(out)
		if err := u.run(ctx, model, p, out); err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			out <- Event{Err: err}
		}
	}()
	return out
}

func (u *UpstreamBackend) run(ctx context.Context, model string, p Prompt, out chan<- Event) error {
	if model == "" && len(u.cfg.Models) > 0 {
		model = u.cfg.Models[0]
	}
	if model == "" {
		return newBackendError(u.cfg.Name, failInvalid, "a model is required: use "+u.cfg.Name+"/<model>")
	}
	body := map[string]any{
		"model":          model,
		"messages":       p.Messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(p.Tools) > 0 {
		body["tools"] = p.Tools
		if len(p.ToolChoice) > 0 {
			body["tool_choice"] = p.ToolChoice
		}
	}
	if p.Effort != "" {
		body["reasoning_effort"] = p.Effort
	}
	if p.MaxTokens > 0 {
		field := "max_tokens"
		if u.cfg.MaxTokensField != "" {
			field = u.cfg.MaxTokensField
		} else if p.MaxTokensField != "" {
			field = p.MaxTokensField
		}
		body[field] = p.MaxTokens
	}
	if len(p.Schema) > 0 {
		body["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "response", "schema": p.Schema, "strict": true},
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.cfg.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if u.key != "" {
		req.Header.Set("Authorization", "Bearer "+u.key)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return newBackendError(u.cfg.Name, failGeneric, err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		var e struct {
			Error upstreamError `json:"error"`
		}
		if json.Unmarshal(raw, &e) != nil || e.Error.Message == "" {
			e.Error.Message = string(raw)
		}
		return u.failure(resp.StatusCode, e.Error, resp.Header.Get("Retry-After"))
	}

	started := map[int]bool{} // tool call indexes seen
	closeCalls := func() {
		for idx := range started {
			emit(ctx, out, Event{Call: &CallEvent{Index: idx, Done: true}})
		}
		started = map[int]bool{}
	}
	finish := "" // set by the chunk that ends the answer
	r := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		data = bytes.TrimSpace(data)
		if ok && len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
			var chunk struct {
				Choices []struct {
					Delta        Delta   `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *Usage         `json:"usage"`
				Error *upstreamError `json:"error"`
			}
			if json.Unmarshal(data, &chunk) == nil {
				if chunk.Error != nil {
					closeCalls()
					return u.failure(0, *chunk.Error, "")
				}
				for _, c := range chunk.Choices {
					if c.Delta.Content != "" {
						emit(ctx, out, Event{Text: c.Delta.Content})
					}
					for i, tc := range c.Delta.ToolCalls {
						idx := i
						if tc.Index != nil {
							idx = *tc.Index
						}
						ev := &CallEvent{Index: idx, Args: tc.Function.Arguments}
						if !started[idx] {
							started[idx] = true
							ev.ID, ev.Name = tc.ID, tc.Function.Name
							if ev.ID == "" {
								ev.ID = "call_" + randomID()
							}
						}
						emit(ctx, out, Event{Call: ev})
					}
					if c.FinishReason != nil && *c.FinishReason != "" {
						closeCalls()
						finish = *c.FinishReason
					}
				}
				if chunk.Usage != nil {
					emit(ctx, out, Event{Usage: chunk.Usage})
				}
			}
		}
		if err != nil {
			closeCalls()
			// The stream is whole only if a chunk gave its finish reason:
			// one that just ends, cleanly or not, was cut on the way.
			switch {
			case finish == "" && err == io.EOF:
				return newBackendError(u.cfg.Name, failIncomplete, "the stream ended without a finish reason")
			case finish == "":
				return newBackendError(u.cfg.Name, failIncomplete, "reading the stream: "+err.Error())
			}
			reason, known := upstreamFinish[finish]
			if !known {
				// Nothing says an answer that ended for a reason unknown
				// here is whole: it is not passed off as one.
				return newBackendError(u.cfg.Name, failIncomplete, "unknown finish reason "+strconv.Quote(finish))
			}
			emit(ctx, out, Event{Finish: reason})
			return nil
		}
	}
}

// upstreamError is the error object of an upstream, whose code may be a
// string, a number or missing.
type upstreamError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// upstreamFinish maps the finish reasons of the Chat Completions API.
var upstreamFinish = map[string]string{
	"stop":           finishStop,
	"length":         finishLength,
	"tool_calls":     finishToolCalls,
	"function_call":  finishToolCalls,
	"content_filter": finishFilter,
}

// failure classifies an error of the upstream: from its status code, or,
// for an error sent inside the stream (status 0), from its code and text.
func (u *UpstreamBackend) failure(status int, e upstreamError, retryAfter string) *BackendError {
	words := e.Type + " " + e.Message
	if e.Code != nil {
		words = fmt.Sprint(e.Code) + " " + words
	}
	hint := classify(words)
	kind := failGeneric
	switch {
	case status == http.StatusRequestEntityTooLarge, hint == failTooLarge:
		kind = failTooLarge
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		kind = failAuth
	case status == http.StatusTooManyRequests:
		kind = failRateLimit
		if hint == failQuota {
			kind = failQuota
		}
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		kind = failTimeout
	case status == http.StatusServiceUnavailable, status == 529:
		kind = failOverloaded
	case status >= 400 && status < 500:
		kind = failInvalid
	case status == 0:
		kind = hint
	}
	be := newBackendError(u.cfg.Name, kind, fmt.Sprintf("status %d: %s", status, strings.TrimSpace(words)))
	if kind == failRateLimit || kind == failQuota || kind == failOverloaded {
		if be.RetryAfter = retryAfterHeader(retryAfter); be.RetryAfter == 0 {
			be.RetryAfter = retryIn(e.Message)
		}
	}
	return be
}
