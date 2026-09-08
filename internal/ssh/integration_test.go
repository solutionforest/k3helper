//go:build integration

package ssh

import (
	"strings"
	"testing"
)

// sandboxNode returns connection details for sandbox VM 1 (server).
func sandboxNode() Node {
	return Node{Host: "127.0.0.1", Port: 2221, User: "sandbox", Key: "../../test/sandbox/ssh/id_ed25519"}
}

func TestDialAndRun(t *testing.T) {
	c, err := Dial(sandboxNode())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	out, code, err := c.Run("echo hello-k3helper")
	if err != nil || code != 0 {
		t.Fatalf("run: out=%q code=%d err=%v", out, code, err)
	}
	if !strings.Contains(out, "hello-k3helper") {
		t.Errorf("output %q missing expected text", out)
	}
}

func TestRunExitCode(t *testing.T) {
	c, _ := Dial(sandboxNode())
	defer c.Close()

	out, code, err := c.Run("exit 3")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if code != 3 {
		t.Errorf("code = %d, want 3 (out=%q)", code, out)
	}
}

func TestSudoRun(t *testing.T) {
	c, _ := Dial(sandboxNode())
	defer c.Close()

	out, code, err := c.SudoRun("whoami")
	if err != nil || code != 0 {
		t.Fatalf("sudo run: out=%q code=%d err=%v", out, code, err)
	}
	if !strings.Contains(out, "root") {
		t.Errorf("sudo whoami = %q, want root", out)
	}
}
