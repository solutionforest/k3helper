//go:build integration

package kubectl_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/solutionforest/k3helper/internal/kube"
	"github.com/solutionforest/k3helper/internal/kubectl"
	"github.com/solutionforest/k3helper/internal/sandbox"
	"github.com/solutionforest/k3helper/internal/troubleshoot"
)

// This is the proof the transport works: the same sandbox cluster, read twice —
// once over SSH the way k3helper always has, and once through a kubeconfig with
// a local kubectl — asserting the two agree.
//
// Unit tests can only show that a command string parses into the arguments we
// expected. They cannot show that a real kubectl, given those arguments,
// returns what the parsers upstream are expecting. That is what this does.
//
// It needs a kubeconfig for the sandbox on this machine. `make bootstrap`
// writes one to sandbox-kubeconfig.yaml; the test skips rather than fails
// without it, so the suite still runs on a machine that only has SSH to the
// cluster.
func localForSandbox(t *testing.T) kubectl.Local {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not installed on this machine")
	}
	path := os.Getenv("K3HELPER_TEST_KUBECONFIG")
	if path == "" {
		root, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		// internal/kubectl → repo root
		path = filepath.Join(root, "..", "..", "sandbox-kubeconfig.yaml")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no sandbox kubeconfig at %s (set K3HELPER_TEST_KUBECONFIG)", path)
	}
	return kubectl.Local{Kubeconfig: path}
}

func names(pods []kube.Pod) []string {
	out := make([]string, 0, len(pods))
	for _, p := range pods {
		out = append(out, p.Namespace+"/"+p.Name)
	}
	sort.Strings(out)
	return out
}

// The pod list must be the same list whichever door it came through.
func TestPodsMatchBetweenTransports(t *testing.T) {
	viaSSH, err := kube.ListPods(sandbox.Dial(t, "server"), "kube-system")
	if err != nil {
		t.Fatalf("ListPods over SSH: %v", err)
	}
	viaKubeconfig, err := kube.ListPods(localForSandbox(t), "kube-system")
	if err != nil {
		t.Fatalf("ListPods through kubeconfig: %v", err)
	}
	a, b := names(viaSSH), names(viaKubeconfig)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("transports disagree about the cluster\n over SSH: %v\n via kubeconfig: %v", a, b)
	}
}

func TestNodesMatchBetweenTransports(t *testing.T) {
	viaSSH, err := kube.ListNodes(sandbox.Dial(t, "server"))
	if err != nil {
		t.Fatalf("ListNodes over SSH: %v", err)
	}
	viaKubeconfig, err := kube.ListNodes(localForSandbox(t))
	if err != nil {
		t.Fatalf("ListNodes through kubeconfig: %v", err)
	}
	if len(viaSSH) != len(viaKubeconfig) {
		t.Fatalf("node count differs: %d over SSH, %d via kubeconfig", len(viaSSH), len(viaKubeconfig))
	}
	for i := range viaSSH {
		if viaSSH[i].Name != viaKubeconfig[i].Name {
			t.Errorf("node %d: %q over SSH, %q via kubeconfig", i, viaSSH[i].Name, viaKubeconfig[i].Name)
		}
		if viaSSH[i].Version != viaKubeconfig[i].Version {
			t.Errorf("node %s: version %q over SSH, %q via kubeconfig",
				viaSSH[i].Name, viaSSH[i].Version, viaKubeconfig[i].Version)
		}
	}
}

// A gather through the kubeconfig must produce usable cluster evidence — not
// an empty bundle that would make a broken cluster look healthy.
func TestGatherThroughKubeconfigSeesTheCluster(t *testing.T) {
	e := troubleshoot.Gatherer{Server: localForSandbox(t), NoHostLayer: true}.Collect()

	if e.KubeconfigError != "" {
		t.Fatalf("cluster layer unreadable through the kubeconfig: %s", e.KubeconfigError)
	}
	if e.CoreDNS == nil {
		t.Error("CoreDNS readiness was not gathered; the jsonpath query did not survive the transport")
	}
	if !e.HostLayerUnavailable {
		t.Error("the missing host layer was not recorded")
	}
	// The host layer must be absent, not merely empty-looking: anything here
	// means a host probe ran somewhere it could not have.
	if len(e.HostMetrics) != 0 || len(e.K3sService) != 0 || len(e.ClockSkew) != 0 {
		t.Error("host evidence appeared for a transport with no hosts")
	}
	if _, recorded := e.ProbeErrors["certificates"]; recorded {
		t.Error("the k3s-only certificate probe was recorded as a failure rather than skipped")
	}
	t.Logf("probe errors: %v", e.ProbeErrors)
}

// port-forward through the kubeconfig binds here rather than on a node, so
// there is no tunnel involved and the port must be reachable directly.
func TestPortForwardThroughKubeconfig(t *testing.T) {
	l := localForSandbox(t)
	fwd, err := l.StartForward("kube-system", "svc/kube-dns", 53)
	if err != nil {
		t.Fatalf("StartForward: %v", err)
	}
	defer fwd.Close()
	if fwd.LocalPort() <= 0 {
		t.Fatalf("LocalPort() = %d, want the port kubectl bound", fwd.LocalPort())
	}
	t.Logf("kube-dns forwarded to 127.0.0.1:%d", fwd.LocalPort())
}
