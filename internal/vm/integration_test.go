//go:build integration

package vm_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/k3helper/internal/sandbox"
	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/solutionforest/k3helper/internal/vm"
)

// TestSandboxClusterReady verifies the sandbox cluster (bootstrapped via
// `k3helper vm setup`) has every node in the targets file Ready.
func TestSandboxClusterReady(t *testing.T) {
	server := sandbox.Dial(t, "server")

	targets, err := sandbox.Targets()
	if err != nil {
		t.Fatalf("load targets: %v", err)
	}
	wantNodes := len(targets.Nodes)

	deadline := time.Now().Add(60 * time.Second)
	var out string
	var code int
	for {
		out, code, err = server.SudoRun(`k3s kubectl get nodes --no-headers`)
		if err == nil && code == 0 && countReady(out) >= wantNodes {
			return // success
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d nodes Ready in time; kubectl output: %q (code %d, err %v)",
				countReady(out), wantNodes, out, code, err)
		}
		time.Sleep(3 * time.Second)
	}
}

// countReady counts nodes whose STATUS column is exactly "Ready"; a substring
// match would also count "NotReady".
func countReady(kubectlOutput string) int {
	n := 0
	for _, line := range strings.Split(kubectlOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "Ready" {
			n++
		}
	}
	return n
}

// TestFullBootstrap reinstalls the whole cluster from scratch. It is slow
// (~3-5 min) and destructive: opt in with K3HELPER_FULL_BOOTSTRAP=1.
func TestFullBootstrap(t *testing.T) {
	if os.Getenv("K3HELPER_FULL_BOOTSTRAP") != "1" {
		t.Skip("set K3HELPER_FULL_BOOTSTRAP=1 to run the full cluster reinstall")
	}
	serverNode, err := sandbox.Node("server")
	if err != nil {
		t.Skipf("sandbox targets unavailable: %v", err)
	}
	agentNodes, err := sandbox.Agents()
	if err != nil {
		t.Fatalf("load agents: %v", err)
	}

	// uninstall k3s everywhere (agents first, then the server)
	for _, n := range append(append([]ssh.Node{}, agentNodes...), serverNode) {
		c, err := ssh.Dial(n)
		if err != nil {
			t.Fatalf("connect %s: %v", n.Host, err)
		}
		c.SudoRun(`/usr/local/bin/k3s-uninstall.sh 2>/dev/null; /usr/local/bin/k3s-agent-uninstall.sh 2>/dev/null; true`)
		c.Close()
	}

	server := sandbox.Dial(t, "server")
	servers := []vm.Target{{Node: serverNode, Client: server}}
	agents := make([]vm.Target, 0, len(agentNodes))
	for _, n := range agentNodes {
		c, err := ssh.Dial(n)
		if err != nil {
			t.Fatalf("connect agent %s: %v", n.Host, err)
		}
		defer c.Close()
		agents = append(agents, vm.Target{Node: n, Client: c})
	}

	err = vm.Setup(servers, agents, vm.Options{
		ServerExtraArgs: "--snapshotter=native --disable=traefik",
		AgentExtraArgs:  "--snapshotter=native",
	})
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
}
