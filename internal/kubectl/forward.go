package kubectl

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Forward is a `kubectl port-forward` running as a child of this process.
//
// The SSH transport has to do this in two parts — kubectl binds on the cluster
// node, and a tunnel carries the port back here. Through a kubeconfig there is
// only one machine involved, so kubectl binds locally and there is nothing to
// tunnel.
type Forward struct {
	cmd   *exec.Cmd
	local int
	once  sync.Once
}

// LocalPort is the port bound on this machine.
func (f *Forward) LocalPort() int { return f.local }

// Close stops the forward. Safe to call more than once: the TUI closes each
// forward on teardown and again when the operator removes it from the list.
func (f *Forward) Close() error {
	f.once.Do(func() {
		if f.cmd != nil && f.cmd.Process != nil {
			f.cmd.Process.Kill()
			// Reap it, or the child stays a zombie for the life of the TUI.
			f.cmd.Wait()
		}
	})
	return nil
}

// StartForward runs `kubectl port-forward` and returns once it is listening.
//
// The local port is chosen by kubectl (":remote" asks for any free one) and
// read back from its output rather than picked here. Picking a port means
// binding it to find out it is free, closing it, and handing the number to a
// process that races anything else on the machine for it; asking kubectl what
// it bound has no such gap.
func (l Local) StartForward(ns, target string, remotePort int) (*Forward, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	args := []string{"--kubeconfig", l.Kubeconfig}
	if l.Context != "" {
		args = append(args, "--context", l.Context)
	}
	args = append(args, "port-forward", "-n", ns, target,
		fmt.Sprintf(":%d", remotePort), "--address", "127.0.0.1")

	cmd := exec.Command(Binary, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("port-forward: %w", err)
	}
	// kubectl reports the bound address on stdout and its failures on stderr.
	// Both are needed: one to learn the port, the other to explain a refusal
	// instead of reporting a bare timeout.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("port-forward: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start port-forward: %w", err)
	}
	f := &Forward{cmd: cmd}

	type result struct {
		port int
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if p, ok := parseForwardingPort(sc.Text()); ok {
				ch <- result{port: p}
				// Keep draining: kubectl blocks on a full pipe, which would
				// wedge the forward as soon as it logged a second line.
				go io.Copy(io.Discard, stdout)
				return
			}
		}
		ch <- result{err: fmt.Errorf("port-forward exited without binding a port")}
	}()
	go io.Copy(io.Discard, stderr)

	select {
	case r := <-ch:
		if r.err != nil {
			f.Close()
			return nil, r.err
		}
		f.local = r.port
		return f, nil
	case <-time.After(10 * time.Second):
		f.Close()
		return nil, fmt.Errorf("port-forward to %s/%s did not start listening within 10s", ns, target)
	}
}

// parseForwardingPort reads the local port out of kubectl's
// "Forwarding from 127.0.0.1:54321 -> 80".
func parseForwardingPort(line string) (int, bool) {
	const marker = "Forwarding from "
	i := strings.Index(line, marker)
	if i < 0 {
		return 0, false
	}
	rest := line[i+len(marker):]
	// Take the address up to the arrow, then the port after the last colon —
	// last, not first, because an IPv6 address has several.
	if j := strings.Index(rest, " ->"); j >= 0 {
		rest = rest[:j]
	}
	k := strings.LastIndexByte(rest, ':')
	if k < 0 {
		return 0, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(rest[k+1:]))
	if err != nil || port <= 0 {
		return 0, false
	}
	return port, true
}
