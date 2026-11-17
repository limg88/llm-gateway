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
)

// CodexBackend answers with the codex CLI, one `codex exec` per request, in
// a read-only sandbox. Codex reports whole messages, not text deltas.
//
// Codex is a coding agent: left as it is, it runs shell commands and uses
// the MCP servers and plugins of the user's configuration, and its read-only
// sandbox still lets it read every file the user can. A client could then
// ask it for the machine's files. So, unless Home is empty, it runs with a
// home of its own that holds the credentials and nothing else, and with the
// features that reach the machine turned off.
type CodexBackend struct {
	Bin       string
	WorkDir   string
	ExtraArgs []string
	Home      string // isolated CODEX_HOME; empty to use the user's own
}

// codexDisabled are the codex features turned off in an isolated backend.
var codexDisabled = []string{
	"shell_tool", "apps", "plugins", "hooks", "browser_use", "browser_use_external",
	"computer_use", "multi_agent", "image_generation", "memories",
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
		ID    string `json:"id"`
		Type  string `json:"type"`
		Text  string `json:"text"`
		Query string `json:"query"`
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
			for _, f := range codexDisabled {
				args = append(args, "--disable", f)
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
				out <- Event{Err: fmt.Errorf("writing output schema: %w", err)}
				return
			}
			args = append(args, "--output-schema", f.Name())
		}
		paths, remove, err := imageFiles(p.Images)
		if err != nil {
			out <- Event{Err: err}
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
		cmd.Dir = c.WorkDir
		if c.Home != "" {
			cmd.Env = append(os.Environ(), "CODEX_HOME="+c.Home)
		}
		answered, ended, lastErr := false, false, ""
		runJSONL(ctx, cmd, text, out, func(line []byte) bool {
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
						if answered {
							emit(ctx, out, Event{Text: "\n\n"})
						}
						answered = true
						emit(ctx, out, Event{Text: l.Item.Text})
					}
				case "web_search":
					emit(ctx, out, Event{Search: &SearchEvent{
						ID:     l.Item.ID,
						Done:   l.Type == "item.completed",
						Action: &WebSearchAction{Type: "search", Query: l.Item.Query},
					}})
				}
			case "turn.completed":
				if u := l.Usage; u != nil {
					emit(ctx, out, Event{Usage: &Usage{
						PromptTokens:     u.InputTokens,
						CompletionTokens: u.OutputTokens,
						TotalTokens:      u.InputTokens + u.OutputTokens,
					}})
				}
				ended = true
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
				emit(ctx, out, Event{Err: errors.New("codex: " + msg)})
				return true
			}
			return false
		})
		if !ended && !answered && lastErr != "" && ctx.Err() == nil {
			out <- Event{Err: errors.New("codex: " + lastErr)}
		}
	}()
	return out
}
