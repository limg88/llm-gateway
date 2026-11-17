//go:build unix

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// detach starts the server again in the background, in a session of its
// own, and returns once it listens on dialAddr.
func detach(dialAddr, logFile string) error {
	exe, err := os.Executable()
	if err != nil {
		return errors.New("cannot find executable path")
	}
	if conn, err := net.DialTimeout("tcp", dialAddr, time.Second); err == nil {
		conn.Close()
		return fmt.Errorf("%s is already in use", dialAddr)
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return err
	}
	log, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()

	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), detachedEnv+"=1")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("llm-gateway stopped at start (%v), see %s", err, logFile)
		case <-deadline:
			return fmt.Errorf("llm-gateway (pid %d) is not listening on %s after 10s, see %s", cmd.Process.Pid, dialAddr, logFile)
		case <-time.After(100 * time.Millisecond):
		}
		if conn, err := net.DialTimeout("tcp", dialAddr, 200*time.Millisecond); err == nil {
			conn.Close()
			pid := cmd.Process.Pid
			fmt.Printf("llm-gateway running in the background, pid %d, listening on %s\nlog: %s\nstop: kill %d\n", pid, dialAddr, logFile, pid)
			return nil
		}
	}
}
