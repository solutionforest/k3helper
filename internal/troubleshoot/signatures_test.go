package troubleshoot

import (
	"strings"
	"testing"
)

func TestImagePullSignature(t *testing.T) {
	e := Evidence{
		PodEvents: map[string][]string{
			"prod/web-abc": {"Failed to pull image \"nginx:typo\": not found"},
		},
	}
	d := Diagnose(e)
	if len(d) == 0 || d[0].SignatureID != "pod.imagepull" {
		t.Fatalf("expected pod.imagepull top, got %+v", d)
	}
	if d[0].Confidence < 50 {
		t.Errorf("confidence = %d, want higher", d[0].Confidence)
	}
	if d[0].Remediation == "" {
		t.Error("remediation missing")
	}
}

func TestOOMSignature(t *testing.T) {
	e := Evidence{
		ContainerStates: map[string]string{"prod/api/api": "OOMKilled"},
	}
	d := Diagnose(e)
	if d[0].SignatureID != "pod.oom" {
		t.Fatalf("expected pod.oom top, got %+v", d)
	}
}

func TestPendingScheduling(t *testing.T) {
	e := Evidence{
		PodEvents: map[string][]string{
			"default/big": {"0/3 nodes are available: 3 Insufficient cpu"},
		},
	}
	d := Diagnose(e)
	if d[0].SignatureID != "pod.pending-sched" {
		t.Fatalf("expected pod.pending-sched, got %+v", d)
	}
}

func TestK3sDownWithNotReadyCorrelation(t *testing.T) {
	e := Evidence{
		NodeNotReady: []string{"agent1"},
		K3sService:   map[string]string{"agent1": "failed"},
	}
	d := Diagnose(e)
	if d[0].SignatureID != "node.notready-k3s-down" {
		t.Fatalf("expected node.notready-k3s-down, got %+v", d)
	}
	if d[0].Confidence < 80 {
		t.Errorf("correlated confidence = %d, want >=80", d[0].Confidence)
	}
	if !strings.Contains(d[0].Remediation, "systemctl restart") {
		t.Error("remediation should mention restart")
	}
}

func TestDiskPressureFromHostMetrics(t *testing.T) {
	e := Evidence{
		HostMetrics: map[string]HostMetric{"agent2": {DiskUsedPercent: 97, AvailMemMB: 4000}},
	}
	d := Diagnose(e)
	if d[0].SignatureID != "node.diskpressure" {
		t.Fatalf("expected node.diskpressure, got %+v", d)
	}
}

func TestKubeconfigError(t *testing.T) {
	e := Evidence{KubeconfigError: "connection refused"}
	d := Diagnose(e)
	if d[0].SignatureID != "cluster.kubeconfig" {
		t.Fatalf("expected cluster.kubeconfig, got %+v", d)
	}
}

// TestUnreachableNodeIsNeverHealthy: a node we could not contact must produce
// a finding. Reporting "healthy" for a cluster we only partly inspected is the
// worst possible failure mode for a troubleshooter.
func TestUnreachableNodeIsNeverHealthy(t *testing.T) {
	e := Evidence{
		// everything we *could* see looks fine
		PodStatuses:  map[string]string{},
		NodeNotReady: []string{},
		K3sService:   map[string]string{"server": "active"},
		Unreachable:  []UnreachableNode{{Name: "agent1", Reason: "dial tcp 10.0.0.5:22: connect: no route to host"}},
	}
	d := Diagnose(e)
	if len(d) == 0 {
		t.Fatal("unreachable node produced no diagnosis")
	}
	if d[0].SignatureID != "node.unreachable" {
		t.Errorf("expected node.unreachable first, got %+v", d)
	}
	if d[0].Confidence < 80 {
		t.Errorf("confidence = %d, want >=80", d[0].Confidence)
	}
}

func TestHealthyEvidenceYieldsNoDiagnoses(t *testing.T) {
	e := Evidence{
		PodEvents:    map[string][]string{},
		PodStatuses:  map[string]string{},
		NodeNotReady: []string{},
	}
	if d := Diagnose(e); len(d) != 0 {
		t.Errorf("healthy evidence should yield zero diagnoses, got %+v", d)
	}
}

func TestDiagnosesAreRanked(t *testing.T) {
	e := Evidence{
		PodStatuses:     map[string]string{"a": "CrashLoopBackOff", "b": "Evicted"},
		ContainerStates: map[string]string{"a/c": "OOMKilled"},
	}
	d := Diagnose(e)
	if len(d) < 2 {
		t.Fatalf("want >=2 diagnoses, got %d", len(d))
	}
	for i := 1; i < len(d); i++ {
		if d[i-1].Confidence < d[i].Confidence {
			t.Errorf("not ranked: %d before %d", d[i-1].Confidence, d[i].Confidence)
		}
	}
}
