package ssh

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// This file backs nodes marked Local: k3helper running directly on the box it
// is managing. Useful when the machine is only reachable through a provider's
// browser console, so there is no way to SSH in from a laptop.
//
// Each helper mirrors the (combined output, exit code, error) contract of the
// matching Client method so callers cannot tell the two transports apart.

const localShell = "/bin/sh"

func runLocal(cmd string) (string, int, error) {
	out, err := exec.Command(localShell, "-c", cmd).CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return string(out), exitErr.ExitCode(), nil
	}
	if err != nil {
		return string(out), -1, fmt.Errorf("local exec: %w", err)
	}
	return string(out), 0, nil
}

func streamLocal(cmd string, w io.Writer) (int, error) {
	c := exec.Command(localShell, "-c", cmd)
	c.Stdout = w
	c.Stderr = w
	err := c.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, fmt.Errorf("local exec: %w", err)
	}
	return 0, nil
}

func writeFileLocal(path string, data []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, data, mode.Perm()); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// os.WriteFile only applies mode on create, and the process umask masks it;
	// chmod pins the caller's requested bits either way.
	if err := os.Chmod(path, mode.Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

func removeFileLocal(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rm %s: %w", path, err)
	}
	return nil
}
