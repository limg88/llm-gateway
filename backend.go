package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
)

// Prompt is the backend-neutral form of a request.
//
// CLI backends use the flattened fields (System, Text, Images, ...).
// Pass-through backends (OpenAI-compatible upstreams) use Messages, Tools
// and ToolChoice, which keep the conversation in its original shape.
type Prompt struct {
	System    string
	Text      string
	Images    []Image
	Schema    json.RawMessage
	WebSearch bool
	Functions []FunctionDefinition
	Effort    string
	// MaxTokens is the output budget in tokens, zero for none. How it is
	// applied depends on the backend: see each one. MaxTokensField is the
	// name a Chat Completions client gave it, for the pass-through backends.
	MaxTokens      int
	MaxTokensField string

	Messages   []ChatMessage
	Tools      []Tool
	ToolChoice json.RawMessage
}

type Source struct {
	URL   string
	Title string
}

// SearchEvent reports a web search (or page fetch) made by the backend.
// It is sent once when the search starts and once more with Done set.
type SearchEvent struct {
	ID      string
	Done    bool
	Action  *WebSearchAction
	Sources []Source
}

// CallEvent streams a function call. The first event of an Index carries
// ID and Name, later ones append to Args, and the last one has Done set.
// A single event may carry all of them.
type CallEvent struct {
	Index int
	ID    string
	Name  string
	Args  string
	Done  bool
}

// Event is one item of a backend's output stream. Exactly one field is set.
// Finish is one of the finish* reasons.
type Event struct {
	Text   string
	Usage  *Usage
	Err    error
	Search *SearchEvent
	Call   *CallEvent
	Finish string
}

// Backend runs a prompt and streams the answer. An answer ends with a
// Finish event or with an error, which arrives as an Event too; then the
// channel is closed. A channel closed without either is a broken answer.
type Backend interface {
	Name() string
	// NativeFunctions reports whether the backend calls the client's
	// functions itself. Otherwise they are emulated with a JSON schema.
	NativeFunctions() bool
	CheckImages(imgs []Image) error
	// Models lists the model names worth advertising, without the backend prefix.
	Models() []string
	Run(ctx context.Context, model string, p Prompt) <-chan Event
	// Status is shown by /health.
	Status() map[string]any
	Close()
}

// emit sends an event unless the request is gone.
func emit(ctx context.Context, out chan<- Event, ev Event) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// limitedWriter keeps the first n bytes written to it and drops the rest.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		q := p
		if len(q) > l.n {
			q = q[:l.n]
		}
		l.w.Write(q)
		l.n -= len(q)
	}
	return len(p), nil
}

const maxStderr = 64 << 10

// runJSONL starts cmd, writes stdin to it and calls onLine for every line of
// its standard output. onLine returns true when the answer is complete: the
// process is then stopped. It returns the failure of the process, if any;
// a process that just ends early is the caller's to judge.
func runJSONL(ctx context.Context, name string, cmd *exec.Cmd, stdin string, onLine func([]byte) bool) error {
	var stderr bytes.Buffer
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stderr = &limitedWriter{w: &stderr, n: maxStderr}
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return newBackendError(name, failGeneric, "starting the CLI: "+err.Error())
	}
	stopped := false
	r := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 && onLine(line) {
			stopped = true
			break
		}
		if err != nil {
			break
		}
	}
	if stopped {
		cmd.Process.Kill()
		io.Copy(io.Discard, stdout)
		cmd.Wait()
		return nil
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return cliError(name, err, stderr.String())
	}
	return nil
}

// cliError classifies the failure of a CLI from the end of its standard
// error, where the reason is.
func cliError(name string, err error, stderr string) error {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = err.Error()
	}
	if len(msg) > 2000 {
		msg = msg[len(msg)-2000:]
	}
	return textFailure(name, msg)
}

func randomID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
