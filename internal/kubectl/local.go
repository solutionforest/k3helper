// Package kubectl runs kubectl on this machine against a kubeconfig, as an
// alternative to running it on a cluster node over SSH.
//
// It exists for managed Kubernetes — EKS, GKE, AKS, and anything else where
// there is no SSH into a control-plane node and no /etc/kubernetes/admin.conf
// to read. Those clusters hand the operator a kubeconfig and nothing else.
//
// The rest of k3helper builds shell command strings and hands them to an
// Executor. This one does not have a shell to hand them to: the whole point of
// the local transport is that it also works on Windows, where there is no
// /bin/sh. So Run parses the command instead of interpreting it, and accepts
// only the kubectl invocations this package itself produced.
package kubectl

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Binary is the kubectl executable to run. A name (rather than a path) is
// resolved through PATH by os/exec.
const Binary = "kubectl"

// ErrHostLayer is returned for commands that are not kubectl invocations —
// disk usage, systemd state, and the rest of the host layer. A kubeconfig
// reaches an API server, not a machine, so these can never work here.
var ErrHostLayer = errors.New("host-layer command not available on a kubeconfig cluster")

// Local runs kubectl on this machine.
type Local struct {
	// Kubeconfig is the path to the kubeconfig file. Required: an empty value
	// would silently fall back to whatever ~/.kube/config points at, which is
	// how a command meant for staging lands on production.
	Kubeconfig string
	// Context names a context within that file. Empty uses its current-context.
	Context string
}

// Base is the kubectl invocation every command from this transport starts
// with. kube.Builder uses it instead of probing the node for a distribution:
// there is no node to probe.
func (l Local) Base() string {
	b := Binary + " --kubeconfig " + quote(l.Kubeconfig)
	if l.Context != "" {
		b += " --context " + quote(l.Context)
	}
	return b
}

// Available reports whether kubectl can be found and run. Callers check this
// once at startup: "kubectl: executable file not found in $PATH" from the
// middle of a diagnosis is a worse message than one at the front door.
func Available() error {
	path, err := exec.LookPath(Binary)
	if err != nil {
		return fmt.Errorf("kubectl not found in PATH — a kubeconfig cluster is driven through kubectl, "+
			"so install it and try again: %w", err)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("kubectl at %s is not usable: %w", path, err)
	}
	return nil
}

// Run executes one command and returns its output, exit code and error, with
// the same contract as ssh.Client.Run: a non-zero exit is reported in the code
// and is not an error.
//
// The command must be a kubectl invocation produced from Base. Anything else
// is refused with exit 127 rather than attempted, so a host-layer probe fails
// where it is written instead of somewhere further down the call stack.
func (l Local) Run(cmd string) (string, int, error) {
	args, mode, err := l.parse(cmd)
	if err != nil {
		if errors.Is(err, ErrHostLayer) {
			// 127 is what a shell reports for a command it cannot run, which is
			// what callers that probe with `command -v` already expect.
			return err.Error(), 127, nil
		}
		return "", -1, err
	}
	c := exec.Command(Binary, args...)
	var out []byte
	if mode == stderrDrop {
		// Callers add 2>/dev/null before parsing JSON, because kubectl prints
		// deprecation warnings to stderr and a merged stream would put them in
		// front of the document. Honour that: drop stderr, keep stdout.
		out, err = c.Output()
	} else {
		out, err = c.CombinedOutput()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// A non-zero exit is a result, not a failure: `kubectl diff` reports 1
		// for "there are differences" and callers read the code.
		return string(out), exitErr.ExitCode(), nil
	}
	if err != nil {
		return string(out), -1, fmt.Errorf("run kubectl: %w", err)
	}
	return string(out), 0, nil
}

// stderrMode is what to do with the child's stderr, decided by the redirection
// the caller wrote at the end of the command.
type stderrMode int

const (
	stderrMerge stderrMode = iota // default, and `2>&1`: what SSH does
	stderrDrop                    // `2>/dev/null`
)

// parse turns a command string into kubectl arguments.
func (l Local) parse(cmd string) ([]string, stderrMode, error) {
	if l.Kubeconfig == "" {
		return nil, stderrMerge, fmt.Errorf("kubectl transport: no kubeconfig configured")
	}
	rest := strings.TrimSpace(cmd)

	// Strip the trailing redirection first: it is punctuation for the shell we
	// are standing in for, not an argument to kubectl.
	mode := stderrMerge
	switch {
	case strings.HasSuffix(rest, "2>/dev/null"):
		mode = stderrDrop
		rest = strings.TrimSpace(strings.TrimSuffix(rest, "2>/dev/null"))
	case strings.HasSuffix(rest, "2>&1"):
		rest = strings.TrimSpace(strings.TrimSuffix(rest, "2>&1"))
	}

	base := l.Base()
	if !strings.HasPrefix(rest, base) {
		return nil, mode, fmt.Errorf("%w: %s", ErrHostLayer, firstWord(rest))
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, base))

	args, err := split(rest)
	if err != nil {
		return nil, mode, err
	}
	// Re-attach the flags from Base as real argv entries. They are quoted in
	// the string form and must not reach kubectl with the quotes still on.
	full := []string{"--kubeconfig", l.Kubeconfig}
	if l.Context != "" {
		full = append(full, "--context", l.Context)
	}
	return append(full, args...), mode, nil
}

// firstWord is used to name the refused command in an error.
func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "(empty command)"
	}
	return s
}

// split breaks a command tail into arguments, honouring the single quoting
// that kube.shellQuote produces: '...' with an embedded quote written '\”.
//
// This is not a general shell parser and deliberately refuses what it does not
// understand. Every command that reaches it was built by k3helper from a small
// set of format strings; a pipe or a subshell here means a caller has grown a
// shape this transport cannot honour, and failing loudly is how that gets
// noticed before it silently drops half a command.
func split(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		started bool
	)
	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ', '\t':
			flush()
		case '\'':
			started = true
			// Consume to the closing quote. '\'' is not an escape inside a
			// single-quoted string; it is a close, a literal quote, and a
			// reopen — which this loop handles by simply continuing.
			i++
			for i < len(s) && s[i] != '\'' {
				cur.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return nil, fmt.Errorf("kubectl transport: unbalanced quote in %q", s)
			}
		case '"':
			started = true
			i++
			for i < len(s) && s[i] != '"' {
				cur.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return nil, fmt.Errorf("kubectl transport: unbalanced quote in %q", s)
			}
		case '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				started = true
			}
		case '|', ';', '&', '>', '<', '`', '$', '(', ')':
			return nil, fmt.Errorf("kubectl transport: shell metacharacter %q in %q "+
				"(this transport runs kubectl directly and has no shell)", string(c), s)
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	flush()
	return args, nil
}

// quote renders a value for the *string* form of the command, which is what
// gets prefix-matched and printed. Nothing ever hands this to a shell.
func quote(s string) string {
	if s == "" || strings.ContainsAny(s, " \t'\"\\") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

// --- file staging ---
//
// deploy and `verify --live` upload a manifest and then apply it by path,
// because in SSH mode the file the operator names is on their machine and
// kubectl reads it on the cluster's. Here both are the same machine, so
// "upload" is a write to the local temp directory. The interface has to match
// ssh.Client either way, or every caller needs a mode switch.

// StageDir is this machine's temp directory. kyaml asks the transport for it
// rather than assuming /tmp, which a Windows machine does not have.
func (l Local) StageDir() string { return os.TempDir() }

// WriteFile writes a staged manifest.
func (l Local) WriteFile(path string, data []byte, mode os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, mode.Perm()); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// os.WriteFile only applies mode on create, and the umask masks it; chmod
	// pins the requested bits either way. Manifests are written 0600 and can
	// carry Secrets.
	if err := os.Chmod(path, mode.Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// RemoveFile deletes a staged manifest.
func (l Local) RemoveFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rm %s: %w", path, err)
	}
	return nil
}

// Close exists so callers can treat this like an ssh.Client and defer a close
// without caring which transport they hold. There is nothing to close.
func (l Local) Close() error { return nil }
