//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// replaceAndRestart swaps in the new program and starts it. Windows can't overwrite a
// running program, but it can rename it, so the old one is moved aside and cleaned up by
// the new one when it starts.
func replaceAndRestart(exe, newPath string) error {
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("moving the old program aside: %w", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		os.Rename(old, exe)
		return fmt.Errorf("putting the new program in place: %w", err)
	}
	args := os.Args[1:]
	if !hasArg(args, "-daemon") {
		args = append(args, "-daemon")
	}
	closeListeners() // let the new copy take over the control socket and the client port
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = detachedProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the new version: %w", err)
	}
	os.Exit(0)
	return nil
}

func hasArg(args []string, a string) bool {
	for _, x := range args {
		if x == a || x == "-"+a {
			return true
		}
	}
	return false
}
