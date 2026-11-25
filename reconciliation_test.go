package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func ledgerRequest(handler http.Handler, method, path, body, key string, ids ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	for _, id := range ids {
		r.Header.Add(clientIDHeader, id)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestReconcileRetry(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := testHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(503)
			return
		}
		writeJSON(w, 200, obj{"usage": obj{"prompt_tokens": 12, "completion_tokens": 3}})
	}))
	defer upstream.Close()
	ledger := filepath.Join(t.TempDir(), "usage.jsonl")
	s, _ := newTestServer(t, func(c *Config) {
		c.Backends = []string{}
		c.Ledger = ledger
		c.Retries = 1
		c.APIKeys = []KeyConfig{{Name: "one", Key: "first"}, {Name: "two", Key: "second"}}
		c.Upstreams = []UpstreamConfig{{Name: "api", BaseURL: upstream.URL, Protocol: "openai", Models: []string{"m"}}}
	})
	h := s.Handler()
	const id = "run:analyst#2"
	lookup := "/v1/attempts?client_request_id=" + url.QueryEscape(id)
	for _, ids := range [][]string{{""}, {"short"}, {"bad / value"}, {strings.Repeat("a", 201)}, {id, id}} {
		w := ledgerRequest(h, "POST", "/v1/chat/completions", `{"model":"api/m"}`, "first", ids...)
		if w.Code != 400 || calls.Load() != 0 {
			t.Fatalf("invalid ID: status=%d calls=%d", w.Code, calls.Load())
		}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- ledgerRequest(h, "POST", "/v1/chat/completions", `{"model":"api/m"}`, "first", id) }()
	select {
	case <-entered:
	case w := <-done:
		t.Fatalf("unexpected early response %d %s", w.Code, w.Body)
	}
	w := ledgerRequest(h, "GET", lookup, "", "first")
	close(release)
	response := <-done
	var open, settled attemptsResponse
	if json.Unmarshal(w.Body.Bytes(), &open) != nil || w.Code != 200 || open.Unsettled != 1 || len(open.Attempts) != 0 {
		t.Fatal(w.Code, w.Body)
	}
	rid := response.Header().Get("X-Request-Id")
	if response.Code != 200 || rid == "" || response.Header().Get(clientIDHeader) != id {
		t.Fatal(response)
	}
	w = ledgerRequest(h, "GET", lookup, "", "first")
	if json.Unmarshal(w.Body.Bytes(), &settled) != nil || w.Code != 200 || settled.ClientID != id || settled.Unsettled != 0 || len(settled.Attempts) != 2 {
		t.Fatal(w.Code, w.Body)
	}
	a, b := settled.Attempts[0], settled.Attempts[1]
	if a.Request != rid || a.Attempt != 1 || a.Outcome != "failed" || a.Status != 503 || a.Tokens != nil || b.Request != rid || b.Attempt != 2 || b.Status != 200 || b.Outcome != "ok" || b.Tokens == nil || b.Tokens.Input != 12 || b.Tokens.Output != 3 {
		t.Fatal(settled)
	}
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(rows) != 4 {
		t.Fatal(rows)
	}
	for _, row := range rows {
		var e auditEntry
		if json.Unmarshal([]byte(row), &e) != nil || e.ClientRequest != id || e.Request != rid {
			t.Fatal(row)
		}
	}
	for key, want := range map[string]int{"second": 404, "invalid": 401} {
		if got := ledgerRequest(h, "GET", lookup, "", key).Code; got != want {
			t.Fatalf("lookup %s: %d", key, got)
		}
	}
}

func TestPersonalReconcile(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := s.Handler()
	const id = "reused:client"
	seen := map[string]bool{}
	for _, spec := range []struct{ endpoint, model string }{{"messages", "claude"}, {"chat/completions", "codex"}} {
		w := ledgerRequest(h, "POST", "/v1/"+spec.endpoint, `{"model":"`+spec.model+`","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`, "", id)
		rid := w.Header().Get("X-Request-Id")
		if w.Code != 200 || rid == "" || seen[rid] || w.Header().Get(clientIDHeader) != id {
			t.Fatal(w.Code, w.Body, w.Header())
		}
		seen[rid] = true
	}
	w := ledgerRequest(h, "GET", "/v1/attempts?client_request_id="+id, "", "")
	var result attemptsResponse
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || w.Code != 200 || result.Unsettled != 0 || len(result.Attempts) != 2 {
		t.Fatal(w.Body)
	}
	for _, a := range result.Attempts {
		if !seen[a.Request] || a.Tokens == nil || a.Cost != nil || a.Outcome != "ok" {
			t.Fatal(a)
		}
		delete(seen, a.Request)
	}
	w = ledgerRequest(h, "GET", "/v1/usage", "", "")
	var usage projectUsage
	if json.Unmarshal(w.Body.Bytes(), &usage) != nil || w.Code != 200 || usage.Project != "personal" || usage.Open != 0 || usage.Attempts != 2 || usage.Unknown != 2 || usage.Tokens.Input != 20 || usage.Tokens.Output != 8 {
		t.Fatal(w.Body)
	}
}

func TestReconcileAcrossRestart(t *testing.T) {
	cfg := defaultConfig()
	cfg.Backends = []string{}
	cfg.Ledger = filepath.Join(t.TempDir(), "ledger.jsonl")
	s := NewServer(cfg)
	if err := s.checkLedger(); err != nil {
		t.Fatal(err)
	}
	trace := requestTrace{id: "server-request", clientID: "run:restart"}
	finish := s.trackRequest("personal", trace)
	entry := auditEntry{Project: "personal", Request: trace.id, ClientRequest: trace.clientID, Attempt: 1, Outcome: "started"}
	write := func() {
		t.Helper()
		if err := s.audit(entry); err != nil {
			t.Fatal(err)
		}
	}
	check := func(server *Server, finished, open int) {
		t.Helper()
		res := ledgerRequest(server.Handler(), "GET", "/v1/attempts?client_request_id="+trace.clientID, "", "")
		var result attemptsResponse
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &result) != nil || len(result.Attempts) != finished || result.Unsettled != open {
			t.Fatal(res.Code, res.Body)
		}
	}
	check(s, 0, 1) // waiting for admission
	write()
	entry.Outcome = "failed"
	write()
	check(s, 1, 1) // between retries
	entry.Attempt = 2
	entry.Outcome = "started"
	write()
	finish()
	s.Close()
	reopened := NewServer(cfg)
	defer reopened.Close()
	if err := reopened.checkLedger(); err != nil {
		t.Fatal(err)
	}
	check(reopened, 1, 1) // durable reservation without a settlement
}
