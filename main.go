// llm-gateway serves the OpenAI and Anthropic HTTP APIs on top of the
// claude and codex command-line tools, and of any OpenAI-compatible API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

const detachedEnv = "LLM_GATEWAY_DETACHED"

func main() {
	cfg, err := loadConfig(os.Args[1:], os.Stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		os.Exit(0)
	case errors.Is(err, errVersion):
		fmt.Println("llm-gateway", version)
		os.Exit(0)
	case err != nil:
		fmt.Fprintln(os.Stderr, "llm-gateway:", err)
		os.Exit(2)
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		fmt.Fprintln(os.Stderr, "llm-gateway: invalid log level:", cfg.LogLevel)
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if cfg.Detach && os.Getenv(detachedEnv) == "" {
		_, port, _ := net.SplitHostPort(cfg.ListenAddr)
		if err := detach(net.JoinHostPort("127.0.0.1", port), cfg.LogFile); err != nil {
			fmt.Fprintln(os.Stderr, "llm-gateway:", err)
			os.Exit(1)
		}
		return
	}

	if cfg.WorkDir == "" {
		dir, err := os.MkdirTemp("", "llm-gateway-")
		if err != nil {
			slog.Error("creating work dir", "err", err)
			os.Exit(1)
		}
		defer os.RemoveAll(dir)
		cfg.WorkDir = dir
	}

	srv := NewServer(cfg)
	defer srv.Close()

	if len(cfg.APIKeys) == 0 && !cfg.loopback() {
		slog.Warn("no API key is set and the server is reachable from other machines: anyone who reaches it can use your CLI accounts")
	}

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdown); err != nil {
			httpSrv.Close()
		}
	}()

	slog.Info("listening", "addr", cfg.ListenAddr, "workdir", cfg.WorkDir, "backends", srv.backendNames(),
		"warm", cfg.Warm, "idle", cfg.WarmIdle, "keys", len(cfg.APIKeys), "version", version)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server error", "err", err)
		srv.Close()
		os.Exit(1)
	}
}
