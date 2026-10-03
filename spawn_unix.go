//go:build !windows

package main

import "syscall"

// detachedProcAttr starts the monitor in its own session so it survives closing the terminal.
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
