package vm

import (
	"strings"
	"testing"
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
