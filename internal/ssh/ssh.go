package ssh

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
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
}

// Executor is the minimal command-execution surface (satisfied by *Client).
type Executor interface {
	Run(cmd string) (string, int, error)
}

// Client wraps an SSH connection to one node.
type Client struct {
	conn *gossh.Client
	node Node
}

// Dial connects to the node using private-key auth.
func Dial(n Node) (*Client, error) {
	if n.Port == 0 {
		n.Port = 22
	}
	key, err := os.ReadFile(n.Key)
	if err != nil {
		return nil, fmt.Errorf("read ssh key %s: %w", n.Key, err)
	}
	signer, err := gossh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key %s: %w", n.Key, err)
	}
	cfg := &gossh.ClientConfig{
		User: n.User,
		Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: func(hostname string, remote net.Addr, key gossh.PublicKey) error {
			return nil // sandbox/dev: no host key verification
		},
		Timeout: 10 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", n.Host, n.Port)
	conn, err := gossh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s@%s: %w", n.User, addr, err)
	}
	return &Client{conn: conn, node: n}, nil
}

// Run executes a command and returns stdout+stderr combined and the exit code.
func (c *Client) Run(cmd string) (string, int, error) {
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
	return c.Run("sudo -n " + cmd)
}

// Stream executes a command, streaming combined output to w. Returns exit code.
func (c *Client) Stream(cmd string, w io.Writer) (int, error) {
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

// RemoveFile deletes remotePath, ignoring "already gone".
func (c *Client) RemoveFile(remotePath string) error {
	if err := validRemotePath(remotePath); err != nil {
		return err
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

// Close closes the connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
