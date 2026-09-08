//go:build integration

package troubleshoot_test

import (
	"testing"

	"github.com/solutionforest/k3helper/internal/sandbox"
	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/solutionforest/k3helper/internal/troubleshoot"
)

// TestGatherSandbox runs the real gatherer against the live sandbox and
// asserts a healthy cluster produces no false K3sService "inactive".
func TestGatherSandbox(t *testing.T) {
	targets, err := sandbox.Targets()
	if err != nil {
		t.Skipf("sandbox targets unavailable: %v", err)
	}
	server := sandbox.Dial(t, "server")

	hosts := map[string]ssh.Executor{}
	for _, n := range targets.Nodes {
		hosts[n.Name] = sandbox.Dial(t, n.Name)
	}

	g := troubleshoot.Gatherer{Server: server, Hosts: hosts}
	e := g.Collect()
	t.Logf("K3sService=%v notReady=%v kubeErr=%q", e.K3sService, e.NodeNotReady, e.KubeconfigError)

	for _, n := range targets.Nodes {
		if e.K3sService[n.Name] != "active" {
			t.Errorf("node %s k3s service = %q, want active", n.Name, e.K3sService[n.Name])
		}
	}
	if len(e.NodeNotReady) > 0 {
		t.Errorf("unexpected NotReady nodes: %v", e.NodeNotReady)
	}
	if e.KubeconfigError != "" {
		t.Errorf("unexpected kubeconfig error: %s", e.KubeconfigError)
	}
}

// TestGatherReportsUnreachableNodes: a node absent from Hosts because it could
// not be dialled must surface as a diagnosis, never as silence.
func TestGatherReportsUnreachableNodes(t *testing.T) {
	server := sandbox.Dial(t, "server")

	g := troubleshoot.Gatherer{
		Server: server,
		Hosts:  map[string]ssh.Executor{"server": server},
		Unreachable: []troubleshoot.UnreachableNode{
			{Name: "ghost", Reason: "dial tcp: connect: no route to host"},
		},
	}
	d := troubleshoot.Diagnose(g.Collect())
	if len(d) == 0 || d[0].SignatureID != "node.unreachable" {
		t.Fatalf("expected node.unreachable diagnosis, got %+v", d)
	}
}
