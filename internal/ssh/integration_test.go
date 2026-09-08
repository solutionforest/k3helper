//go:build integration

package ssh_test

import (
	"os"
	"strings"
	"testing"

	"github.com/solutionforest/k3helper/internal/sandbox"
)

func TestDialAndRun(t *testing.T) {
	c := sandbox.Dial(t, "server")

	out, code, err := c.Run("echo hello-k3helper")
	if err != nil || code != 0 {
		t.Fatalf("run: out=%q code=%d err=%v", out, code, err)
	}
	if !strings.Contains(out, "hello-k3helper") {
		t.Errorf("output %q missing expected text", out)
	}
}

func TestRunExitCode(t *testing.T) {
	c := sandbox.Dial(t, "server")

	out, code, err := c.Run("exit 3")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if code != 3 {
		t.Errorf("code = %d, want 3 (out=%q)", code, out)
	}
}

func TestSudoRun(t *testing.T) {
	c := sandbox.Dial(t, "server")

	out, code, err := c.SudoRun("whoami")
	if err != nil || code != 0 {
		t.Fatalf("sudo run: out=%q code=%d err=%v", out, code, err)
	}
	if !strings.Contains(out, "root") {
		t.Errorf("sudo whoami = %q, want root", out)
	}
}

// TestWriteAndRemoveFile covers the upload path that deploy and
// verify --dry-run-server rely on.
func TestWriteAndRemoveFile(t *testing.T) {
	c := sandbox.Dial(t, "server")

	const remote = "/tmp/k3helper-writefile-test.yaml"
	body := []byte("hello: world\nlist:\n  - a\n  - b\n")
	if err := c.WriteFile(remote, body, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	out, code, err := c.Run("cat '" + remote + "'")
	if err != nil || code != 0 {
		t.Fatalf("cat: code=%d err=%v", code, err)
	}
	if out != string(body) {
		t.Errorf("contents = %q, want %q", out, string(body))
	}
	mode, _, _ := c.Run("stat -c %a '" + remote + "'")
	if strings.TrimSpace(mode) != "600" {
		t.Errorf("mode = %q, want 600", strings.TrimSpace(mode))
	}

	if err := c.RemoveFile(remote); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if _, code, _ := c.Run("test -e '" + remote + "'"); code == 0 {
		t.Error("file still present after RemoveFile")
	}
}

// TestWriteFileRejectsUnsafePaths: the remote path is interpolated into a
// single-quoted shell word, so quotes must never reach the shell.
func TestWriteFileRejectsUnsafePaths(t *testing.T) {
	c := sandbox.Dial(t, "server")

	for _, bad := range []string{"", "/tmp/x'; touch /tmp/pwned; '", "/tmp/x\ny"} {
		if err := c.WriteFile(bad, []byte("x"), 0o600); err == nil {
			t.Errorf("WriteFile(%q) = nil, want rejection", bad)
		}
		if err := c.RemoveFile(bad); err == nil {
			t.Errorf("RemoveFile(%q) = nil, want rejection", bad)
		}
	}
	if _, code, _ := c.Run("test -e /tmp/pwned"); code == 0 {
		os.Remove("/tmp/pwned")
		t.Fatal("injection succeeded: /tmp/pwned was created on the remote host")
	}
}
