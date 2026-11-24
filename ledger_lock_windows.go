//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func lockLedger(f *os.File) error {
	lock := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	var offset syscall.Overlapped
	result, _, err := lock.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&offset)))
	if result == 0 {
		return err
	}
	return nil
}
