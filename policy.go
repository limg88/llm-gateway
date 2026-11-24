package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

var errBudget = errors.New("daily project budget reached")

type Budget struct {
	Daily   float64 `json:"daily_usd"`
	Reserve float64 `json:"reserve_usd"`
}
type cacheItem struct {
	body    []byte
	expires time.Time
}
type responseCache struct {
	mu    sync.Mutex
	items map[[32]byte]cacheItem
}

func (c *responseCache) get(key [32]byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	item := c.items[key]
	if time.Now().Before(item.expires) {
		return item.body
	}
	delete(c.items, key)
	return nil
}
func (c *responseCache) put(key [32]byte, body []byte, ttl time.Duration) {
	// At most 128 small responses, and never cache responses with side effects.
	if len(body) > 64<<10 {
		return
	}
	var r struct {
		Choices []struct {
			Finish  string `json:"finish_reason"`
			Message struct {
				Content   string            `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &r) != nil || len(r.Choices) != 1 || r.Choices[0].Finish != "stop" || r.Choices[0].Message.Content == "" || len(r.Choices[0].Message.ToolCalls) > 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[[32]byte]cacheItem)
	}
	if len(c.items) >= 128 {
		for k := range c.items {
			delete(c.items, k)
			break
		}
	}
	c.items[key] = cacheItem{body, time.Now().Add(ttl)}
}
func cacheEligible(endpoint string, stream bool, fields map[string]json.RawMessage) bool {
	if endpoint != "chat/completions" || stream {
		return false
	}
	for key := range fields {
		switch key {
		case "model", "messages", "temperature", "max_tokens", "max_completion_tokens":
		default:
			return false
		}
	}
	var temp float64
	if json.Unmarshal(fields["temperature"], &temp) != nil || string(fields["temperature"]) == "null" || temp != 0 {
		return false
	}
	var msgs []struct {
		Role    string
		Content json.RawMessage
	}
	if json.Unmarshal(fields["messages"], &msgs) != nil {
		return false
	}
	for _, m := range msgs {
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
			return false
		}
		var text string
		if json.Unmarshal(m.Content, &text) != nil {
			return false
		}
	}
	return true
}
func cacheKey(key *apiKey, target string, payload []byte) [32]byte {
	h := sha256.New()
	h.Write(key.digest[:])
	h.Write([]byte(target))
	h.Write([]byte{0})
	h.Write(payload)
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}
