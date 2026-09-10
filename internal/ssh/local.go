package ssh

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
)

// This file backs nodes marked Local: k3helper running directly on the box it
// is managing. Useful when the machine is only reachable through a provider's
// browser console, so there is no way to SSH in from a laptop.
//
// Each helper mirrors the (combined output, exit code, error) contract of the
// matching Client method so callers cannot tell the two transports apart.

const localShell = "/bin/sh"

// errNoLocalShell explains a `local: true` node on a platform that has no
// POSIX shell. k3helper builds and runs on Windows, but the local transport
// means "run these commands on the machine k3helper is on" — and every command
// it runs is a Linux one, aimed at a node that hosts k3s. There is nothing
// sensible to do here except say so.
//
// The bare failure is "exec: /bin/sh: executable file not found in %PATH%",
// which reads like a broken install rather than a targets file describing
// something that cannot exist.
func errNoLocalShell() error {
	return fmt.Errorf("local: true needs a POSIX shell at %s, which %s does not have — "+
		"a local node is the machine k3helper runs on, and k3s nodes are Linux hosts. "+
		"Describe the node with host/user/key to reach it over SSH instead",
		localShell, runtime.GOOS)
}

func runLocal(cmd string) (string, int, error) {
	if runtime.GOOS == "windows" {
		return "", -1, errNoLocalShell()
	}
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
	if runtime.GOOS == "windows" {
		return -1, errNoLocalShell()
	}
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

// writeFileStreamLocal backs WriteFileFrom for a local node.
func writeFileStreamLocal(path string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Chmod(path, mode.Perm())
}

func removeFileLocal(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rm %s: %w", path, err)
	}
	return nil
}
