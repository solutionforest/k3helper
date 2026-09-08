package troubleshoot

import (
	"os"
	"testing"

	"github.com/solutionforest/k3helper/internal/ssh"
)

// TestGatherSandbox runs the real gatherer against the live sandbox when
// K3HELPER_SANDBOX=1 is set; asserts no false K3sService "inactive".
func TestGatherSandbox(t *testing.T) {
	if os.Getenv("K3HELPER_SANDBOX") != "1" {
		t.Skip("set K3HELPER_SANDBOX=1 with live sandbox")
	}
	server, err := ssh.Dial(ssh.Node{Host: "192.168.139.177", Port: 22, User: "sandbox", Key: "../../test/sandbox/ssh/id_ed25519"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	hosts := map[string]ssh.Executor{"server": server}
	g := Gatherer{Server: server, Hosts: hosts}
	e := g.Collect()
	t.Logf("K3sService=%v notReady=%v kubeErr=%q", e.K3sService, e.NodeNotReady, e.KubeconfigError)
	if e.K3sService["server"] != "active" {
		t.Errorf("server k3s service = %q, want active", e.K3sService["server"])
	}
	if len(e.NodeNotReady) > 0 {
		t.Errorf("unexpected NotReady nodes: %v", e.NodeNotReady)
	}
}
