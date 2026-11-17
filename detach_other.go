//go:build !unix

package main

import "errors"

func detach(dialAddr, logFile string) error {
	return errors.New("-detach is not supported on this system")
}
