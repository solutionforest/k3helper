package cli

import (
	"strings"
	"testing"
)

func TestRootCmdVersion(t *testing.T) {
	var out strings.Builder
	root := NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "k3helper") {
		t.Errorf("output = %q, want version string", out.String())
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
