//go:build !windows

package main

import (
	"errors"
	"os"
)

// requireAdmin makes sure the installer can write system folders and manage services.
func requireAdmin() error {
	if os.Geteuid() != 0 {
		return errors.New("run this as root, e.g. with sudo")
	}
	return nil
}
