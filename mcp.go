package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// The client's functions reach a CLI as the tools of an MCP server, served
// here. The server never executes them: the gateway sees the call in the
// CLI's output, stops the CLI and hands the call to the client.

type functionSet struct {
	defs []FunctionDefinition
	refs int
}

// mcpRegistry holds the function sets currently published. The id of a set
// derives from its content and a per-process secret: equal sets share an id,
// which keeps a CLI's command line stable, and ids cannot be guessed.
type mcpRegistry struct {
	mu     sync.Mutex
	secret []byte
	sets   map[string]*functionSet
}

func newMCPRegistry() *mcpRegistry {
	secret := make([]byte, 32)
	rand.Read(secret)
	return &mcpRegistry{secret: secret, sets: map[string]*functionSet{}}
}

func (r *mcpRegistry) register(defs []FunctionDefinition) (id string, unregister func()) {
	b, _ := json.Marshal(defs)
	mac := hmac.New(sha256.New, r.secret)
	mac.Write(b)
	id = hex.EncodeToString(mac.Sum(nil))[:32]

	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.sets[id]
	if set == nil {
		set = &functionSet{defs: defs}
		r.sets[id] = set
	}
	set.refs++
	var once sync.Once
	return id, func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if set.refs--; set.refs == 0 {
				delete(r.sets, id)
			}
		})
	}
}

func (r *mcpRegistry) get(id string) *functionSet {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sets[id]
}

func (r *mcpRegistry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sets)
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

const mcpProtocolVersion = "2025-06-18"

func rpcResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id json.RawMessage, code int, msg string) map[string]any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

// handleMCP serves one function set over MCP's streamable HTTP transport,
// without sessions or server-initiated messages.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	set := s.mcp.get(r.PathValue("id"))
	if set == nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	var req rpcRequest
	if err != nil || json.Unmarshal(body, &req) != nil {
		writeJSON(w, http.StatusBadRequest, rpcError(nil, -32700, "parse error"))
		return
	}
	if len(req.ID) == 0 || bytes.Equal(req.ID, []byte("null")) {
		// A notification: nothing to answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = mcpProtocolVersion
		}
		writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "llm-gateway", "version": version},
		}))
	case "ping":
		writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{}))
	case "tools/list":
		tools := make([]map[string]any, 0, len(set.defs))
		for _, d := range set.defs {
			schema := d.Parameters
			if len(bytes.TrimSpace(schema)) == 0 || bytes.Equal(bytes.TrimSpace(schema), []byte("null")) {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{"name": d.Name, "description": d.Description, "inputSchema": schema})
		}
		writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{"tools": tools}))
	case "tools/call":
		writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "the function is executed by the client"}},
			"isError": false,
		}))
	default:
		writeJSON(w, http.StatusOK, rpcError(req.ID, -32601, "method not found"))
	}
}

// PublishFunctions makes a function set available to the CLIs and returns
// the URL of its MCP server.
func (s *Server) PublishFunctions(defs []FunctionDefinition) (url string, unpublish func()) {
	id, unregister := s.mcp.register(defs)
	return s.cfg.SelfURL + "/mcp/" + id, unregister
}
