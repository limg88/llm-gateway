package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// KeyConfig is one API key of the gateway.
type KeyConfig struct {
	Name     string   `json:"name"`     // shown in logs, never the key itself
	Key      string   `json:"key"`      // prefer key_env
	KeyEnv   string   `json:"key_env"`  // environment variable holding the key
	RPM      int      `json:"rpm"`      // requests per minute, 0 for no limit
	Backends []string `json:"backends"` // allowed backends, empty for all
}

// Config is the whole configuration. Every value comes, in this order of
// precedence, from a command-line flag, an environment variable, the
// configuration file, or its default.
type Config struct {
	ListenAddr     string `json:"listen_addr"`
	ClaudeBin      string `json:"claude_bin"`
	CodexBin       string `json:"codex_bin"`
	WorkDir        string `json:"work_dir"`
	APIKey         string `json:"api_key"`
	MaxConcurrency int    `json:"max_concurrency"`
	MaxQueue       int    `json:"max_queue"`
	RequestTimeout int    `json:"request_timeout"` // seconds
	Warm           int    `json:"warm"`
	WarmIdle       int    `json:"warm_idle"` // seconds
	SelfURL        string `json:"self_url"`
	LogFile        string `json:"log_file"`
	LogLevel       string `json:"log_level"`

	// File-only settings.
	APIKeys         []KeyConfig         `json:"api_keys"`
	Backends        []string            `json:"backends"` // enabled CLI backends, default claude and codex
	Models          map[string][]string `json:"models"`   // alias -> targets, tried in order
	Upstreams       []UpstreamConfig    `json:"upstreams"`
	ClaudeIsolate   *bool               `json:"claude_isolate"`
	ClaudeWebFetch  bool                `json:"claude_web_fetch"` // with web search, let claude fetch URLs from this machine
	CodexIsolate    *bool               `json:"codex_isolate"`
	ClaudeExtraArgs []string            `json:"claude_extra_args"`
	CodexExtraArgs  []string            `json:"codex_extra_args"`
	ResponseTTL     int                 `json:"response_ttl"` // seconds a response can be continued
	ResponseMax     int                 `json:"response_max"` // stored conversations, 0 disables
	MetricsPublic   *bool               `json:"metrics_public"`

	Detach bool `json:"-"`
}

func defaultConfig() Config {
	return Config{
		ListenAddr:     "127.0.0.1:8080",
		ClaudeBin:      "claude",
		CodexBin:       "codex",
		MaxConcurrency: 4,
		MaxQueue:       64,
		RequestTimeout: 300,
		Warm:           1,
		WarmIdle:       300,
		LogLevel:       "info",
		Backends:       []string{"claude", "codex"},
		ResponseTTL:    3600,
		ResponseMax:    1000,
	}
}

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// configPath finds -config before the flags are parsed, as the file sets
// their defaults.
func configPath(args []string) string {
	for i, a := range args {
		a = strings.TrimPrefix(a, "-")
		a = strings.TrimPrefix(a, "-")
		if a == "config" && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "config="); ok {
			return v
		}
	}
	return os.Getenv("CONFIG_FILE")
}

func loadConfig(args []string, stderr io.Writer) (Config, error) {
	cfg := defaultConfig()
	if path := configPath(args); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("reading the configuration: %w", err)
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	}
	env := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	envInt := func(key string, def int) int {
		if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
			return n
		}
		return def
	}
	fs := flag.NewFlagSet("llm-gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.String("config", "", "JSON configuration file (CONFIG_FILE)")
	fs.StringVar(&cfg.ListenAddr, "addr", env("LISTEN_ADDR", cfg.ListenAddr), "listen address (LISTEN_ADDR)")
	fs.StringVar(&cfg.ClaudeBin, "claude-bin", env("CLAUDE_BIN", cfg.ClaudeBin), "claude CLI path (CLAUDE_BIN)")
	fs.StringVar(&cfg.CodexBin, "codex-bin", env("CODEX_BIN", cfg.CodexBin), "codex CLI path (CODEX_BIN)")
	fs.StringVar(&cfg.WorkDir, "workdir", env("WORK_DIR", cfg.WorkDir), "working directory for the CLIs; default is an empty temp dir (WORK_DIR)")
	fs.StringVar(&cfg.APIKey, "api-key", env("API_KEY", cfg.APIKey), "if set, require 'Authorization: Bearer <key>' (API_KEY)")
	fs.IntVar(&cfg.MaxConcurrency, "max-concurrency", envInt("MAX_CONCURRENCY", cfg.MaxConcurrency), "max concurrent backend requests (MAX_CONCURRENCY)")
	fs.IntVar(&cfg.MaxQueue, "max-queue", envInt("MAX_QUEUE", cfg.MaxQueue), "max requests waiting for a free slot; more are refused with 503 (MAX_QUEUE)")
	fs.IntVar(&cfg.RequestTimeout, "timeout", envInt("REQUEST_TIMEOUT", cfg.RequestTimeout), "per-request timeout in seconds (REQUEST_TIMEOUT)")
	fs.IntVar(&cfg.Warm, "warm", envInt("WARM", cfg.Warm), "claude processes started ahead of time, kept idle for the next request; 0 disables (WARM)")
	fs.IntVar(&cfg.WarmIdle, "warm-idle", envInt("WARM_IDLE", cfg.WarmIdle), "seconds a warm claude process waits before it is stopped (WARM_IDLE)")
	fs.StringVar(&cfg.SelfURL, "self-url", env("SELF_URL", cfg.SelfURL), "base URL the CLIs reach this server at, for the client's functions; default from -addr on 127.0.0.1 (SELF_URL)")
	fs.BoolVar(&cfg.Detach, "detach", false, "run in the background, in a session of its own, so that closing the terminal does not stop it")
	fs.StringVar(&cfg.LogFile, "log-file", env("LOG_FILE", cfg.LogFile), "where -detach writes the log (LOG_FILE)")
	fs.StringVar(&cfg.LogLevel, "log-level", env("LOG_LEVEL", cfg.LogLevel), "debug, info, warn or error (LOG_LEVEL)")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if *showVersion {
		return cfg, errVersion
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return cfg, cfg.validate()
}

var errVersion = errors.New("version")

func (c *Config) validate() error {
	host, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return fmt.Errorf("invalid -addr %q: %w", c.ListenAddr, err)
	}
	if c.SelfURL == "" {
		c.SelfURL = "http://" + net.JoinHostPort("127.0.0.1", port)
	}
	c.SelfURL = strings.TrimRight(c.SelfURL, "/")
	if c.MaxConcurrency < 1 {
		return errors.New("max_concurrency must be at least 1")
	}
	if c.RequestTimeout < 1 {
		return errors.New("request_timeout must be at least 1")
	}
	if c.LogFile == "" {
		c.LogFile = defaultLogFile()
	}

	// The single key of -api-key joins the keys of the file.
	if c.APIKey != "" {
		c.APIKeys = append([]KeyConfig{{Name: "default", Key: c.APIKey}}, c.APIKeys...)
	}
	seen := map[string]bool{}
	for i := range c.APIKeys {
		k := &c.APIKeys[i]
		if k.KeyEnv != "" {
			k.Key = os.Getenv(k.KeyEnv)
		}
		if k.Name == "" {
			k.Name = fmt.Sprintf("key-%d", i+1)
		}
		if k.Key == "" {
			return fmt.Errorf("api key %q is empty", k.Name)
		}
		if seen[k.Name] {
			return fmt.Errorf("api key name %q is used twice", k.Name)
		}
		seen[k.Name] = true
	}

	reserved := map[string]bool{"claude": true, "codex": true}
	for _, u := range c.Upstreams {
		if !nameRE.MatchString(u.Name) || reserved[u.Name] {
			return fmt.Errorf("invalid upstream name %q", u.Name)
		}
		if !strings.HasPrefix(u.BaseURL, "http://") && !strings.HasPrefix(u.BaseURL, "https://") {
			return fmt.Errorf("upstream %q: base_url must be an http(s) URL", u.Name)
		}
		reserved[u.Name] = true
	}
	for _, b := range c.Backends {
		if b != "claude" && b != "codex" {
			return fmt.Errorf("unknown backend %q in backends: use claude or codex", b)
		}
	}
	if c.CodexIsolate == nil || *c.CodexIsolate {
		if err := checkCodexArgs(c.CodexExtraArgs); err != nil {
			return err
		}
	}
	for alias, targets := range c.Models {
		if !nameRE.MatchString(alias) || len(targets) == 0 {
			return fmt.Errorf("invalid model alias %q", alias)
		}
	}
	_ = host
	return nil
}

// loopback reports whether the server listens only on this machine.
func (c *Config) loopback() bool {
	host, _, _ := net.SplitHostPort(c.ListenAddr)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func defaultLogFile() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "llm-gateway", "llm-gateway.log")
}
