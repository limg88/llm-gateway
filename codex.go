package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// CodexBackend answers with the codex CLI, one `codex exec` per request, in
// a read-only sandbox. Codex reports whole messages, not text deltas.
//
// Codex has no setting for an output budget, so the budget is checked
// afterwards: the answer is held back until the turn reports its usage, and
// one whose output tokens (reasoning included, as the OpenAI API counts
// them) exceed the budget is refused whole. Its text is never cut to fit.
//
// Codex has no setting for an output budget, so the budget is checked
// afterwards: the answer is held back until the turn reports its usage, and
// one whose output tokens (reasoning included, as the OpenAI API counts
// them) exceed the budget is refused whole. Its text is never cut to fit.
//
// Codex is a coding agent: left as it is, it runs shell commands and uses
// the MCP servers and plugins of the user's configuration, and its read-only
// sandbox still lets it read every file the user can. A client could then
// ask it for the machine's files. So, unless Home is empty, it runs with a
// home of its own that holds the credentials and nothing else, and with the
// every feature the installed codex has on turned off.
type CodexBackend struct {
	Bin       string
	WorkDir   string
	ExtraArgs []string
	Home      string // isolated CODEX_HOME; empty to use the user's own
	WebSearch bool   // let an isolated codex search the web

	mu       sync.Mutex
	features []string // enabled features of the installed codex
}

// codexKeep are the features an isolated codex may keep, and when. Every
// other feature the installed codex reports as enabled is turned off, so
// that a feature added by a later version starts disabled here.
//
// code_mode_host carries the web search tool, and with it apply_patch,
// which the read-only sandbox refuses. It is a host for tools, more than
// web search alone: whether to keep it is the operator's choice (WebSearch),
// never the client's. CheckPrompt refuses the requests that would need it
// otherwise.
func (c *CodexBackend) keep(feature string, p Prompt) bool {
	return feature == "code_mode_host" && c.WebSearch && p.WebSearch
}

// CheckPrompt refuses what this backend is not set up to do.
func (c *CodexBackend) CheckPrompt(p Prompt) error {
	if p.WebSearch && c.Home != "" && !c.WebSearch {
		return errors.New("web search is not enabled for codex on this server (codex_web_search)")
	}
	return nil
}

// enabledFeatures asks the installed codex which features are on, in the
// isolated home. The answer is cached; a failure is not, and fails the
// request: codex never runs isolated without this list.
func (c *CodexBackend) enabledFeatures(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.features != nil {
		return c.features, nil
	}
	cmd := exec.CommandContext(ctx, c.Bin, "features", "list")
	cmd.Env = append(childEnvironment(), "CODEX_HOME="+c.Home)
	cmd.Dir = c.WorkDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("codex: cannot list the features to disable: %w", err)
	}
	features := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		// name, stage (one or more words), enabled
		f := strings.Fields(line)
		if len(f) >= 3 && f[len(f)-1] == "true" {
			features = append(features, f[0])
		}
	}
	if len(features) == 0 {
		return nil, errors.New("codex: unexpected output of `codex features list`")
	}
	c.features = features
	return features, nil
}

// codexUnsafeArgs are the options that would undo the isolation.
var codexUnsafeArgs = []string{"--enable", "--sandbox", "-s", "--add-dir", "--config", "-c", "--profile", "-p", "--approve-for-me", "--cd", "-C"}

// checkCodexArgs refuses, for an isolated codex, the extra arguments that
// widen what it can reach.
func checkCodexArgs(args []string) error {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		if strings.HasPrefix(name, "--dangerously") || oneOf(name, codexUnsafeArgs) {
			return fmt.Errorf("codex_extra_args: %s is not allowed with codex_isolate", name)
		}
	}
	return nil
}

// newCodexHome creates a CODEX_HOME with nothing but a link to the user's
// credentials: no configuration, MCP servers, plugins or instructions.
func newCodexHome() (string, error) {
	src := os.Getenv("CODEX_HOME")
	if src == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		src = filepath.Join(home, ".codex")
	}
	dir, err := os.MkdirTemp("", "llm-gateway-codex-")
	if err != nil {
		return "", err
	}
	auth := filepath.Join(src, "auth.json")
	if _, err := os.Stat(auth); err != nil {
		slog.Warn("codex: no auth.json to share with the isolated home: codex needs its credentials in the environment", "path", auth)
		return dir, nil
	}
	if err := os.Symlink(auth, filepath.Join(dir, "auth.json")); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func (c *CodexBackend) Name() string          { return "codex" }
func (c *CodexBackend) NativeFunctions() bool { return false }
func (c *CodexBackend) Models() []string      { return nil }
func (c *CodexBackend) Status() map[string]any {
	return map[string]any{"bin": c.Bin, "isolated": c.Home != ""}
}

func (c *CodexBackend) Close() {
	if c.Home != "" {
		os.RemoveAll(c.Home) // removes the link, not the credentials
	}
}

func (c *CodexBackend) CheckImages(imgs []Image) error {
	for _, img := range imgs {
		if img.URL != "" {
			return errCodexImageURL
		}
	}
	return nil
}

type codexLine struct {
	Type string `json:"type"`
	Item *struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Text   string `json:"text"`
		Query  string `json:"query"`
		Action *struct {
			Type  string `json:"type"`
			Query string `json:"query"`
			URL   string `json:"url"`
		} `json:"action"`
	} `json:"item"`
	Usage *struct {
		InputTokens     int `json:"input_tokens"`
		OutputTokens    int `json:"output_tokens"`
		ReasoningTokens int `json:"reasoning_output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

func (c *CodexBackend) Run(ctx context.Context, model string, p Prompt) <-chan Event {
	out := make(chan Event, 16)
	go func() {
		defer close(out)
		args := []string{"exec", "--json", "--skip-git-repo-check", "--ephemeral", "--color", "never", "--sandbox", "read-only", "--cd", c.WorkDir}
		if model != "" {
			args = append(args, "--model", model)
		}
		if p.Effort != "" && p.Effort != "none" {
			args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", p.Effort))
		}
		if p.WebSearch {
			args = append(args, "-c", `web_search="live"`)
		}
		if c.Home != "" {
			features, err := c.enabledFeatures(ctx)
			if err != nil {
				out <- Event{Err: newBackendError("codex", failGeneric, err.Error())}
				return
			}
			for _, f := range features {
				if !c.keep(f, p) {
					args = append(args, "--disable", f)
				}
			}
		}
		if len(p.Schema) > 0 {
			f, err := os.CreateTemp("", "llm-gateway-schema-*.json")
			if err == nil {
				defer os.Remove(f.Name())
				_, err = f.Write(p.Schema)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
			}
			if err != nil {
				out <- Event{Err: newBackendError("codex", failGeneric, "writing the output schema: "+err.Error())}
				return
			}
			args = append(args, "--output-schema", f.Name())
		}
		paths, remove, err := imageFiles(p.Images)
		if err != nil {
			out <- Event{Err: newBackendError("codex", failGeneric, err.Error())}
			return
		}
		defer remove()
		args = append(args, c.ExtraArgs...)
		// --image takes a list: it goes first, so that another option
		// always follows it. The prompt is read from standard input.
		var first []string
		for _, path := range paths {
			first = append(first, "--image", path)
		}
		args = append(append([]string{"exec"}, first...), args[1:]...)

		text := p.Text + imagesNote(len(p.Images))
		if p.System != "" {
			text = "<instructions>\n" + p.System + "\n</instructions>\n\n" + text
		}

		cmd := exec.CommandContext(ctx, c.Bin, args...)
		cmd.Env = childEnvironment()
		cmd.Dir = c.WorkDir
		if c.Home != "" {
			cmd.Env = append(childEnvironment(), "CODEX_HOME="+c.Home)
		}
		var (
			answered, ended bool
			lastErr         string
			held            []string // messages kept until the budget is checked
		)
		say := func(text string) {
			if answered {
				text = "\n\n" + text
			}
			answered = true
			if p.MaxTokens > 0 {
				held = append(held, text)
				return
			}
			emit(ctx, out, Event{Text: text})
		}
		runErr := runJSONL(ctx, "codex", cmd, text, func(line []byte) bool {
			var l codexLine
			if json.Unmarshal(line, &l) != nil {
				return false
			}
			switch l.Type {
			case "item.started", "item.completed":
				if l.Item == nil {
					return false
				}
				switch l.Item.Type {
				case "agent_message":
					if l.Type == "item.completed" && l.Item.Text != "" {
						say(l.Item.Text)
					}
				case "web_search":
					action := &WebSearchAction{Type: "search", Query: l.Item.Query}
					if a := l.Item.Action; a != nil && a.Type == "open_page" {
						action = &WebSearchAction{Type: "open_page", URL: a.URL}
					}
					emit(ctx, out, Event{Search: &SearchEvent{ID: l.Item.ID, Done: l.Type == "item.completed", Action: action}})
				}
			case "turn.completed":
				ended = true
				output := 0
				if u := l.Usage; u != nil {
					output = u.OutputTokens
					emit(ctx, out, Event{Usage: &Usage{
						PromptTokens:     u.InputTokens,
						CompletionTokens: u.OutputTokens,
						TotalTokens:      u.InputTokens + u.OutputTokens,
					}})
				}
				if p.MaxTokens > 0 && output > p.MaxTokens {
					emit(ctx, out, Event{Err: newBackendError("codex", failOutputLimit,
						fmt.Sprintf("%d output tokens against a budget of %d", output, p.MaxTokens))})
					return true
				}
				for _, text := range held {
					emit(ctx, out, Event{Text: text})
				}
				emit(ctx, out, Event{Finish: finishStop})
				return true
			case "turn.failed", "error":
				msg := l.Message
				if l.Error != nil && l.Error.Message != "" {
					msg = l.Error.Message
				}
				if msg == "" {
					msg = "turn failed"
				}
				if l.Type == "error" {
					// Codex also reports its retries as errors and goes
					// on: only a failed turn, or the exit, ends the answer.
					lastErr = msg
					return false
				}
				ended = true
				emit(ctx, out, Event{Err: textFailure("codex", msg)})
				return true
			}
			return false
		})
		// Only turn.completed ends an answer: a codex that stops before it,
		// whatever it wrote so far, did not finish.
		switch {
		case ended:
		case ctx.Err() != nil:
			out <- Event{Err: ctx.Err()}
		case lastErr != "":
			out <- Event{Err: textFailure("codex", lastErr)}
		case runErr != nil:
			out <- Event{Err: runErr}
		default:
			out <- Event{Err: newBackendError("codex", failIncomplete, "codex exited before the turn was completed")}
		}
	}()
	return out
}
