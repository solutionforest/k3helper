//go:build integration

package kube_test

import (
	"strings"
	"testing"

	"github.com/solutionforest/k3helper/internal/kube"
	"github.com/solutionforest/k3helper/internal/sandbox"
)

// The offline tests parse fixtures we wrote ourselves, which proves only that
// the parser matches our idea of the API's output. These run the same queries
// against a real API server, which is the only thing that can prove the field
// names and the multi-resource output shape are right.

func TestListWorkloadsLive(t *testing.T) {
	server := sandbox.Dial(t, "server")

	// k3s always runs coredns (Deployment) and svclb/local-path (DaemonSet).
	deployments, err := kube.ListWorkloads(server, "deployments", "kube-system")
	if err != nil {
		t.Fatalf("ListWorkloads(deployments): %v", err)
	}
	var coredns *kube.Workload
	for i := range deployments {
		if strings.HasPrefix(deployments[i].Name, "coredns") {
			coredns = &deployments[i]
		}
	}
	if coredns == nil {
		t.Fatalf("coredns not among %d deployments — the parser is probably wrong", len(deployments))
	}
	if !strings.Contains(coredns.Ready, "/") {
		t.Errorf("Ready = %q, want an n/m ratio", coredns.Ready)
	}
	if coredns.Selector == "" {
		t.Error("no selector parsed; the pod drill-down would list nothing")
	}
	if coredns.Images == "" {
		t.Error("no images parsed")
	}
	if coredns.Age <= 0 {
		t.Errorf("Age = %v", coredns.Age)
	}
	// The selector has to be one the API server accepts, or the drill-down
	// silently returns an empty pod list.
	pods, err := kube.ListPodsSelector(server, "kube-system", coredns.Selector)
	if err != nil {
		t.Fatalf("ListPodsSelector(%q): %v", coredns.Selector, err)
	}
	if len(pods) == 0 {
		t.Errorf("selector %q matched no pods", coredns.Selector)
	}

	daemonsets, err := kube.ListWorkloads(server, "daemonsets", "kube-system")
	if err != nil {
		t.Fatalf("ListWorkloads(daemonsets): %v", err)
	}
	for _, d := range daemonsets {
		// DaemonSet status fields have different names; a mis-parse shows as
		// "0/0" on a DaemonSet that is actually running.
		if d.Ready == "0/0" {
			t.Errorf("daemonset %s reports 0/0 — check the numberReady/desiredNumberScheduled fields", d.Name)
		}
	}
	t.Logf("%d deployments, %d daemonsets", len(deployments), len(daemonsets))
}

func TestListServicesLive(t *testing.T) {
	server := sandbox.Dial(t, "server")

	svcs, err := kube.ListServices(server, "")
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	var kubernetes *kube.Service
	for i := range svcs {
		if svcs[i].Namespace == "default" && svcs[i].Name == "kubernetes" {
			kubernetes = &svcs[i]
		}
	}
	if kubernetes == nil {
		t.Fatalf("the default/kubernetes service is missing from %d services", len(svcs))
	}
	if kubernetes.ClusterIP == "" || kubernetes.Ports == "" {
		t.Errorf("service parsed without an address or ports: %+v", kubernetes)
	}
}

func TestListIngressesLive(t *testing.T) {
	server := sandbox.Dial(t, "server")

	// A cluster with no ingresses is the normal case here; what matters is
	// that the query succeeds rather than erroring on an empty list.
	if _, err := kube.ListIngresses(server, ""); err != nil {
		t.Fatalf("ListIngresses: %v", err)
	}
}

// The ownership graph asks for five kinds in one query and relies on each item
// carrying its own `kind`. That is true of kubectl's mixed-resource output and
// not of a single-resource list, so it needs proving against the real thing.
func TestOwnerTreeLive(t *testing.T) {
	server := sandbox.Dial(t, "server")

	roots, err := kube.OwnerTree(server, "kube-system")
	if err != nil {
		t.Fatalf("OwnerTree: %v", err)
	}
	if len(roots) == 0 {
		t.Fatal("no roots in kube-system; every k3s cluster runs workloads there")
	}
	var sawDeployment, sawPod bool
	var walk func(n *kube.TreeNode, depth int)
	walk = func(n *kube.TreeNode, depth int) {
		if n.Kind == "" {
			t.Errorf("node %q has no kind — kubectl's mixed-resource output was parsed wrong", n.Name)
		}
		switch n.Kind {
		case "Deployment":
			sawDeployment = true
		case "Pod":
			sawPod = true
		}
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	if !sawDeployment || !sawPod {
		t.Errorf("graph has deployment=%v pod=%v; expected both in kube-system", sawDeployment, sawPod)
	}
	// coredns is a Deployment → ReplicaSet → Pod chain, which is the whole
	// point of the view.
	var chained bool
	for _, r := range roots {
		if r.Kind != "Deployment" {
			continue
		}
		for _, rs := range r.Children {
			if rs.Kind == "ReplicaSet" && len(rs.Children) > 0 {
				chained = true
			}
		}
	}
	if !chained {
		t.Error("no deployment → replicaset → pod chain was built; owner references are not being followed")
	}
}
