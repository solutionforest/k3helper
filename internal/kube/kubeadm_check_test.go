//go:build integration

package kube_test

import (
	"testing"

	"github.com/solutionforest/k3helper/internal/kube"
	"github.com/solutionforest/k3helper/internal/sandbox"
)

// The resource browser used a hardcoded k3s kubectl until recently; this
// proves it reads a kubeadm cluster too.
func TestBrowserReadsKubeadmCluster(t *testing.T) {
	server := sandbox.Dial(t, "server")
	if base := kube.DetectBase(server); base == "" {
		t.Fatal("no kubectl base detected")
	} else {
		t.Logf("detected base: %s", base)
	}
	nodes, err := kube.ListNodes(server)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("no nodes returned")
	}
	for _, n := range nodes {
		if n.Status != "Ready" {
			t.Errorf("node %s = %s", n.Name, n.Status)
		}
	}
	pods, err := kube.ListPods(server, "kube-system")
	if err != nil || len(pods) == 0 {
		t.Fatalf("ListPods(kube-system) = %d pods, %v", len(pods), err)
	}
	t.Logf("%d nodes, %d kube-system pods", len(nodes), len(pods))
}
