package check

import (
	"os"
	"strings"
	"testing"
)

// unitExec answers list-unit-files for the units it was given, and is-active
// from a state map — mirroring how systemd reports the two separately.
type unitExec struct {
	present map[string]bool
	state   map[string]string
}

func (u unitExec) Run(cmd string) (string, int, error) {
	if strings.Contains(cmd, "list-unit-files") {
		for unit := range u.present {
			if strings.Contains(cmd, unit+".service") && u.present[unit] {
				return unit + ".service enabled\n", 0, nil
			}
		}
		return "", 1, nil
	}
	if strings.Contains(cmd, "is-active") {
		for unit, st := range u.state {
			if strings.Contains(cmd, " "+unit+" ") || strings.HasSuffix(strings.TrimSpace(cmd), unit+" 2>/dev/null") {
				return st + "\n", 0, nil
			}
		}
		// systemd prints "inactive" for a unit it does not know
		return "inactive\n", 3, nil
	}
	return "", 1, nil
}

func TestDetectDistro(t *testing.T) {
	cases := []struct {
		name    string
		present map[string]bool
		want    Distro
	}{
		{"k3s server", map[string]bool{"k3s": true}, DistroK3s},
		{"k3s agent", map[string]bool{"k3s-agent": true}, DistroK3s},
		{"kubeadm", map[string]bool{"kubelet": true}, DistroKubeadm},
		{"nothing installed", map[string]bool{}, DistroNone},
		// k3s wins when both are somehow present: its unit is authoritative.
		{"both", map[string]bool{"k3s": true, "kubelet": true}, DistroK3s},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectDistro(unitExec{present: tc.present}); got != tc.want {
				t.Errorf("DetectDistro = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDistroUnitsAndKubeconfig(t *testing.T) {
	if got := DistroK3s.Units("server"); len(got) != 1 || got[0] != "k3s" {
		t.Errorf("k3s server units = %v", got)
	}
	if got := DistroK3s.Units("agent"); len(got) != 1 || got[0] != "k3s-agent" {
		t.Errorf("k3s agent units = %v", got)
	}
	// kubelet runs on every kubeadm node regardless of role.
	for _, role := range []string{"server", "agent"} {
		got := DistroKubeadm.Units(role)
		if len(got) != 2 || got[0] != "kubelet" {
			t.Errorf("kubeadm %s units = %v, want kubelet first", role, got)
		}
	}
	if DistroK3s.Kubeconfig() != "/etc/rancher/k3s/k3s.yaml" {
		t.Errorf("k3s kubeconfig = %q", DistroK3s.Kubeconfig())
	}
	if DistroKubeadm.Kubeconfig() != "/etc/kubernetes/admin.conf" {
		t.Errorf("kubeadm kubeconfig = %q", DistroKubeadm.Kubeconfig())
	}
}

func TestServiceCheckK3sHealthy(t *testing.T) {
	res := ServiceCheck{Role: "server"}.Run(Context{Exec: unitExec{
		present: map[string]bool{"k3s": true},
		state:   map[string]string{"k3s": "active"},
	}})
	if res.Status != OK {
		t.Errorf("status = %s (%s)", res.Status, res.Summary)
	}
}

func TestServiceCheckKubeadmNode(t *testing.T) {
	// kubelet up, containerd down: the node looks alive to systemd but
	// cannot start a single container.
	res := ServiceCheck{Role: "agent"}.Run(Context{Exec: unitExec{
		present: map[string]bool{"kubelet": true, "containerd": true},
		state:   map[string]string{"kubelet": "active", "containerd": "failed"},
	}})
	if res.Status != Fail {
		t.Fatalf("status = %s, want FAIL (%s)", res.Status, res.Summary)
	}
	if !strings.Contains(res.Summary, "containerd") {
		t.Errorf("summary should name the dead unit: %s", res.Summary)
	}
	if !strings.Contains(res.Remediation, "journalctl") {
		t.Errorf("remediation should point at logs: %s", res.Remediation)
	}
}

// A node with no Kubernetes at all must say "install it", not "restart it".
func TestServiceCheckNothingInstalled(t *testing.T) {
	res := ServiceCheck{Role: "server"}.Run(Context{Exec: unitExec{present: map[string]bool{}}})
	if res.Status != Fail {
		t.Fatalf("status = %s, want FAIL", res.Status)
	}
	if !strings.Contains(res.Remediation, "vm setup") {
		t.Errorf("remediation should offer to install: %s", res.Remediation)
	}
	if strings.Contains(res.Remediation, "restart") {
		t.Errorf("must not suggest restarting a service that is not installed: %s", res.Remediation)
	}
}

// Regression: `systemctl is-active` prints "inactive" for an uninstalled
// unit, so a stopped k3s and a missing k3s used to be indistinguishable —
// and the missing case got the wrong advice.
func TestK3sServiceCheckDistinguishesStoppedFromMissing(t *testing.T) {
	stopped := K3sServiceCheck{Role: "server"}.Run(Context{Exec: unitExec{
		present: map[string]bool{"k3s": true},
		state:   map[string]string{"k3s": "inactive"},
	}})
	if !strings.Contains(stopped.Remediation, "journalctl") {
		t.Errorf("a stopped-but-installed k3s should point at logs: %s", stopped.Remediation)
	}

	missing := K3sServiceCheck{Role: "server"}.Run(Context{Exec: unitExec{present: map[string]bool{}}})
	if !strings.Contains(missing.Summary, "not found") {
		t.Errorf("an uninstalled k3s should say so: %s", missing.Summary)
	}
	if !strings.Contains(missing.Remediation, "get.k3s.io") {
		t.Errorf("an uninstalled k3s should offer to install: %s", missing.Remediation)
	}
}

// The host runner must use the distro-aware ServiceCheck. Wiring the k3s-only
// predecessor told the operator of a healthy kubeadm node to install a second
// Kubernetes distribution.
//
// HostChecks is now the one list both `check` and the TUI build from, so this
// asserts against the list itself rather than grepping each call site.
func TestHostChecksUseDistroAwareServiceCheck(t *testing.T) {
	var found bool
	for _, c := range HostChecks("server") {
		if _, isK3sOnly := c.(K3sServiceCheck); isK3sOnly {
			t.Error("HostChecks wires the k3s-only K3sServiceCheck; use ServiceCheck")
		}
		if _, ok := c.(ServiceCheck); ok {
			found = true
		}
	}
	if !found {
		t.Error("HostChecks does not run a service check at all")
	}
}

// Both callers must build their runner from HostChecks, or a check added to
// the list silently goes missing from one of them.
func TestCheckWiringUsesHostChecks(t *testing.T) {
	for _, path := range []string{"../cli/check_cmd.go", "../tui/tui.go"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(src), "check.HostChecks(") {
			t.Errorf("%s builds its own check list instead of using check.HostChecks", path)
		}
	}
}

// A kubeconfig cluster reports every host check as skipped, with the reason.
// Reporting nothing would read as a clean bill of health for a layer that was
// never looked at.
func TestSkippedHostResultsCoverEveryHostCheck(t *testing.T) {
	results := SkippedHostResults("server")
	if len(results) != len(HostChecks("server")) {
		t.Fatalf("got %d skipped results for %d host checks", len(results), len(HostChecks("server")))
	}
	for _, r := range results {
		if r.Status != Skip {
			t.Errorf("%s: status %v, want Skip — a managed cluster is not unhealthy for having no SSH", r.ID, r.Status)
		}
		if !strings.Contains(r.Summary, "kubeconfig") {
			t.Errorf("%s: summary %q does not say why it was skipped", r.ID, r.Summary)
		}
	}
}
