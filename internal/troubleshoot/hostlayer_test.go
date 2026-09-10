package troubleshoot

import (
	"strings"
	"testing"

	"github.com/solutionforest/k3helper/internal/ssh"
)

// fakeExec answers a fixed script of commands, so a gather can be driven with
// no cluster and no SSH.
type fakeExec struct{ responses map[string]string }

func (f fakeExec) Run(cmd string) (string, int, error) {
	for prefix, out := range f.responses {
		if strings.Contains(cmd, prefix) {
			return out, 0, nil
		}
	}
	return "", 1, nil
}

// A kubeconfig cluster reports the missing host layer as a note on the scope
// of the diagnosis, never as a fault.
func TestHostLayerUnavailableIsInformational(t *testing.T) {
	ds := Diagnose(Evidence{HostLayerUnavailable: true})
	var found bool
	for _, d := range ds {
		if d.SignatureID == "cluster.host-layer-unavailable" {
			found = true
			if !d.Informational() {
				t.Error("the host-layer note is treated as a fault; it would fail every managed cluster's CI run")
			}
			if !strings.Contains(d.Remediation, "kubeconfig") {
				t.Errorf("remediation %q does not explain the limit", d.Remediation)
			}
		}
	}
	if !found {
		t.Fatal("no host-layer finding for a cluster with no host layer — silence reads as a clean bill of health")
	}
	if !OnlyInformational(ds) {
		t.Error("a healthy kubeconfig cluster produced a non-informational finding")
	}
}

func TestHostLayerSignatureSilentForSSHCluster(t *testing.T) {
	for _, d := range Diagnose(Evidence{}) {
		if d.SignatureID == "cluster.host-layer-unavailable" {
			t.Error("the host-layer note fired for a cluster that has one")
		}
	}
}

// The two ways of seeing less must stay distinct. "We tried and failed" is a
// fault; "there was nothing to try" is how managed clusters work.
func TestHostLayerIsNotAProbeError(t *testing.T) {
	e := Evidence{HostLayerUnavailable: true}
	if len(e.ProbeErrors) != 0 {
		t.Error("the missing host layer was recorded as a failed probe")
	}
	for _, d := range Diagnose(e) {
		if d.SignatureID == "cluster.partial-evidence" {
			t.Error("a kubeconfig cluster reported failed probes; it would send the operator to check RBAC")
		}
		if d.SignatureID == "node.unreachable" {
			t.Error("a kubeconfig cluster reported unreachable nodes; it has none")
		}
	}
}

// Real faults still have to be reported, and still have to set the exit code.
func TestClusterFaultsStillFireWithoutHostLayer(t *testing.T) {
	e := Evidence{
		HostLayerUnavailable: true,
		PodStatuses:          map[string]string{"default/web": "CrashLoopBackOff"},
	}
	ds := Diagnose(e)
	if OnlyInformational(ds) {
		t.Fatal("a crash-looping pod produced only scope notes")
	}
	var found bool
	for _, d := range ds {
		if d.SignatureID == "pod.crashloop" {
			found = true
		}
	}
	if !found {
		t.Error("pod.crashloop did not fire on a kubeconfig cluster")
	}
}

// Gatherer must carry the flag through: setting it on the gatherer and losing
// it in the evidence is how the note would go missing.
func TestGathererCarriesNoHostLayer(t *testing.T) {
	g := Gatherer{
		Server:      fakeExec{responses: map[string]string{}},
		Hosts:       map[string]ssh.Executor{},
		NoHostLayer: true,
	}
	e := g.Collect()
	if !e.HostLayerUnavailable {
		t.Error("Gatherer.NoHostLayer did not reach the evidence")
	}
}

// A crashlooping container is only reported as CrashLoopBackOff while it is
// waiting between attempts. Sampled the moment it restarts, the pod is Running
// with no reason attached — which used to read as nothing worse than an
// unready pod, and made the fault matrix catch the fault or miss it depending
// on when doctor happened to look.
func TestCrashLoopFoundFromRestartsWhenSampledMidRestart(t *testing.T) {
	e := Evidence{
		NotReadyPods: []string{"default/fault-crashloop"},
		PodRestarts:  map[string]int{"default/fault-crashloop": 5},
		// No PodStatuses entry: kubectl said Running, because it was.
	}
	var found bool
	for _, d := range Diagnose(e) {
		if d.SignatureID == "pod.crashloop" {
			found = true
		}
	}
	if !found {
		t.Error("a pod that is not ready after 5 restarts was not called a crash loop")
	}
}

// The waiting half of the cycle must still work, and the same pod seen both
// ways must not be counted twice — a doubled count inflates the confidence.
func TestCrashLoopCountsEachPodOnce(t *testing.T) {
	both := Evidence{
		PodStatuses:  map[string]string{"default/web": "CrashLoopBackOff"},
		NotReadyPods: []string{"default/web"},
		PodRestarts:  map[string]int{"default/web": 9},
	}
	waitingOnly := Evidence{
		PodStatuses: map[string]string{"default/web": "CrashLoopBackOff"},
	}
	if got, want := confidenceOf(Diagnose(both), "pod.crashloop"),
		confidenceOf(Diagnose(waitingOnly), "pod.crashloop"); got != want {
		t.Errorf("one pod seen two ways scored %d, want %d", got, want)
	}
}

// A pod that fell over once and came back is not a crash loop, and a pod that
// is simply slow to pass readiness is not one either. Reporting them would
// send people to `kubectl logs --previous` for a healthy rollout.
func TestCrashLoopIgnoresFewRestartsAndReadyPods(t *testing.T) {
	tests := []struct {
		name string
		e    Evidence
	}{
		{"one restart, still starting up", Evidence{
			NotReadyPods: []string{"default/web"},
			PodRestarts:  map[string]int{"default/web": 1},
		}},
		{"never restarted, failing readiness", Evidence{
			NotReadyPods: []string{"default/web"},
			PodRestarts:  map[string]int{"default/web": 0},
		}},
		{"restarted often but now ready", Evidence{
			PodRestarts: map[string]int{"default/web": 12},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if c := confidenceOf(Diagnose(tc.e), "pod.crashloop"); c != 0 {
				t.Errorf("pod.crashloop fired at %d%%", c)
			}
		})
	}
}
