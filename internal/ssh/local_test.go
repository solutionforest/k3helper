package ssh

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// requireLocalShell skips a test that needs the POSIX shell the local
// transport runs commands through.
//
// Local mode means "manage the machine k3helper is on", and the commands it
// runs are Linux ones aimed at a k3s node — so it is a Unix-only path by
// design, not an unimplemented one. Windows gets a clear error instead, which
// TestLocalOnWindowsExplainsItself covers.
func requireLocalShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("local mode needs /bin/sh; on Windows it is refused with an explanation instead")
	}
}

// dialLocal is the path a targets file with `local: true` takes.
func dialLocal(t *testing.T) *Client {
	t.Helper()
	c, err := Dial(Node{Local: true, Host: "localhost"})
	if err != nil {
		t.Fatalf("Dial local: %v", err)
	}
	return c
}

func TestDialLocalDoesNotConnect(t *testing.T) {
	c := dialLocal(t)
	if c.conn != nil {
		t.Error("local client opened an SSH connection")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestLocalRunExitCodes(t *testing.T) {
	requireLocalShell(t)
	c := dialLocal(t)

	out, code, err := c.Run("echo hello")
	if err != nil || code != 0 {
		t.Fatalf("Run: code=%d err=%v", code, err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Errorf("out = %q, want %q", out, "hello")
	}

	// A non-zero exit is a result, not a Go error — same contract as SSH.
	out, code, err = c.Run("echo oops >&2; exit 3")
	if err != nil {
		t.Fatalf("unexpected error for failing command: %v", err)
	}
	if code != 3 {
		t.Errorf("code = %d, want 3", code)
	}
	if !strings.Contains(out, "oops") {
		t.Errorf("stderr not captured in combined output: %q", out)
	}
}

func TestLocalStream(t *testing.T) {
	requireLocalShell(t)
	c := dialLocal(t)
	var buf bytes.Buffer
	code, err := c.Stream("echo streamed; exit 7", &buf)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if code != 7 {
		t.Errorf("code = %d, want 7", code)
	}
	if !strings.Contains(buf.String(), "streamed") {
		t.Errorf("stream output = %q", buf.String())
	}
}

func TestLocalSudoPrefix(t *testing.T) {
	requireLocalShell(t)
	c := dialLocal(t)
	want := "sudo -n "
	if os.Geteuid() == 0 {
		want = "" // minimal console-only images often ship no sudo binary
	}
	if got := c.SudoPrefix(); got != want {
		t.Errorf("SudoPrefix() = %q, want %q", got, want)
	}

	remote, err := Dial(Node{Host: "example.invalid", Local: false, User: "x", Key: "k"})
	if err == nil {
		remote.Close()
		t.Fatal("expected dial to a bogus host to fail")
	}
}

func TestLocalWriteAndRemoveFile(t *testing.T) {
	requireLocalShell(t)
	c := dialLocal(t)
	path := filepath.Join(t.TempDir(), "manifest.yaml")

	if err := c.WriteFile(path, []byte("kind: Pod\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "kind: Pod\n" {
		t.Errorf("contents = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}

	if err := c.RemoveFile(path); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still present after RemoveFile: %v", err)
	}
	// Removing an already-gone file is not an error.
	if err := c.RemoveFile(path); err != nil {
		t.Errorf("second RemoveFile: %v", err)
	}
}

func TestLocalWriteFileRejectsBadPath(t *testing.T) {
	requireLocalShell(t)
	c := dialLocal(t)
	if err := c.WriteFile("/tmp/bad'name", []byte("x"), 0o600); err == nil {
		t.Error("expected quoted path to be rejected on the local transport too")
	}
}

// On Windows the local transport must explain itself rather than failing with
// "exec: /bin/sh: executable file not found", which reads like a broken
// install rather than a targets file describing something that cannot exist.
func TestLocalOnWindowsExplainsItself(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the refusal only happens on Windows")
	}
	c := dialLocal(t)
	_, _, err := c.Run("echo hi")
	if err == nil {
		t.Fatal("local mode ran a command on a platform with no POSIX shell")
	}
	for _, want := range []string{"local: true", "over SSH"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
