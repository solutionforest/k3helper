//go:build integration

package kube_test

import (
	"strings"
	"testing"

	"github.com/solutionforest/k3helper/internal/kube"
	"github.com/solutionforest/k3helper/internal/sandbox"
)

// TestListPodsLive checks the pod browser against the real cluster: the
// kube-system pods k3s always runs must come back parsed, not empty.
func TestListPodsLive(t *testing.T) {
	server := sandbox.Dial(t, "server")

	pods, err := kube.ListPods(server, "")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(pods) == 0 {
		t.Fatal("no pods returned from a running cluster")
	}
	var sawCoreDNS bool
	for _, p := range pods {
		if p.Namespace == "kube-system" && strings.HasPrefix(p.Name, "coredns") {
			sawCoreDNS = true
			if p.Ready == "" || !strings.Contains(p.Ready, "/") {
				t.Errorf("Ready = %q, want an n/m ratio", p.Ready)
			}
			if p.Node == "" {
				t.Error("pod is scheduled but Node is empty")
			}
			if p.Age <= 0 {
				t.Errorf("Age = %v, want a positive duration", p.Age)
			}
		}
	}
	if !sawCoreDNS {
		t.Error("coredns pod not found; the pod list is probably being parsed wrong")
	}
	t.Logf("%d pods; first: %+v", len(pods), pods[0])

	// Namespace scoping must actually scope.
	sys, err := kube.ListPods(server, "kube-system")
	if err != nil {
		t.Fatalf("ListPods(kube-system): %v", err)
	}
	for _, p := range sys {
		if p.Namespace != "kube-system" {
			t.Errorf("namespace scoping leaked %s/%s", p.Namespace, p.Name)
		}
	}
}

func TestListNodesLive(t *testing.T) {
	server := sandbox.Dial(t, "server")

	nodes, err := kube.ListNodes(server)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	targets, err := sandbox.Targets()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != len(targets.Nodes) {
		t.Errorf("got %d nodes, want %d from the targets file", len(nodes), len(targets.Nodes))
	}
	for _, n := range nodes {
		if n.Status != "Ready" {
			t.Errorf("node %s = %s, want Ready", n.Name, n.Status)
		}
		if n.Version == "" {
			t.Errorf("node %s has no kubelet version", n.Name)
		}
	}
	t.Logf("nodes: %+v", nodes)
}

func TestListEventsLive(t *testing.T) {
	server := sandbox.Dial(t, "server")

	events, err := kube.ListEvents(server, "")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	// A freshly bootstrapped cluster always has scheduling/pull events.
	if len(events) == 0 {
		t.Skip("no events retained on this cluster")
	}
	for i := 1; i < len(events); i++ {
		if events[i-1].Age > events[i].Age {
			t.Fatalf("events not sorted newest-first at %d: %v then %v", i, events[i-1].Age, events[i].Age)
		}
	}
	t.Logf("%d events; newest: %+v", len(events), events[0])
}

// Logs and describe are the drill-downs the browser offers; both must work
// against a real pod.
func TestLogsAndDescribeLive(t *testing.T) {
	server := sandbox.Dial(t, "server")

	pods, err := kube.ListPods(server, "kube-system")
	if err != nil || len(pods) == 0 {
		t.Fatalf("no kube-system pods to read: %v", err)
	}
	target := pods[0]

	if _, err := kube.Logs(server, target.Namespace, target.Name, 20, false); err != nil {
		t.Errorf("Logs(%s/%s): %v", target.Namespace, target.Name, err)
	}

	body, err := kube.Describe(server, "pod", target.Namespace, target.Name)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !strings.Contains(body, target.Name) {
		t.Errorf("describe output does not mention %s", target.Name)
	}
}
