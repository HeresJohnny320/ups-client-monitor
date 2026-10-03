//go:build windows

package main

import "syscall"

const detachedProcess = 0x00000008

// detachedProcAttr starts the monitor without a console so it survives closing the terminal.
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}
