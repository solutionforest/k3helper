package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// testKey returns a throwaway host key.
func testKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// useTempKnownHosts points the package at a scratch known_hosts file.
func useTempKnownHosts(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	old := knownHostsPath
	knownHostsPath = path
	t.Cleanup(func() { knownHostsPath = old })
	return path
}

func TestHostKeyInsecureAcceptsAnything(t *testing.T) {
	cb, err := hostKeyCallback(HostKeyInsecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("anything:22", &net.TCPAddr{}, testKey(t)); err != nil {
		t.Errorf("insecure mode should accept any key, got %v", err)
	}
}

// An unknown host must be refused by default, with a message that says how to
// proceed rather than just "handshake failed".
func TestHostKeyVerifyRejectsUnknownHost(t *testing.T) {
	useTempKnownHosts(t)
	cb, err := hostKeyCallback(HostKeyVerify)
	if err != nil {
		t.Fatal(err)
	}
	err = cb("newhost:22", &net.TCPAddr{}, testKey(t))
	if err == nil {
		t.Fatal("unknown host was accepted")
	}
	var hkErr *HostKeyError
	if !asHostKeyError(err, &hkErr) {
		t.Fatalf("err = %T, want *HostKeyError", err)
	}
	if hkErr.Changed {
		t.Error("an unknown host is not a changed key")
	}
	for _, want := range []string{"not in known_hosts", "ssh-keyscan", "--accept-new-host-key", "SHA256:"} {
		if !strings.Contains(hkErr.Error(), want) {
			t.Errorf("message missing %q:\n%s", want, hkErr.Error())
		}
	}
}

// Trust-on-first-use records the key so the next connection succeeds without
// the flag.
func TestHostKeyAcceptNewRecordsAndThenVerifies(t *testing.T) {
	path := useTempKnownHosts(t)
	key := testKey(t)

	cb, err := hostKeyCallback(HostKeyAcceptNew)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("host.example:22", &net.TCPAddr{}, key); err != nil {
		t.Fatalf("accept-new should trust an unknown host: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatalf("known_hosts not written: %v (%q)", err, data)
	}

	// A fresh strict callback must now accept the same host.
	cb2, err := hostKeyCallback(HostKeyVerify)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb2("host.example:22", &net.TCPAddr{}, key); err != nil {
		t.Errorf("recorded host should verify: %v", err)
	}
}

// A changed key is the dangerous case: it must be refused even in
// accept-new mode, and the message must say so.
func TestHostKeyChangedIsRefusedEvenWithAcceptNew(t *testing.T) {
	path := useTempKnownHosts(t)
	original := testKey(t)
	line := knownhosts.Line([]string{knownhosts.Normalize("host.example:22")}, original)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []HostKeyMode{HostKeyVerify, HostKeyAcceptNew} {
		cb, err := hostKeyCallback(mode)
		if err != nil {
			t.Fatal(err)
		}
		err = cb("host.example:22", &net.TCPAddr{}, testKey(t)) // different key
		if err == nil {
			t.Fatalf("mode %v accepted a changed host key", mode)
		}
		var hkErr *HostKeyError
		if !asHostKeyError(err, &hkErr) {
			t.Fatalf("err = %T, want *HostKeyError", err)
		}
		if !hkErr.Changed {
			t.Error("a changed key must be reported as changed, not unknown")
		}
		if !strings.Contains(hkErr.Error(), "CHANGED") || !strings.Contains(hkErr.Error(), "ssh-keygen -R") {
			t.Errorf("message should warn and give the fix:\n%s", hkErr.Error())
		}
	}
}

// A missing known_hosts is the normal state on a fresh machine and must not
// be an error by itself.
func TestHostKeyVerifyCreatesMissingKnownHosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "known_hosts")
	old := knownHostsPath
	knownHostsPath = path
	t.Cleanup(func() { knownHostsPath = old })

	if _, err := hostKeyCallback(HostKeyVerify); err != nil {
		t.Fatalf("missing known_hosts should be created, got %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("known_hosts not created: %v", err)
	}
}

func TestDialLocalIgnoresHostKeyPolicy(t *testing.T) {
	// A local node never opens a connection, so verification cannot apply.
	c, err := Dial(Node{Local: true, HostKey: HostKeyVerify})
	if err != nil {
		t.Fatalf("local dial failed: %v", err)
	}
	defer c.Close()
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got, err := expandHome("~/.ssh/id_ed25519")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(home, ".ssh", "id_ed25519") {
		t.Errorf("expandHome = %q", got)
	}
	// absolute and relative paths pass through untouched
	for _, p := range []string{"/abs/key", "rel/key", ""} {
		if got, _ := expandHome(p); got != p {
			t.Errorf("expandHome(%q) = %q, want unchanged", p, got)
		}
	}
	if _, err := expandHome("~someone/key"); err == nil {
		t.Error("~user paths should be rejected rather than silently mishandled")
	}
}

func asHostKeyError(err error, target **HostKeyError) bool {
	for err != nil {
		if e, ok := err.(*HostKeyError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// A host recorded only under a different key algorithm (an old ssh-rsa entry
// while the server now offers ed25519) is not a changed key. Reporting it as
// interception told the user to delete a perfectly good record.
func TestDifferentAlgorithmIsNotAChangedKey(t *testing.T) {
	path := useTempKnownHosts(t)

	rsaKey := testRSAKey(t)
	line := knownhosts.Line([]string{knownhosts.Normalize("host.example:22")}, rsaKey)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cb, err := hostKeyCallback(HostKeyVerify)
	if err != nil {
		t.Fatal(err)
	}
	err = cb("host.example:22", &net.TCPAddr{}, testKey(t)) // ed25519
	if err == nil {
		t.Fatal("an unrecorded algorithm should still not connect silently")
	}
	var hkErr *HostKeyError
	if !asHostKeyError(err, &hkErr) {
		t.Fatalf("err = %T, want *HostKeyError", err)
	}
	if hkErr.Changed {
		t.Error("a different algorithm was reported as a CHANGED key — a false tampering alarm")
	}

	if !hkErr.KnownOtherAlgorithm {
		t.Error("should be reported as known-under-another-algorithm")
	}
	if !strings.Contains(hkErr.Error(), "not under the key type it offered") {
		t.Errorf("message should explain the situation:\n%s", hkErr.Error())
	}

	// Trust-on-first-use must NOT extend to a host that is already recorded:
	// an interceptor could simply offer a key type the record happens to lack.
	cb, err = hostKeyCallback(HostKeyAcceptNew)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("host.example:22", &net.TCPAddr{}, testKey(t)); err == nil {
		t.Error("accept-new silently trusted a new key type for an already-known host")
	}
	after, _ := os.ReadFile(path)
	if strings.Count(string(after), "\n") != 1 {
		t.Errorf("known_hosts was appended to: %q", string(after))
	}
}

// Trust-on-first-use still applies to a host that is genuinely unknown.
func TestAcceptNewStillTrustsCompletelyUnknownHosts(t *testing.T) {
	useTempKnownHosts(t)
	cb, err := hostKeyCallback(HostKeyAcceptNew)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("brand-new:22", &net.TCPAddr{}, testKey(t)); err != nil {
		t.Errorf("an unknown host should still be trusted on first use: %v", err)
	}
}

func testRSAKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := gossh.NewPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}
