package transport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/kubectl"
)

func kubeconfigCluster(t *testing.T) *config.Targets {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return &config.Targets{Cluster: "prod", Kubeconfig: path, KubeContext: "admin"}
}

func sshCluster() *config.Targets {
	return &config.Targets{Cluster: "lab", Nodes: []config.Node{
		{Name: "server", Role: "server", Host: "10.0.0.1", Port: 22, User: "root"},
		{Name: "agent1", Role: "agent", Host: "10.0.0.2", Port: 22, User: "root"},
	}}
}

// A kubeconfig cluster has no hosts, and that is the absence of a layer rather
// than a fault. Reporting the nodes as unreachable would make every managed
// cluster look degraded.
func TestHostsIsEmptyForKubeconfigCluster(t *testing.T) {
	tg := kubeconfigCluster(t)
	server, err := Local(tg)
	if err != nil {
		t.Skipf("kubectl not available: %v", err)
	}
	hosts, failures, closeAll := Hosts(tg, server, ServerName(tg))
	defer closeAll()
	if len(hosts) != 0 {
		t.Errorf("got %d hosts for a kubeconfig cluster, want none", len(hosts))
	}
	if len(failures) != 0 {
		t.Errorf("got %d node failures, want none — there are no nodes to fail", len(failures))
	}
}

func TestRequireHostsRefusesKubeconfigCluster(t *testing.T) {
	tg := kubeconfigCluster(t)
	err := RequireHosts(tg, "vm setup")
	if err == nil {
		t.Fatal("vm setup was allowed against a cluster with no machines")
	}
	for _, want := range []string{"vm setup", "prod", "kubeconfig"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRequireHostsAllowsSSHCluster(t *testing.T) {
	if err := RequireHosts(sshCluster(), "vm setup"); err != nil {
		t.Errorf("vm setup refused for an SSH cluster: %v", err)
	}
}

func TestServerNameNamesTheContext(t *testing.T) {
	if got := ServerName(kubeconfigCluster(t)); !strings.Contains(got, "admin") {
		t.Errorf("ServerName() = %q, want the kube context in it", got)
	}
	if got := ServerName(sshCluster()); got != "server" {
		t.Errorf("ServerName() = %q, want the server node's name", got)
	}
}

// Server must not dial anything for a kubeconfig cluster, and must hand back a
// transport pointed at the file the targets named.
func TestServerForKubeconfigClusterIsLocalKubectl(t *testing.T) {
	tg := kubeconfigCluster(t)
	c, err := Server(tg)
	if err != nil {
		t.Skipf("kubectl not available: %v", err)
	}
	defer c.Close()
	l, ok := c.(kubectl.Local)
	if !ok {
		t.Fatalf("Server returned %T, want kubectl.Local", c)
	}
	if l.Kubeconfig != tg.KubeconfigPath() {
		t.Errorf("kubeconfig = %q, want %q", l.Kubeconfig, tg.KubeconfigPath())
	}
	if l.Context != "admin" {
		t.Errorf("context = %q, want admin", l.Context)
	}
}

// kubectl.Local must satisfy everything deploy and verify --live need, or the
// mismatch only shows up the first time somebody deploys to a managed cluster.
func TestLocalSatisfiesCluster(t *testing.T) {
	var _ Cluster = kubectl.Local{}
}
