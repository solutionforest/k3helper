//go:build integration

package troubleshoot_test

import (
	"strings"
	"testing"
	"time"

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

// TestGatherCollectsClusterEvidence asserts the cluster-layer probes actually
// return data. Without this, a probe that silently fails is indistinguishable
// from a healthy cluster: both leave the evidence nil and doctor reports
// "no issues detected".
func TestGatherCollectsClusterEvidence(t *testing.T) {
	server := sandbox.Dial(t, "server")
	g := troubleshoot.Gatherer{Server: server, Hosts: map[string]ssh.Executor{"server": server}}
	e := g.Collect()

	if e.KubeconfigError != "" {
		t.Fatalf("cluster unreachable: %s", e.KubeconfigError)
	}
	if e.CoreDNS == nil {
		t.Error("CoreDNS evidence not collected — the probe returned nothing")
	} else if e.CoreDNS.Desired == 0 {
		t.Errorf("CoreDNS desired replicas = 0, want the real deployment size (%+v)", *e.CoreDNS)
	}
	if e.CertSubject == "" {
		t.Error("certificate evidence not collected — `k3s certificate check` returned nothing parseable")
	}
	if e.CertExpiryDays <= 0 {
		t.Errorf("cert expiry = %d days; a freshly bootstrapped cluster should be far from expiry", e.CertExpiryDays)
	}
	t.Logf("coredns=%+v etcd=%v cert=%s in %dd emptyEndpoints=%v",
		e.CoreDNS, e.Etcd, e.CertSubject, e.CertExpiryDays, e.EmptyEndpoints)

	// The sandbox is a single sqlite-backed server, so there is no etcd; the
	// signature must stay silent rather than reporting 0 of 0 members ready.
	if e.Etcd != nil && e.Etcd.Desired == 0 {
		t.Error("Etcd should be nil on a sqlite-backed cluster, not a zero-valued ratio")
	}

	// Nothing above should read as a problem on a healthy sandbox.
	for _, d := range troubleshoot.Diagnose(e) {
		switch d.SignatureID {
		case "cluster.etcd-quorum", "cluster.cert-expiry", "network.coredns", "network.empty-endpoints":
			t.Errorf("healthy cluster produced %s: %s", d.SignatureID, d.Title)
		}
	}
}

// TestGatherDetectsCoreDNSOutage injects a real fault: scale CoreDNS to zero
// and confirm the signature fires, then restore it.
func TestGatherDetectsCoreDNSOutage(t *testing.T) {
	server := sandbox.Dial(t, "server")
	const kubectl = "sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"

	if _, code, err := server.Run(kubectl + " -n kube-system scale deployment coredns --replicas=0"); err != nil || code != 0 {
		t.Fatalf("scale down coredns: code=%d err=%v", code, err)
	}
	// Restore and wait for readiness: leaving DNS down would make every
	// later test in the package see a broken cluster.
	t.Cleanup(func() {
		server.Run(kubectl + " -n kube-system scale deployment coredns --replicas=1")
		// Scaling back up has to schedule the pod, pull if needed and pass a
		// readiness probe; 30s was tight enough to fail intermittently.
		for i := 0; i < 120; i++ {
			out, _, _ := server.Run(kubectl + " -n kube-system get deployment coredns -o jsonpath={.status.readyReplicas}")
			if strings.TrimSpace(out) == "1" {
				return
			}
			time.Sleep(time.Second)
		}
		t.Error("CoreDNS did not return to ready after the test; the sandbox is left degraded")
	})

	var diagnosed bool
	for i := 0; i < 15 && !diagnosed; i++ {
		time.Sleep(2 * time.Second)
		g := troubleshoot.Gatherer{Server: server, Hosts: map[string]ssh.Executor{"server": server}}
		if ds := troubleshoot.Diagnose(g.Collect()); hasSignature(ds, "network.coredns") {
			diagnosed = true
			t.Logf("detected after %ds", (i+1)*2)
		}
	}
	if !diagnosed {
		t.Error("CoreDNS scaled to zero but the network.coredns signature never fired")
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
	// Assert the finding is present, not that it ranks first: an unrelated
	// higher-confidence fault on the cluster must not fail this test.
	if !hasSignature(troubleshoot.Diagnose(g.Collect()), "node.unreachable") {
		t.Fatalf("expected a node.unreachable diagnosis, got %+v", troubleshoot.Diagnose(g.Collect()))
	}
}

func hasSignature(ds []troubleshoot.Diagnosis, id string) bool {
	for _, d := range ds {
		if d.SignatureID == id {
			return true
		}
	}
	return false
}
