//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// replaceAndRestart swaps in the new program and runs it in this same process. The
// process ID doesn't change, so systemd, launchd and pfSense's service keep tracking it.
func replaceAndRestart(exe, newPath string) error {
	if err := swapBinary(exe, newPath); err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

// swapBinary puts the new program in place of the old one in a single step.
func swapBinary(exe, newPath string) error {
	if err := os.Rename(newPath, exe); err != nil {
		return fmt.Errorf("replacing %s: %w", exe, err)
	}
	return nil
}
