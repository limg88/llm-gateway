package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeFallbackAndLedger(t *testing.T) {
	for _, status := range []int{429, 503, 400, 401} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var first, second atomic.Int32
			primary := testHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { first.Add(1); w.WriteHeader(status) }))
			defer primary.Close()
			secondary := testHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				second.Add(1)
				var payload obj
				json.NewDecoder(r.Body).Decode(&payload)
				if payload["model"] != "m" || payload["temperature"] != 0.25 || r.Header.Get("Authorization") != "Bearer provider-fixture" {
					t.Error("model, sampling or credential lost")
				}
				writeJSON(w, 200, obj{"choices": []obj{{"message": obj{"content": "ok"}}}, "usage": obj{"prompt_tokens": 10, "completion_tokens": 3}})
			}))
			defer secondary.Close()
			price := 2.0
			ledger := filepath.Join(t.TempDir(), "usage.jsonl")
			_, gateway := newTestServer(t, func(c *Config) {
				c.Backends = []string{}
				c.Profile = "production"
				c.Ledger = ledger
				c.Retries = 1
				c.PriceVersion = "test"
				t.Setenv("TEST_PROJECT", "project-fixture")
				t.Setenv("TEST_PROVIDER", "provider-fixture")
				c.APIKeys = []KeyConfig{{Name: "client", KeyEnv: "TEST_PROJECT", Project: "pilot"}}
				c.Models = map[string][]string{"claude/sonnet": {"a/m", "b/m"}}
				c.Prices = map[string]Price{"b/m": {Input: &price, Output: &price}}
				for name, url := range map[string]string{"a": primary.URL, "b": secondary.URL} {
					c.Upstreams = append(c.Upstreams, UpstreamConfig{Name: name, BaseURL: url, Models: []string{"m"}, Protocol: "openai", APIKeyEnv: "TEST_PROVIDER"})
				}
			})
			code, _ := post(t, gateway, "/v1/chat/completions", "project-fixture", obj{"model": "claude/sonnet", "temperature": 0.25, "messages": user("private-canary")})
			if transient(status) {
				if code != 200 || first.Load() != 2 || second.Load() != 1 {
					t.Fatalf("status=%d attempts=%d/%d", code, first.Load(), second.Load())
				}
			} else if code != status || first.Load() != 1 || second.Load() != 0 {
				t.Fatalf("replayed permanent error %d", status)
			}
			b, err := os.ReadFile(ledger)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"private-canary", "provider-fixture", "project-fixture"} {
				if bytes.Contains(b, []byte(secret)) {
					t.Fatal("sensitive ledger field")
				}
			}
			if transient(status) {
				var e auditEntry
				rows := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
				if err = json.Unmarshal(rows[len(rows)-1], &e); err != nil || e.Attempt != 3 || e.Project != "pilot" || e.Cost == nil || math.Abs(*e.Cost-26e-6) > 1e-12 {
					t.Fatalf("bad settlement: %+v", e)
				}
			}
		})
	}
}
func TestNativeIncremental(t *testing.T) {
	for _, endpoint := range []string{"messages", "chat/completions"} {
		t.Run(endpoint, func(t *testing.T) {
			release := make(chan struct{})
			first := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
			last := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3},\"choices\":[]}\n\ndata: [DONE]\n\n"
			protocol := "openai"
			if endpoint == "messages" {
				protocol = "anthropic"
				first = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2,\"cache_read_input_tokens\":8,\"output_tokens\":1}}}\n\n"
				last = "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			}
			upstream := testHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/"+endpoint {
					t.Errorf("wrong path %s", r.URL.Path)
				}
				var body obj
				json.NewDecoder(r.Body).Decode(&body)
				if protocol == "anthropic" && (r.Header.Get("anthropic-beta") != "test-beta" || body["cache_control"] == nil || r.Header.Get("x-api-key") != "provider-fixture") {
					t.Error("lost native Anthropic fields")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, first)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				case <-time.After(3 * time.Second):
					t.Error("first event buffered")
					return
				}
				io.WriteString(w, last)
			}))
			defer upstream.Close()
			_, gateway := newTestServer(t, func(c *Config) {
				c.Backends = []string{}
				c.Upstreams = []UpstreamConfig{{Name: "api", BaseURL: upstream.URL + "/v1", Protocol: protocol, Models: []string{"m"}, APIKey: "provider-fixture"}}
			})
			req, _ := http.NewRequest("POST", gateway.URL+"/v1/"+endpoint, strings.NewReader(`{"model":"api/m","stream":true,"cache_control":{"type":"ephemeral"},"messages":[]}`))
			req.Header.Set("anthropic-beta", "test-beta")
			req.Header.Set(clientIDHeader, "native:stream")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.Header.Get("X-Request-Id") == "" || resp.Header.Get(clientIDHeader) != "native:stream" {
				t.Error("missing stream correlation headers")
			}
			reader := bufio.NewReader(resp.Body)
			var got strings.Builder
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				got.WriteString(line)
				if line == "\n" {
					break
				}
			}
			close(release)
			tail, err := io.ReadAll(reader)
			if err != nil || got.String() != first || string(tail) != last {
				t.Fatal("incomplete or rewritten stream")
			}
		})
	}
}
func TestTOMLProfile(t *testing.T) {
	t.Setenv("GATEWAY_PROJECT_KEY", "fixture")
	t.Setenv("OPENAI_API_KEY", "provider1")
	t.Setenv("ANTHROPIC_API_KEY", "provider2")
	c, err := loadConfig([]string{"-config", "gateway.example.toml"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Profile != "production" || len(c.APIKeys) != 1 || len(c.Models["claude/sonnet"]) != 1 {
		t.Fatal("bad example configuration")
	}
}
func TestAnthropicPricing(t *testing.T) {
	var m meter
	m.observe([]byte(`{"usage":{"input_tokens":4,"output_tokens":3,"cache_creation_input_tokens":10,"cache_read_input_tokens":2,"cache_creation":{"ephemeral_1h_input_tokens":7}}}`), "messages")
	r := 1.
	p := Price{Input: &r, Output: &r, CacheRead: &r, CacheWrite: &r, CacheHour: &r}
	u := m.result()
	if u.Write != 3 || u.Hour != 7 || *p.cost(u) != 19e-6 {
		t.Fatal(u)
	}
	p.CacheHour = nil
	if p.cost(u) != nil {
		t.Fatal("unpriced cache treated as free")
	}
}

func TestProjectBudgetRecovery(t *testing.T) {
	cfg := defaultConfig()
	cfg.Ledger = filepath.Join(t.TempDir(), "usage.jsonl")
	cfg.Budgets = map[string]Budget{"a": {Daily: 1, Reserve: 0.6}}
	cfg.Backends = nil
	s := NewServer(cfg)
	defer s.Close()
	day := time.Now().UTC().Format(time.DateOnly)
	entry := auditEntry{Project: "a", BudgetDay: day, Outcome: "started", Attempt: 1}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- s.audit(entry) }()
	}
	yes, no := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			yes++
		} else if err == errBudget {
			no++
		} else {
			t.Fatal(err)
		}
	}
	if yes != 1 || no != 1 {
		t.Fatal("concurrent budget not enforced")
	}
	restarted := NewServer(cfg)
	defer restarted.Close()
	if err := restarted.audit(entry); err != errBudget {
		t.Fatal("lost reservation on restart", err)
	}
	entry.Project = "b"
	if err := restarted.audit(entry); err != nil {
		t.Fatal(err)
	}
}
func TestNativeCacheIsolation(t *testing.T) {
	var calls atomic.Int32
	up := testHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, obj{"choices": []obj{{"finish_reason": "stop", "message": obj{"content": "ok"}}}, "usage": obj{"prompt_tokens": 2, "completion_tokens": 1}})
	}))
	defer up.Close()
	_, gw := newTestServer(t, func(c *Config) {
		c.CacheTTL = 30
		c.Backends = nil
		c.APIKeys = []KeyConfig{{Key: "one", Name: "a"}, {Key: "two", Name: "b"}}
		c.Upstreams = []UpstreamConfig{{Name: "mock", Protocol: "openai", BaseURL: up.URL, Models: []string{"m"}}}
	})
	payload := obj{"model": "mock/m", "temperature": 0, "messages": user("question")}
	for _, key := range []string{"one", "one", "two"} {
		if code, _ := post(t, gw, "/v1/chat/completions", key, payload); code != 200 {
			t.Fatal(code)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("cache isolation", calls.Load())
	}
	payload["tools"] = []obj{weatherTool}
	for i := 0; i < 2; i++ {
		post(t, gw, "/v1/chat/completions", "one", payload)
	}
	if calls.Load() != 4 {
		t.Fatal("cached tool request")
	}
}
func TestNativePartialStreamAndDataPolicy(t *testing.T) {
	var forbidden, backup atomic.Int32
	first := testHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer first.Close()
	denied := testHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forbidden.Add(1) }))
	defer denied.Close()
	last := testHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { backup.Add(1) }))
	defer last.Close()
	_, gw := newTestServer(t, func(c *Config) {
		c.Backends = nil
		c.APIKeys = []KeyConfig{{Name: "qa", Key: "client", DataClass: "client"}}
		c.Models = map[string][]string{"safe": {"denied/m", "first/m", "last/m"}}
		for _, u := range []struct{ name, url, level string }{{"denied", denied.URL, "synthetic"}, {"first", first.URL, "client"}, {"last", last.URL, "client"}} {
			c.Upstreams = append(c.Upstreams, UpstreamConfig{Name: u.name, BaseURL: u.url, Protocol: "openai", Models: []string{"m"}, MaxDataClass: u.level})
		}
	})
	code, b := post(t, gw, "/v1/chat/completions", "client", obj{"model": "safe", "messages": user("x"), "stream": true})
	if code != 200 || !bytes.Contains(b, []byte("upstream_stream_interrupted")) || bytes.Contains(b, []byte("[DONE]")) || forbidden.Load() != 0 || backup.Load() != 0 {
		t.Fatalf("unexpected retry/disclosure: %d %s", code, b)
	}
}
func TestAzureEmbeddings(t *testing.T) {
	up := testHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("api-key") != "deployment-fixture" || r.Header.Get("Authorization") != "" || r.URL.Path != "/openai/v1/embeddings" {
			t.Error("invalid Azure v1 contract")
		}
		writeJSON(w, 200, obj{"data": []obj{{"embedding": []float64{0.1, 0.2}}}, "usage": obj{"prompt_tokens": 2}})
	}))
	defer up.Close()
	_, gw := newTestServer(t, func(c *Config) {
		c.Backends = nil
		c.Upstreams = []UpstreamConfig{{Name: "azure", Protocol: "azure", APIKey: "deployment-fixture", BaseURL: up.URL + "/openai/v1", Models: []string{"encoder"}}}
	})
	if code, _ := post(t, gw, "/v1/embeddings", "", obj{"model": "azure/encoder", "input": "hello"}); code != 200 {
		t.Fatal(code)
	}
}

func TestUsageIsolationAndLedgerLock(t *testing.T) {
	cfg := defaultConfig()
	cfg.Backends = nil
	cfg.Ledger = filepath.Join(t.TempDir(), "usage.jsonl")
	cfg.APIKeys = []KeyConfig{{Name: "p1", Key: "key1"}, {Name: "p2", Key: "key2"}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg)
	defer s.Close()
	if err := s.checkLedger(); err != nil {
		t.Fatal(err)
	}
	other := NewServer(cfg)
	defer other.Close()
	if err := other.checkLedger(); err == nil {
		t.Fatal("two writers acquired the same ledger")
	}
	for _, project := range []string{"p1", "p2", "p2"} {
		e := auditEntry{Project: project, Request: requestID(), Outcome: "started"}
		if err := s.audit(e); err != nil {
			t.Fatal(err)
		}
		e.Outcome = "completed"
		e.Units = &Units{Input: 5, Output: 7}
		if err := s.audit(e); err != nil {
			t.Fatal(err)
		}
	}
	ts := testHTTP(s.Handler())
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer key1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var totals struct {
		Project  string
		Attempts int   `json:"finished_attempts"`
		Tokens   Units `json:"known_tokens"`
	}
	if json.NewDecoder(resp.Body).Decode(&totals) != nil || totals.Project != "p1" || totals.Attempts != 1 || totals.Tokens.Output != 7 {
		t.Fatal("cross-project usage", totals)
	}
}
