package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	defaultSystemPrompt = "You are a helpful assistant."
	mcpServerName       = "fn"
	mcpToolPrefix       = "mcp__" + mcpServerName + "__"
)

// ClaudeBackend answers with the claude CLI (Claude Code) in print mode.
//
// The CLI runs with no built-in tools, in an empty directory, and talks
// stream-json on both sides, so that a process can be started before the
// request it will serve arrives.
type ClaudeBackend struct {
	Bin       string
	WorkDir   string
	Isolate   bool     // ignore the user's settings, hooks and plugins
	ExtraArgs []string // appended to every command line
	// Publish serves the client's functions as an MCP server.
	Publish func([]FunctionDefinition) (url string, unpublish func())

	pool *warmPool[*claudeProc]
}

func NewClaudeBackend(bin, workDir string, warm int, idle time.Duration) *ClaudeBackend {
	return &ClaudeBackend{Bin: bin, WorkDir: workDir, Isolate: true, pool: newWarmPool[*claudeProc](warm, idle)}
}

func (c *ClaudeBackend) Name() string              { return "claude" }
func (c *ClaudeBackend) NativeFunctions() bool     { return true }
func (c *ClaudeBackend) CheckImages([]Image) error { return nil }
func (c *ClaudeBackend) Models() []string          { return []string{"opus", "sonnet", "haiku"} }
func (c *ClaudeBackend) Close()                    { c.pool.close() }
func (c *ClaudeBackend) Status() map[string]any {
	idle, hits, misses := c.pool.stats()
	return map[string]any{"bin": c.Bin, "warm_idle": idle, "warm_size": c.pool.size, "warm_hits": hits, "warm_misses": misses}
}

// claudeEffort maps an OpenAI reasoning effort to the CLI's levels.
func claudeEffort(e string) string {
	switch e {
	case "minimal", "low":
		return "low"
	case "medium", "high", "xhigh", "max":
		return e
	}
	return ""
}

// args builds the command line. Options that take a list are always
// followed by another option, never left last before a positional.
func (c *ClaudeBackend) args(model string, p Prompt, functionsURL string) []string {
	system := p.System
	if system == "" {
		system = defaultSystemPrompt
	}
	tools := ""
	var allowed []string
	if p.WebSearch {
		tools = "WebSearch,WebFetch"
		allowed = append(allowed, "WebSearch", "WebFetch")
	}
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--no-session-persistence",
		"--disable-slash-commands",
		"--tools", tools,
		"--strict-mcp-config",
		"--system-prompt", system,
	}
	if c.Isolate {
		// The working directory is empty: only the user's own settings
		// could add hooks, plugins or instructions, and they are left out.
		args = append(args, "--setting-sources", "project,local")
	}
	if functionsURL != "" {
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
			mcpServerName: map[string]any{"type": "http", "url": functionsURL},
		}})
		args = append(args, "--mcp-config", string(cfg))
		for _, f := range p.Functions {
			allowed = append(allowed, mcpToolPrefix+f.Name)
		}
	}
	if len(allowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if e := claudeEffort(p.Effort); e != "" {
		args = append(args, "--effort", e)
	}
	if len(p.Schema) > 0 {
		args = append(args, "--json-schema", string(p.Schema))
	}
	return append(args, c.ExtraArgs...)
}

// poolKey identifies everything that goes on the command line: two prompts
// with the same key can use the same waiting process.
func poolKey(model string, p Prompt) string {
	b, _ := json.Marshal([]any{model, p.System, p.Schema, p.WebSearch, p.Functions, p.Effort})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// claudeProc is a running claude CLI that reads user messages on its
// standard input.
type claudeProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan []byte
	err    error // set before done is closed
	stderr bytes.Buffer
	done   chan struct{}

	killMu  sync.Mutex
	killed  bool
	cleanup func()
}

func (c *ClaudeBackend) start(model string, p Prompt) (*claudeProc, error) {
	url, unpublish := "", func() {}
	if len(p.Functions) > 0 {
		if c.Publish == nil {
			return nil, errors.New("claude: functions are not available")
		}
		url, unpublish = c.Publish(p.Functions)
	}
	cmd := exec.Command(c.Bin, c.args(model, p, url)...)
	cmd.Dir = c.WorkDir
	proc := &claudeProc{cmd: cmd, lines: make(chan []byte, 64), done: make(chan struct{}), cleanup: unpublish}
	cmd.Stderr = &limitedWriter{w: &proc.stderr, n: maxStderr}
	stdin, err := cmd.StdinPipe()
	if err == nil {
		proc.stdin = stdin
		var stdout io.ReadCloser
		if stdout, err = cmd.StdoutPipe(); err == nil {
			if err = cmd.Start(); err == nil {
				go proc.read(stdout)
				return proc, nil
			}
		}
	}
	unpublish()
	return nil, fmt.Errorf("starting %s: %w", c.Bin, err)
}

func (p *claudeProc) read(stdout io.Reader) {
	r := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			p.lines <- line
		}
		if err != nil {
			break
		}
	}
	p.err = p.cmd.Wait()
	p.cleanup()
	close(p.lines)
	close(p.done)
}

// send writes one user message.
func (p *claudeProc) send(text string, imgs []Image) error {
	msg, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": claudeContent(text, imgs)},
	})
	if err != nil {
		return err
	}
	_, err = p.stdin.Write(append(msg, '\n'))
	return err
}

func (p *claudeProc) kill() {
	p.killMu.Lock()
	defer p.killMu.Unlock()
	if p.killed {
		return
	}
	p.killed = true
	p.stdin.Close()
	p.cmd.Process.Kill()
	// Unblock the reader, which may be waiting to hand over a line.
	go func() {
		for range p.lines {
		}
	}()
}

func (p *claudeProc) exited() <-chan struct{} { return p.done }

func (p *claudeProc) exitErr() error {
	<-p.done
	p.killMu.Lock()
	defer p.killMu.Unlock()
	if p.err == nil {
		return errors.New("claude exited before it answered")
	}
	return cliError("claude", p.err, p.stderr.String())
}

func (c *ClaudeBackend) Run(ctx context.Context, model string, p Prompt) <-chan Event {
	out := make(chan Event, 16)
	go func() {
		defer close(out)
		st := &claudeStream{ctx: ctx, out: out, searches: map[string]bool{}, blocks: map[int]int{}, structured: len(p.Schema) > 0}
		if len(p.Functions) > 0 {
			st.text = newTextCalls(p.Functions)
		}
		key := poolKey(model, p)
		c.converse(ctx, key, model, p, out, st.line)
		// Have a process ready for the next request of this kind.
		c.pool.add(key, func() (*claudeProc, error) { return c.start(model, p) })
	}()
	return out
}

// converse answers with a waiting process if there is one, and with a new
// one otherwise or when the waiting one turns out to be dead.
func (c *ClaudeBackend) converse(ctx context.Context, key, model string, p Prompt, out chan<- Event, onLine func([]byte) bool) {
	if proc, ok := c.pool.take(key); ok {
		slog.Debug("claude process", "warm", true)
		if c.answer(ctx, p, proc, out, onLine, true) {
			return
		}
	}
	proc, err := c.start(model, p)
	if err != nil {
		out <- Event{Err: err}
		return
	}
	c.answer(ctx, p, proc, out, onLine, false)
}

// answer sends the prompt to proc and reads the reply. With retry set it
// returns false, having sent nothing, if the process died without output.
func (c *ClaudeBackend) answer(ctx context.Context, p Prompt, proc *claudeProc, out chan<- Event, onLine func([]byte) bool, retry bool) bool {
	stop := context.AfterFunc(ctx, proc.kill)
	defer stop()
	defer proc.kill()

	sendErr := proc.send(p.Text, p.Images)
	seen := false
	for line := range proc.lines {
		seen = true
		if onLine(line) {
			return true
		}
	}
	if ctx.Err() != nil {
		out <- Event{Err: ctx.Err()}
		return true
	}
	if retry && !seen {
		return false
	}
	err := proc.exitErr()
	if sendErr != nil && proc.err == nil {
		err = fmt.Errorf("claude: %w", sendErr)
	}
	out <- Event{Err: err}
	return true
}

type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

func (u *claudeUsage) usage() *Usage {
	in := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	return &Usage{PromptTokens: in, CompletionTokens: u.OutputTokens, TotalTokens: in + u.OutputTokens}
}

// claudeLine is one line of the CLI's stream-json output.
type claudeLine struct {
	Type  string `json:"type"`
	Event *struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		ContentBlock *struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Usage *claudeUsage `json:"usage"`
	} `json:"event"`
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ParentToolUseID  *string         `json:"parent_tool_use_id"`
	ToolUseResult    json.RawMessage `json:"tool_use_result"`
	IsError          bool            `json:"is_error"`
	Subtype          string          `json:"subtype"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Usage            *claudeUsage    `json:"usage"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// claudeStream turns the CLI's output lines into events.
type claudeStream struct {
	ctx  context.Context
	out  chan<- Event
	text *textCalls // nil without functions

	structured bool            // the answer is the structured output of the result
	wrote      bool            // some text was sent
	blocks     map[int]int     // content block index -> call index, in the current message
	calls      int             // function calls made
	searches   map[string]bool // tool_use ids of the running searches
	usage      claudeUsage
}

func (s *claudeStream) send(ev Event) { emit(s.ctx, s.out, ev) }

func (s *claudeStream) sendText(t string) {
	if t == "" || s.structured {
		return
	}
	if s.text == nil {
		s.wrote = true
		s.send(Event{Text: t})
		return
	}
	pieces, _ := s.text.feed(t)
	s.pieces(pieces)
}

func (s *claudeStream) pieces(pieces []textPiece) {
	for _, pc := range pieces {
		if pc.call == nil {
			s.wrote = true
			s.send(Event{Text: pc.text})
			continue
		}
		slog.Warn("claude: function call written as text, made a call", "function", pc.call.Name)
		s.send(Event{Call: &CallEvent{Index: s.calls, ID: "call_" + randomID(), Name: pc.call.Name, Args: pc.call.Arguments, Done: true}})
		s.calls++
	}
}

// line handles one output line and reports whether the answer is complete.
func (s *claudeStream) line(line []byte) bool {
	var l claudeLine
	if err := json.Unmarshal(line, &l); err != nil {
		return false
	}
	switch l.Type {
	case "stream_event":
		if l.Event == nil || l.ParentToolUseID != nil {
			return false
		}
		ev := l.Event
		switch ev.Type {
		case "message_start":
			s.blocks = map[int]int{}
		case "content_block_start":
			if b := ev.ContentBlock; b != nil && b.Type == "tool_use" && strings.HasPrefix(b.Name, mcpToolPrefix) {
				s.blocks[ev.Index] = s.calls
				s.send(Event{Call: &CallEvent{Index: s.calls, ID: "call_" + randomID(), Name: strings.TrimPrefix(b.Name, mcpToolPrefix)}})
				s.calls++
			}
		case "content_block_delta":
			if ev.Delta == nil {
				return false
			}
			switch ev.Delta.Type {
			case "text_delta":
				s.sendText(ev.Delta.Text)
				if s.text != nil && s.text.called {
					return s.finish()
				}
			case "input_json_delta":
				if idx, ok := s.blocks[ev.Index]; ok && ev.Delta.PartialJSON != "" {
					s.send(Event{Call: &CallEvent{Index: idx, Args: ev.Delta.PartialJSON}})
				}
			}
		case "content_block_stop":
			if idx, ok := s.blocks[ev.Index]; ok {
				s.send(Event{Call: &CallEvent{Index: idx, Done: true}})
			}
		case "message_delta":
			if ev.Usage != nil {
				s.usage = *ev.Usage
			}
			// The client executes its functions: the turn ends here.
			if ev.Delta != nil && ev.Delta.StopReason == "tool_use" && len(s.blocks) > 0 {
				return s.finish()
			}
		}
	case "assistant", "user":
		if l.Message == nil || l.ParentToolUseID != nil {
			return false
		}
		var blocks []claudeBlock
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			return false
		}
		for _, b := range blocks {
			switch {
			case b.Type == "tool_use" && (b.Name == "WebSearch" || b.Name == "WebFetch"):
				if !s.searches[b.ID] {
					s.searches[b.ID] = true
					s.send(Event{Search: &SearchEvent{ID: b.ID, Action: claudeSearchAction(b.Name, b.Input)}})
				}
			case b.Type == "tool_result" && s.searches[b.ToolUseID]:
				delete(s.searches, b.ToolUseID)
				s.send(Event{Search: &SearchEvent{ID: b.ToolUseID, Done: true, Sources: claudeSearchSources(b.Content, l.ToolUseResult)}})
			}
		}
	case "result":
		if l.IsError {
			msg := l.Result
			if msg == "" {
				msg = l.Subtype
			}
			s.send(Event{Err: fmt.Errorf("claude: %s", msg)})
			return true
		}
		if s.text != nil {
			s.pieces(s.text.flush())
		}
		if !s.wrote && s.calls == 0 {
			// Structured output and some errors come only in the result.
			structured := bytes.TrimSpace(l.StructuredOutput)
			switch {
			case len(structured) > 0 && !bytes.Equal(structured, []byte("null")):
				s.send(Event{Text: string(structured)})
			case l.Result != "":
				s.send(Event{Text: l.Result})
			}
		}
		if l.Usage != nil {
			s.usage = *l.Usage
		}
		return s.finish()
	}
	return false
}

func (s *claudeStream) finish() bool {
	s.send(Event{Usage: s.usage.usage()})
	return true
}
