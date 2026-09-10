// Package transport opens the connections k3helper works through, and is the
// one place that decides how a cluster is reached.
//
// A cluster is described either by its nodes (SSH to a machine, run the
// kubectl that lives there) or by a kubeconfig (run kubectl here, talk to the
// API server). Both answer the same interface, so callers ask for a cluster
// executor and stop caring — except where they genuinely need a machine, which
// is what RequireHosts is for.
//
// Centralising this is not tidiness. Each command used to dial for itself, and
// that is how `local` and the host-key policy each got silently dropped from
// one call site; a second way to reach a cluster would multiply that.
package transport

import (
	"fmt"
	"os"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/kubectl"
	"github.com/solutionforest/k3helper/internal/ssh"
)

// Cluster is a machine kubectl runs on: it executes commands and can stage a
// file for kubectl to read. Satisfied by *ssh.Client and by kubectl.Local.
//
// The file methods are here because `deploy` and `verify --live` name a
// manifest on the operator's machine and kubectl reads it on the cluster's.
// Over SSH those differ; through a kubeconfig they are the same machine, and
// the write is a local one.
type Cluster interface {
	Run(cmd string) (string, int, error)
	WriteFile(path string, data []byte, mode os.FileMode) error
	RemoveFile(path string) error
	Close() error
}

// NodeFailure is a node named in the targets file that could not be contacted.
type NodeFailure struct {
	Name   string
	Reason string
}

// Server opens the connection cluster-layer commands run through.
//
// In SSH mode that is a connection to the first server node. In kubeconfig
// mode it is a local kubectl, and nothing is dialled at all.
func Server(t *config.Targets) (Cluster, error) {
	if t.Mode() == config.ModeKubeconfig {
		return Local(t)
	}
	srv, err := t.Server()
	if err != nil {
		return nil, err
	}
	c, err := ssh.Dial(srv.SSH())
	if err != nil {
		return nil, fmt.Errorf("connect to server: %w", err)
	}
	return c, nil
}

// Local builds the kubeconfig transport for a cluster, checking up front that
// kubectl exists. Finding that out mid-diagnosis produces a much worse message
// than finding it out at the front door.
func Local(t *config.Targets) (Cluster, error) {
	if err := kubectl.Available(); err != nil {
		return nil, err
	}
	return kubectl.Local{Kubeconfig: t.KubeconfigPath(), Context: t.KubeContext}, nil
}

// ServerName is the name to print for whatever Server connected to.
func ServerName(t *config.Targets) string {
	if t.Mode() == config.ModeKubeconfig {
		if t.KubeContext != "" {
			return t.KubeContext + " (kubeconfig)"
		}
		return "kubeconfig"
	}
	if srv, err := t.Server(); err == nil {
		return srv.Name
	}
	return t.Cluster
}

// Hosts opens a connection to every node for host-layer probes, reusing the
// one already open for the server node.
//
// A kubeconfig cluster has no hosts and returns an empty map with no failures:
// that is the absence of a layer, not a fault, and callers distinguish the two.
// A node that cannot be dialled is returned as a NodeFailure rather than
// skipped, because silently dropping it lets a diagnosis report a healthy
// cluster while a machine is down.
func Hosts(t *config.Targets, server Cluster, serverName string) (map[string]ssh.Executor, []NodeFailure, func()) {
	hosts := map[string]ssh.Executor{}
	var failures []NodeFailure
	var open []*ssh.Client
	closeAll := func() {
		for _, c := range open {
			c.Close()
		}
	}
	if t.Mode() == config.ModeKubeconfig {
		return hosts, nil, closeAll
	}
	for _, n := range t.Nodes {
		if n.Name == serverName {
			if exec, ok := server.(ssh.Executor); ok {
				hosts[n.Name] = exec
			}
			continue
		}
		c, err := ssh.Dial(n.SSH())
		if err != nil {
			failures = append(failures, NodeFailure{Name: n.Name, Reason: err.Error()})
			continue
		}
		open = append(open, c)
		hosts[n.Name] = c
	}
	return hosts, failures, closeAll
}

// Forward is a live port-forward, reachable on this machine.
type Forward interface {
	// LocalPort is the port bound here.
	LocalPort() int
	Close() error
}

// StartForward opens a port-forward to a pod or service and returns once it is
// listening locally.
//
// seq only matters for the SSH transport, which has to pick a distinct port on
// the cluster node for each forward so that several can coexist.
func StartForward(c Cluster, ns, target string, remotePort, seq int) (Forward, error) {
	switch t := c.(type) {
	case kubectl.Local:
		return t.StartForward(ns, target, remotePort)
	case *ssh.Client:
		return startSSHForward(t, ns, target, remotePort, seq)
	default:
		return nil, fmt.Errorf("port-forward is not supported by this connection")
	}
}

// RequireHosts refuses an operation that has to run on the machines
// themselves. `vm setup` installs a distribution and `registry` writes
// containerd configuration; neither has any meaning without a host, and doing
// half of one is worse than doing none.
func RequireHosts(t *config.Targets, what string) error {
	if t.Mode() != config.ModeKubeconfig {
		return nil
	}
	return fmt.Errorf("%s needs SSH access to the cluster's machines, and %q is reached through a kubeconfig\n\n"+
		"A kubeconfig reaches the API server, not the nodes behind it. To use this command, describe the "+
		"nodes in the targets file instead. On a managed cluster (EKS/GKE/AKS) this is the provider's job, "+
		"not k3helper's.", what, t.Cluster)
}
