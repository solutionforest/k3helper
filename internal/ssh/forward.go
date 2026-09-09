package ssh

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
)

// Tunnel forwards a local TCP port to an address reachable from the remote
// host, the way `ssh -L` does.
//
// This is what makes port-forwarding useful from a laptop: `kubectl
// port-forward` binds on the node it runs on, so without a tunnel the port is
// only open on the server and not on the machine the operator is sitting at.
type Tunnel struct {
	// LocalAddr is the address actually listened on, including the port that
	// was chosen when the request asked for :0.
	LocalAddr string

	listener net.Listener
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closeErr error
	once     sync.Once
}

// Forward starts listening on localAddr and proxies every connection to
// remoteAddr, resolved from the far end of the SSH connection.
//
// A local node needs no tunnel — the port is already local — so this returns
// an error rather than pretending to build one.
func (c *Client) Forward(localAddr, remoteAddr string) (*Tunnel, error) {
	if c.local {
		return nil, fmt.Errorf("no tunnel needed for a local node: connect to %s directly", remoteAddr)
	}
	if c.conn == nil {
		return nil, fmt.Errorf("ssh connection is closed")
	}
	ln, err := net.Listen("tcp", localAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", localAddr, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t := &Tunnel{LocalAddr: ln.Addr().String(), listener: ln, cancel: cancel}

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		for {
			local, err := ln.Accept()
			if err != nil {
				return // listener closed, or shutting down
			}
			t.wg.Add(1)
			go func() {
				defer t.wg.Done()
				t.proxy(ctx, c, local, remoteAddr)
			}()
		}
	}()
	return t, nil
}

// proxy joins one accepted connection to a channel opened on the far side.
func (t *Tunnel) proxy(ctx context.Context, c *Client, local net.Conn, remoteAddr string) {
	defer local.Close()

	remote, err := c.conn.Dial("tcp", remoteAddr)
	if err != nil {
		return // the far end is not listening yet, or has gone away
	}
	defer remote.Close()

	// Copy both ways and stop as soon as either direction ends, so a hung
	// half-open connection cannot keep the pair alive.
	done := make(chan struct{}, 2)
	go func() { io.Copy(remote, local); done <- struct{}{} }()
	go func() { io.Copy(local, remote); done <- struct{}{} }()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Close stops accepting and waits for in-flight connections to finish.
func (t *Tunnel) Close() error {
	t.once.Do(func() {
		t.cancel()
		t.closeErr = t.listener.Close()
		t.wg.Wait()
	})
	return t.closeErr
}
