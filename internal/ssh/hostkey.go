package ssh

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyMode decides how a server's host key is checked.
type HostKeyMode int

const (
	// HostKeyVerify checks the key against known_hosts and refuses to connect
	// on a miss or a mismatch. This is the default.
	HostKeyVerify HostKeyMode = iota
	// HostKeyAcceptNew trusts a host the first time it is seen and appends it
	// to known_hosts, but still refuses if a recorded key later changes.
	HostKeyAcceptNew
	// HostKeyInsecure accepts any key. Only for throwaway environments.
	HostKeyInsecure
)

// knownHostsPath is the file host keys are read from and appended to.
// Overridable for tests and for callers that keep a separate file.
var knownHostsPath = defaultKnownHosts()

func defaultKnownHosts() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}

// appendMu serialises writes so two nodes learned in parallel cannot
// interleave lines in known_hosts.
var appendMu sync.Mutex

// HostKeyError explains a verification failure in terms of what to do next.
// The distinction matters: an unknown host is a first connection, a changed
// key may be an attack.
type HostKeyError struct {
	Host        string
	Fingerprint string
	Changed     bool
	err         error
}

func (e *HostKeyError) Error() string {
	if e.Changed {
		return fmt.Sprintf(
			"host key for %s has CHANGED (now %s).\n"+
				"This can mean the machine was rebuilt — or that the connection is being intercepted.\n"+
				"If you rebuilt it: ssh-keygen -R %q\n"+
				"Then reconnect. Do not pass --insecure-host-key to silence this without checking.",
			e.Host, e.Fingerprint, hostOnly(e.Host))
	}
	return fmt.Sprintf(
		"host %s is not in known_hosts (key %s).\n"+
			"Verify the fingerprint out of band, then either:\n"+
			"  ssh-keyscan -H %s >> ~/.ssh/known_hosts\n"+
			"or re-run with --accept-new-host-key to trust it on first use,\n"+
			"or --insecure-host-key to skip verification entirely.",
		e.Host, e.Fingerprint, hostOnly(e.Host))
}

func (e *HostKeyError) Unwrap() error { return e.err }

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// hostKeyCallback builds the verification callback for a mode.
func hostKeyCallback(mode HostKeyMode) (gossh.HostKeyCallback, error) {
	if mode == HostKeyInsecure {
		return gossh.InsecureIgnoreHostKey(), nil
	}
	if knownHostsPath == "" {
		return nil, fmt.Errorf("cannot locate ~/.ssh/known_hosts; pass --insecure-host-key to skip verification")
	}
	// knownhosts.New fails on a missing file, which is the normal state on a
	// fresh machine. Create it so first use works.
	if err := ensureKnownHosts(knownHostsPath); err != nil {
		return nil, err
	}
	verify, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", knownHostsPath, err)
	}

	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		err := verify(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
			// The host is recorded with a different key: never auto-accept.
			return &HostKeyError{
				Host:        hostname,
				Fingerprint: gossh.FingerprintSHA256(key),
				Changed:     true,
				err:         err,
			}
		}
		if errors.As(err, &keyErr) {
			// Unknown host.
			if mode == HostKeyAcceptNew {
				if aerr := appendKnownHost(hostname, key); aerr != nil {
					return fmt.Errorf("trust %s on first use: %w", hostname, aerr)
				}
				return nil
			}
			return &HostKeyError{
				Host:        hostname,
				Fingerprint: gossh.FingerprintSHA256(key),
				err:         err,
			}
		}
		return err
	}, nil
}

func ensureKnownHosts(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	return f.Close()
}

// appendKnownHost records a host key for trust-on-first-use.
func appendKnownHost(hostname string, key gossh.PublicKey) error {
	appendMu.Lock()
	defer appendMu.Unlock()

	f, err := os.OpenFile(knownHostsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, err = f.WriteString(line)
	return err
}
