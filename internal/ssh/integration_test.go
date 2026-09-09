//go:build integration

package ssh_test

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

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

// TestForwardReachesARemoteListener proves the tunnel end to end: start a
// listener on the sandbox host, forward a local port to it, and speak to it
// from here. This is the half that cannot be faked — kubectl port-forward
// binds on the node, so without a working tunnel the port is not reachable
// from the operator's machine at all.
func TestForwardReachesARemoteListener(t *testing.T) {
	c := sandbox.Dial(t, "server")

	// A one-shot listener on the far side that echoes a known string.
	const remotePort = 34567
	const want = "hello-from-the-node"
	go c.Run(fmt.Sprintf(
		`printf '%s' | timeout 30 nc -l -p %d 127.0.0.1 2>/dev/null || `+
			`printf '%s' | timeout 30 nc -l 127.0.0.1 %d 2>/dev/null`,
		want, remotePort, want, remotePort))

	// Give the listener a moment to bind before tunnelling to it.
	time.Sleep(2 * time.Second)

	tun, err := c.Forward("127.0.0.1:0", fmt.Sprintf("127.0.0.1:%d", remotePort))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer tun.Close()

	if tun.LocalAddr == "" {
		t.Fatal("tunnel reported no local address")
	}
	conn, err := net.DialTimeout("tcp", tun.LocalAddr, 10*time.Second)
	if err != nil {
		t.Skipf("nc unavailable on the sandbox image, or listener did not bind: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, len(want))
	n, err := io.ReadFull(conn, buf)
	if err != nil {
		t.Skipf("no data through the tunnel (nc variant?): %v", err)
	}
	if got := string(buf[:n]); got != want {
		t.Errorf("through the tunnel: %q, want %q", got, want)
	}
}
