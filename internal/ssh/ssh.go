package ssh

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// Node describes how to reach a machine (mirrors config.Node without import cycle).
type Node struct {
	Host string
	Port int
	User string
	Key  string
	// Local marks the machine k3helper itself is running on. Commands are
	// executed through /bin/sh instead of an SSH connection, so no sshd,
	// key or loopback network access is required.
	Local bool
	// HostKey selects how the server's host key is verified. The zero value
	// verifies against known_hosts.
	HostKey HostKeyMode
}

// Executor is the minimal command-execution surface (satisfied by *Client).
type Executor interface {
	Run(cmd string) (string, int, error)
}

// Client wraps an SSH connection to one node, or local /bin/sh execution
// when the node is marked Local.
type Client struct {
	conn  *gossh.Client
	node  Node
	local bool
}

// Dial connects to the node using private-key auth. For a Local node it
// returns a client that shells out on this machine without connecting.
func Dial(n Node) (*Client, error) {
	if n.Local {
		return &Client{node: n, local: true}, nil
	}
	if n.Port == 0 {
		n.Port = 22
	}
	keyPath, err := expandHome(n.Key)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read ssh key %s: %w", keyPath, err)
	}
	signer, err := gossh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key %s: %w", keyPath, err)
	}
	callback, err := hostKeyCallback(n.HostKey)
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("%s:%d", n.Host, n.Port)
	cfg := &gossh.ClientConfig{
		User:            n.User,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: callback,
		Timeout:         10 * time.Second,
	}
	conn, err := gossh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s@%s: %w", n.User, addr, err)
	}
	return &Client{conn: conn, node: n}, nil
}

// Run executes a command and returns stdout+stderr combined and the exit code.
func (c *Client) Run(cmd string) (string, int, error) {
	if c.local {
		return runLocal(cmd)
	}
	sess, err := c.conn.NewSession()
	if err != nil {
		return "", -1, fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	code := 0
	if exitErr, ok := err.(*gossh.ExitError); ok {
		code = exitErr.ExitStatus()
		err = nil
	} else if err != nil {
		return string(out), -1, err
	}
	return string(out), code, nil
}

// SudoRun executes a command with sudo (non-interactive; sandbox hosts have passwordless sudo).
func (c *Client) SudoRun(cmd string) (string, int, error) {
	return c.Run(c.SudoPrefix() + cmd)
}

// SudoPrefix returns the prefix needed to run a command as root on this node.
// It is empty when we are already root on a local node — minimal images
// reachable only through a browser console often have no sudo binary at all.
func (c *Client) SudoPrefix() string {
	if c.local && os.Geteuid() == 0 {
		return ""
	}
	return "sudo -n "
}

// Stream executes a command, streaming combined output to w. Returns exit code.
func (c *Client) Stream(cmd string, w io.Writer) (int, error) {
	if c.local {
		return streamLocal(cmd, w)
	}
	sess, err := c.conn.NewSession()
	if err != nil {
		return -1, fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()
	sess.Stdout = w
	sess.Stderr = w
	err = sess.Run(cmd)
	code := 0
	if exitErr, ok := err.(*gossh.ExitError); ok {
		code = exitErr.ExitStatus()
		err = nil
	}
	return code, err
}

// WriteFile creates remotePath on the node with the given contents and mode.
// Contents are streamed over stdin, so manifest size is not bounded by ARG_MAX.
func (c *Client) WriteFile(remotePath string, data []byte, mode os.FileMode) error {
	if err := validRemotePath(remotePath); err != nil {
		return err
	}
	if c.local {
		return writeFileLocal(remotePath, data, mode)
	}
	sess, err := c.conn.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()
	sess.Stdin = bytes.NewReader(data)
	var errBuf bytes.Buffer
	sess.Stderr = &errBuf
	cmd := fmt.Sprintf("umask 077 && cat > '%s' && chmod %o '%s'", remotePath, mode.Perm(), remotePath)
	if err := sess.Run(cmd); err != nil {
		return fmt.Errorf("write %s: %w: %s", remotePath, err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// WriteFileFrom streams a file to the node instead of holding it in memory.
//
// WriteFile takes a []byte, which is fine for a manifest and wrong for a k3s
// airgap image archive: those run to a few hundred megabytes, and buffering
// one per node to send it a chunk at a time serves no purpose. Progress, when
// given, is called with the running byte count so a long upload can say what
// it is doing rather than appearing hung.
func (c *Client) WriteFileFrom(remotePath string, r io.Reader, mode os.FileMode, progress func(sent int64)) error {
	if err := validRemotePath(remotePath); err != nil {
		return err
	}
	if c.local {
		return writeFileStreamLocal(remotePath, r, mode)
	}
	sess, err := c.conn.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()
	sess.Stdin = &countingReader{r: r, cb: progress}
	var errBuf bytes.Buffer
	sess.Stderr = &errBuf
	cmd := fmt.Sprintf("umask 077 && cat > '%s' && chmod %o '%s'", remotePath, mode.Perm(), remotePath)
	if err := sess.Run(cmd); err != nil {
		return fmt.Errorf("write %s: %w: %s", remotePath, err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// countingReader reports progress as the bytes go past.
type countingReader struct {
	r    io.Reader
	n    int64
	cb   func(int64)
	next int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	// Report every 8MB rather than every read: a callback per 32KB chunk turns
	// a progress line into a flood.
	if c.cb != nil && c.n >= c.next {
		c.next = c.n + 8<<20
		c.cb(c.n)
	}
	return n, err
}

// ListenRemote opens a listener on the *node*, carried back over this
// connection.
//
// It is how a node with no route out borrows the operator's: k3helper runs a
// proxy here, the node connects to a port on its own loopback, and the SSH
// channel carries the traffic. Nothing new is exposed to the network — the
// listener is on the node's loopback and the traffic rides a connection that
// already exists.
//
// Needs AllowTcpForwarding on the node's sshd, which is the default; a server
// with it disabled fails here rather than half way through an install.
func (c *Client) ListenRemote(addr string) (net.Listener, error) {
	if c.local {
		return nil, fmt.Errorf("a local node does not need a tunnel: it is this machine")
	}
	l, err := c.conn.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("open a listener on the node (is AllowTcpForwarding enabled in its sshd?): %w", err)
	}
	return l, nil
}

// RemoveFile deletes remotePath, ignoring "already gone".
func (c *Client) RemoveFile(remotePath string) error {
	if err := validRemotePath(remotePath); err != nil {
		return err
	}
	if c.local {
		return removeFileLocal(remotePath)
	}
	out, code, err := c.Run(fmt.Sprintf("rm -f '%s'", remotePath))
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("rm %s (exit %d): %s", remotePath, code, strings.TrimSpace(out))
	}
	return nil
}

// validRemotePath rejects paths that would break out of the single quotes
// used to build the remote shell command.
func validRemotePath(p string) error {
	if p == "" {
		return fmt.Errorf("remote path is empty")
	}
	if strings.ContainsAny(p, "'\n\x00") {
		return fmt.Errorf("invalid remote path %q: quotes and newlines are not allowed", p)
	}
	return nil
}

// Close closes the connection (no-op for a local client).
func (c *Client) Close() error {
	if c.local {
		return nil
	}
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// expandHome resolves a leading ~ in a key path; targets files are written by
// humans and "~/.ssh/id_ed25519" is what they type.
func expandHome(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", p, err)
	}
	if p == "~" {
		return home, nil
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:]), nil
	}
	return "", fmt.Errorf("cannot expand %q: ~user paths are not supported", p)
}
