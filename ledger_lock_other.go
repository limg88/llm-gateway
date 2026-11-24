//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package main

import (
	"errors"
	"os"
)

func lockLedger(*os.File) error { return errors.New("ledger locking unavailable on this platform") }
