package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Run with REPO_PILOT_DIR pointing to the actual consumer checkout.
// The test executes its unmodified analysis module and reads this repository's HEAD.
func TestRepoPilotConsumer(t *testing.T) {
	dir := os.Getenv("REPO_PILOT_DIR")
	if dir == "" {
		t.Skip("optional external consumer checkout")
	}
	var rounds atomic.Int32
	upstream := testHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model    string
			Messages []struct{ Role, Content string }
			Tools    []json.RawMessage
			Format   json.RawMessage `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		msg := obj{}
		reason := "stop"
		switch rounds.Add(1) {
		case 1:
			if len(payload.Tools) != 2 || payload.Model != "mock-model" {
				t.Error("initial request lost tools or alias")
			}
			reason = "tool_calls"
			msg["tool_calls"] = []obj{{"id": "read-1", "type": "function", "function": obj{"name": "read_lines", "arguments": `{"path":"go.mod","start":1,"end":1}`}}}
		case 2:
			last := payload.Messages[len(payload.Messages)-1]
			if last.Role != "tool" || !strings.Contains(last.Content, "github.com/limg88/llm-gateway") {
				t.Error("Repo Pilot did not read the requested Git evidence")
			}
			msg["content"] = "Prepare final answer."
		case 3:
			if !strings.Contains(string(payload.Format), "repository_analysis") {
				t.Error("missing final JSON Schema")
			}
			msg["content"] = `{"summary":"Go module [E1].","current":"Go [E1]","expected":"","gaps":"","questions":"","citations":["E1"]}`
		default:
			t.Error("unexpected extra inference")
		}
		writeJSON(w, 200, obj{"choices": []obj{{"message": msg, "finish_reason": reason}}, "usage": obj{"prompt_tokens": 10, "completion_tokens": 6}})
	}))
	defer upstream.Close()
	_, gateway := newTestServer(t, func(c *Config) {
		c.Backends = []string{}
		c.Models = map[string][]string{"claude/sonnet": {"mock/mock-model"}}
		c.APIKeys = []KeyConfig{{Name: "repo-pilot", Key: "consumer-fixture"}}
		c.Upstreams = []UpstreamConfig{{Name: "mock", BaseURL: upstream.URL, Protocol: "openai", Models: []string{"mock-model"}}}
	})
	code := `import {pathToFileURL} from 'node:url';
const {analyze,gateway}=await import(pathToFileURL(process.argv[1]));
const complete=gateway(process.env.REPO_PILOT_GATEWAY, 'claude/sonnet',process.env.REPO_PILOT_API_KEY);
const result=await analyze('.', {mode:'question',question:'Which Go module?',story:'',ref:'HEAD'},complete);
if(result.answer.citations[0]!=='E1'||result.evidence[0]?.path!=='go.mod')throw Error('Evidence missing');`
	cmd := exec.Command("node", "--input-type=module", "-e", code, filepath.Join(dir, "analysis.ts"))
	cmd.Env = append(os.Environ(), "REPO_PILOT_GATEWAY="+gateway.URL+"/v1/chat/completions", "REPO_PILOT_API_KEY=consumer-fixture")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("consumer: %v: %s", err, out)
	}
	if rounds.Load() != 3 {
		t.Fatalf("unexpected rounds %d", rounds.Load())
	}
}
