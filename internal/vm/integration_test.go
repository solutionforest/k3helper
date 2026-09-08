//go:build integration

package vm

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/k3helper/internal/ssh"
)

// sandboxClient connects to a sandbox VM.
func sandboxClient(port int) (*ssh.Client, error) {
	return ssh.Dial(ssh.Node{
		Host: "127.0.0.1", Port: port, User: "sandbox",
		Key: "../../test/sandbox/ssh/id_ed25519",
	})
}

// TestSandboxClusterReady verifies the sandbox cluster (bootstrapped via
// `k3helper vm setup`) has all nodes Ready. Run after sandbox-up + vm setup.
func TestSandboxClusterReady(t *testing.T) {
	server, err := sandboxClient(2221)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer server.Close()

	deadline := time.Now().Add(60 * time.Second)
	for {
		out, code, err := server.SudoRun(`k3s kubectl get nodes --no-headers`)
		if err == nil && code == 0 && strings.Count(out, "Ready") >= 3 {
			return // success
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster not ready in time; kubectl output: %q (code %d, err %v)", out, code, err)
		}
		time.Sleep(3 * time.Second)
	}
}

// TestFullBootstrap reinstalls the whole cluster from scratch. It is slow
// (~3-5 min) and destructive: opt in with K3HELPER_FULL_BOOTSTRAP=1.
func TestFullBootstrap(t *testing.T) {
	if os.Getenv("K3HELPER_FULL_BOOTSTRAP") != "1" {
		t.Skip("set K3HELPER_FULL_BOOTSTRAP=1 to run the full cluster reinstall")
	}
	// uninstall k3s everywhere
	for _, port := range []int{2222, 2223, 2221} {
		c, err := sandboxClient(port)
		if err != nil {
			t.Fatalf("connect :%d: %v", port, err)
		}
		c.SudoRun(`/usr/local/bin/k3s-uninstall.sh 2>/dev/null; /usr/local/bin/k3s-agent-uninstall.sh 2>/dev/null; true`)
		c.Close()
	}

	server, err := sandboxClient(2221)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	defer server.Close()
	agent1, _ := sandboxClient(2222)
	defer agent1.Close()
	agent2, _ := sandboxClient(2223)
	defer agent2.Close()

	srvNode := ssh.Node{Host: "127.0.0.1", Port: 2221, User: "sandbox", Key: "../../test/sandbox/ssh/id_ed25519"}
	agents := []struct {
		Node   ssh.Node
		Client *ssh.Client
	}{
		{ssh.Node{Host: "127.0.0.1", Port: 2222, User: "sandbox", Key: "../../test/sandbox/ssh/id_ed25519"}, agent1},
		{ssh.Node{Host: "127.0.0.1", Port: 2223, User: "sandbox", Key: "../../test/sandbox/ssh/id_ed25519"}, agent2},
	}

	err = Setup(server, srvNode, agents, Options{
		ServerExtraArgs: "--snapshotter=native --disable=traefik",
		AgentExtraArgs:  "--snapshotter=native",
	})
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
}
