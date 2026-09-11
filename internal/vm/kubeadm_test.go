package vm

import (
	"strings"
	"testing"

	"github.com/solutionforest/k3helper/internal/ssh"
)

// kubeadm defaults the API server's advertise address to the default route's
// interface, which on a cloud VM is the public one — and the join command
// handed to every agent is built from it. That is the same fault the k3s path
// had against real air-gapped VMs, where agents dialled a public address their
// network could not reach.
func TestKubeadmAdvertisesTheConfiguredAddress(t *testing.T) {
	var rec []string
	servers := fakeTargets(&rec, "10.104.0.13")
	if err := SetupKubeadm(servers, nil, KubeadmOptions{}); err != nil {
		t.Fatalf("SetupKubeadm: %v", err)
	}
	var init string
	for _, c := range rec {
		if strings.Contains(c, "kubeadm init") {
			init = c
		}
	}
	if init == "" {
		t.Fatal("no kubeadm init was issued")
	}
	if !strings.Contains(init, "--apiserver-advertise-address='10.104.0.13'") {
		t.Errorf("kubeadm init did not pin the advertise address: %s", init)
	}
}

func TestKubeadmAdvertiseOverride(t *testing.T) {
	var rec []string
	servers := fakeTargets(&rec, "203.0.113.10")
	if err := SetupKubeadm(servers, nil, KubeadmOptions{JoinAddress: "10.104.0.13"}); err != nil {
		t.Fatalf("SetupKubeadm: %v", err)
	}
	all := strings.Join(rec, "\n")
	if !strings.Contains(all, "--apiserver-advertise-address='10.104.0.13'") {
		t.Errorf("--join-address was ignored:\n%s", all)
	}
}

// A hostname cannot be an advertise address: the flag takes an IP. Rather than
// pass something kubeadm will reject, the flag is left off and kubeadm's own
// default applies.
func TestKubeadmSkipsAdvertiseForHostname(t *testing.T) {
	var rec []string
	servers := fakeTargets(&rec, "server.internal.example")
	if err := SetupKubeadm(servers, nil, KubeadmOptions{}); err != nil {
		t.Fatalf("SetupKubeadm: %v", err)
	}
	all := strings.Join(rec, "\n")
	if strings.Contains(all, "--apiserver-advertise-address") {
		t.Errorf("a hostname was passed where kubeadm wants an IP:\n%s", all)
	}
}

// Every step that reaches the network needs the proxy, and it has to sit after
// sudo — sudo resets the environment, so a prefix on the outside reaches the
// shell and not the command doing the fetching.
//
// The CNI step is a URL kubectl fetches itself. Live, it was the one step that
// had been missed: the node installed Kubernetes fine through the tunnel and
// then failed with "dial tcp 20.205.243.166:443: i/o timeout".
func TestKubeadmProxyReachesEveryNetworkStep(t *testing.T) {
	var rec []string
	servers := []Target{{Node: ssh.Node{Host: "10.104.0.5"},
		Client: &tunnelHost{fakeHost: fakeHost{name: "s1", rec: &rec}}}}
	agents := []Target{{Node: ssh.Node{Host: "10.104.0.6"},
		Client: &tunnelHost{fakeHost: fakeHost{name: "a1", rec: &rec}}}}
	if err := SetupKubeadm(servers, agents, KubeadmOptions{ViaProxy: true}); err != nil {
		t.Fatalf("SetupKubeadm: %v", err)
	}
	steps := map[string]string{
		"prerequisites": "base64 -d",
		"kubeadm init":  "kubeadm init",
		"CNI":           "apply -f",
		"join":          "kubeadm join",
	}
	for name, needle := range steps {
		var cmd string
		for _, c := range rec {
			if strings.Contains(c, needle) {
				cmd = c
			}
		}
		if cmd == "" {
			t.Errorf("no %s step was run", name)
			continue
		}
		if !strings.Contains(cmd, "http_proxy=http://127.0.0.1:") {
			t.Errorf("the %s step runs without the proxy: %s", name, cmd)
		}
		// After sudo, not before: sudo resets the environment.
		if i, j := strings.Index(cmd, "sudo"), strings.Index(cmd, "http_proxy="); i >= 0 && j >= 0 && j < i {
			t.Errorf("the %s step puts the proxy before sudo, which resets it: %s", name, cmd)
		}
	}
}

// Waiting for cloud-init is not enough on its own: apt-daily and
// unattended-upgrades run on timers and can take the lock minutes after boot.
// Live, that failed the second node with "Could not get lock
// /var/lib/apt/lists/lock. It is held by process 10754 (apt-get)".
func TestKubeadmPrereqWaitsForTheAptLock(t *testing.T) {
	script := kubeadmPrereqScript("v1.31", AptMirror{})
	if !strings.Contains(script, "DPkg::Lock::Timeout") {
		t.Errorf("apt is run without waiting for the lock:\n%s", script)
	}
	// Every apt invocation, not just the first: the lock can be taken between
	// them.
	for _, line := range strings.Split(script, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "apt-get ") {
			t.Errorf("this apt-get does not wait for the lock: %s", l)
		}
	}
}
