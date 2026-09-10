package transport

import (
	"fmt"
	"net"
	"strconv"

	"github.com/solutionforest/k3helper/internal/kube"
	"github.com/solutionforest/k3helper/internal/ssh"
)

// sshForward is a port-forward opened in two parts: kubectl binds a port on
// the cluster node, and an SSH tunnel carries it to this machine. Both halves
// are needed — without the tunnel the port is open on the node and nowhere the
// operator can reach.
//
// This is the code that used to live in the TUI. It moved here so the TUI can
// ask for "a forward" without knowing which transport it holds.
type sshForward struct {
	pf     *kube.PortForward
	tunnel *ssh.Tunnel
	local  int
}

func (f *sshForward) LocalPort() int { return f.local }

func (f *sshForward) Close() error {
	if f.tunnel != nil {
		f.tunnel.Close()
		f.tunnel = nil
	}
	if f.pf != nil {
		f.pf.Stop()
		f.pf = nil
	}
	return nil
}

func startSSHForward(client *ssh.Client, ns, target string, remotePort, seq int) (Forward, error) {
	if client == nil {
		return nil, fmt.Errorf("no server connection")
	}
	// A node-side port distinct from the local one, so several forwards can
	// coexist and neither side collides with something already bound.
	nodePort := 39000 + seq
	pf, err := kube.StartPortForward(client, ns, target, remotePort, nodePort)
	if err != nil {
		return nil, err
	}
	// :0 lets the OS choose a free local port and report which.
	tunnel, err := client.Forward("127.0.0.1:0", pf.NodeAddr())
	if err != nil {
		pf.Stop()
		return nil, err
	}
	local := 0
	if _, portStr, e := net.SplitHostPort(tunnel.LocalAddr); e == nil {
		local, _ = strconv.Atoi(portStr)
	}
	return &sshForward{pf: pf, tunnel: tunnel, local: local}, nil
}
