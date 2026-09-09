package cli

import (
	"strings"
	"testing"
)

// The version must land on stdout, not stderr: `VER=$(k3helper version)` is how
// scripts and the release workflow pin it, and cobra's cmd.Printf writes to
// stderr, so pointing both streams at one buffer would hide the difference.
func TestRootCmdVersionGoesToStdout(t *testing.T) {
	var out, errOut strings.Builder
	root := NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "k3helper") {
		t.Errorf("stdout = %q, want the version string", out.String())
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want empty", errOut.String())
	}
}

func TestRootCmdHelpListsSubcommands(t *testing.T) {
	var out strings.Builder
	root := NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"vm", "verify", "gen", "deploy", "check", "doctor", "tui"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help output missing %q", want)
		}
	}
}

func TestRootCmdUnknownCommand(t *testing.T) {
	var errOut strings.Builder
	root := NewRootCmd()
	root.SetOut(&strings.Builder{})
	root.SetErr(&errOut)
	root.SetArgs([]string{"definitely-not-a-command"})
	if err := root.Execute(); err == nil {
		t.Error("expected error for unknown command")
	}
}
