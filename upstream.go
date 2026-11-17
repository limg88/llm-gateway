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
}

// UpstreamBackend forwards the conversation, in its original shape, to an
// OpenAI-compatible chat completions endpoint and reads its stream.
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
		return fmt.Errorf("%s: a model is required: use %s/<model>", u.cfg.Name, u.cfg.Name)
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
		return fmt.Errorf("%s: %w", u.cfg.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		var e APIError
		if json.Unmarshal(msg, &e) == nil && e.Error.Message != "" {
			msg = []byte(e.Error.Message)
		}
		return fmt.Errorf("%s: status %d: %s", u.cfg.Name, resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	started := map[int]bool{} // tool call indexes seen
	finish := func() {
		for idx := range started {
			emit(ctx, out, Event{Call: &CallEvent{Index: idx, Done: true}})
		}
		started = map[int]bool{}
	}
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
				Usage *Usage        `json:"usage"`
				Error *APIErrorBody `json:"error"`
			}
			if json.Unmarshal(data, &chunk) == nil {
				if chunk.Error != nil {
					return fmt.Errorf("%s: %s", u.cfg.Name, chunk.Error.Message)
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
					if c.FinishReason != nil {
						finish()
					}
				}
				if chunk.Usage != nil {
					emit(ctx, out, Event{Usage: chunk.Usage})
				}
			}
		}
		if err != nil {
			finish()
			if err == io.EOF {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s: %w", u.cfg.Name, err)
		}
	}
}
