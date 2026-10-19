//go:build !windows

package main

import "fmt"

// On Linux the installer writes a systemd unit instead.
func serviceCommand(cmd, configPath string) error {
	return fmt.Errorf("%s is for Windows; on Linux the install script sets up systemd", cmd)
}

func runAsService(configPath string) bool { return false }
