//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// requireAdmin makes sure the installer can write Program Files and create the startup task.
func requireAdmin() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("run this from an Administrator PowerShell or Command Prompt")
	}
	return nil
}
