//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"os"
	"syscall"
)

func lockLedger(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }
