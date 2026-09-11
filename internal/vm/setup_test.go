package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/solutionforest/k3helper/internal/bundle"

	"github.com/solutionforest/k3helper/internal/ssh"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/k3helper/internal/check"
)

func TestServerToken(t *testing.T) {
	c := fakeClient{check.MapExec{
		`sudo -n cat /var/lib/rancher/k3s/server/node-token`: {Out: "  K10abc123::server:node\n", Code: 0},
	}}
	token, err := serverToken(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "K10abc123::server:node" {
		t.Errorf("token = %q, want trimmed value", token)
	}

	fail := fakeClient{check.MapExec{
		`sudo -n cat /var/lib/rancher/k3s/server/node-token`: {Out: "cat: no such file\n", Code: 1},
	}}
	if _, err := serverToken(fail); err == nil {
		t.Error("expected error when token missing")
	}
}

func TestServerInternalIP(t *testing.T) {
	c := fakeClient{check.MapExec{
		`hostname -I | awk '{print $1}'`: {Out: "172.18.0.2 \n", Code: 0},
	}}
	ip, err := serverInternalIP(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "172.18.0.2" {
		t.Errorf("ip = %q", ip)
	}
}

func TestAllReady(t *testing.T) {
	good := []string{"server=True", "agent1=True"}
	if !allReady(good) {
		t.Error("expected allReady=true")
	}
	bad := []string{"server=True", "agent1=False"}
	if allReady(bad) {
		t.Error("expected allReady=false when one node NotReady")
	}
	if allReady([]string{}) {
		t.Error("empty should not be ready")
	}
}

func TestInstallURLDefault(t *testing.T) {
	var o Options
	if o.installURL() != "https://get.k3s.io" {
		t.Errorf("default URL = %q", o.installURL())
	}
	o.InstallURL = "http://stub/install.sh"
	if o.installURL() != "http://stub/install.sh" {
		t.Errorf("override URL = %q", o.installURL())
	}
	if (Options{Channel: ""}).channel() != "stable" {
		t.Error("default channel should be stable")
	}
}

func TestWithSpace(t *testing.T) {
	if withSpace("") != "" {
		t.Error("empty should stay empty")
	}
	if withSpace(" --disable traefik") != " --disable traefik" {
		t.Errorf("arg passthrough = %q", withSpace(" --disable traefik"))
	}
}

// fakeClient adapts a check.Executor to the minimal ssh surface vm uses.
// Only commands invoked through serverToken/serverInternalIP (via SudoRun/Run) hit it.
type fakeClient struct{ check.MapExec }

func (f fakeClient) SudoRun(cmd string) (string, int, error) {
	// vm calls SudoRun("cat ...") — ssh.Client prefixes "sudo -n "; emulate both forms
	if out, ok := f.MapExec["sudo -n "+cmd]; ok {
		return out.Out, out.Code, nil
	}
	return f.MapExec.Run(cmd)
}

func (f fakeClient) Run(cmd string) (string, int, error) { return f.MapExec.Run(cmd) }

var _ = strings.TrimSpace // keep import if unused in future edits

// --- kubeconfig rewrite ---

const k3sKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: LS0t
    server: https://127.0.0.1:6443
  name: default
`

// k3s writes 127.0.0.1 because it expects to be used on the node. Copied to
// another machine unchanged it points kubectl at the caller's own loopback.
func TestRewriteKubeconfigServer(t *testing.T) {
	out, err := rewriteKubeconfigServer(k3sKubeconfig, "10.0.0.10")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !strings.Contains(out, "server: https://10.0.0.10:6443") {
		t.Errorf("address not rewritten:\n%s", out)
	}
	if strings.Contains(out, "127.0.0.1") {
		t.Errorf("loopback still present:\n%s", out)
	}
	// everything else must survive untouched
	if !strings.Contains(out, "certificate-authority-data: LS0t") {
		t.Error("rewrite damaged the rest of the file")
	}
}

func TestRewriteKubeconfigVariants(t *testing.T) {
	cases := []struct{ name, in, host, want string }{
		{"localhost", "    server: https://localhost:6443\n", "10.0.0.5", "https://10.0.0.5:6443"},
		{"ipv6 loopback", "    server: https://[::1]:6443\n", "10.0.0.5", "https://10.0.0.5:6443"},
		{"non-default port preserved", "    server: https://127.0.0.1:7443\n", "10.0.0.5", "https://10.0.0.5:7443"},
		{"ipv6 target is bracketed", "    server: https://127.0.0.1:6443\n", "fd00::1", "https://[fd00::1]:6443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := rewriteKubeconfigServer(tc.in, tc.host)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("got %q, want it to contain %q", out, tc.want)
			}
		})
	}
}

// A genuinely local node needs no rewrite.
func TestRewriteKubeconfigSkipsLoopbackTargets(t *testing.T) {
	for _, host := range []string{"", "127.0.0.1", "localhost", "::1"} {
		out, err := rewriteKubeconfigServer(k3sKubeconfig, host)
		if err != nil {
			t.Fatalf("host %q: %v", host, err)
		}
		if out != k3sKubeconfig {
			t.Errorf("host %q should leave the kubeconfig unchanged", host)
		}
	}
}

// Handing back a kubeconfig that cannot connect is worse than failing.
func TestRewriteKubeconfigReportsUnrecognisedShape(t *testing.T) {
	_, err := rewriteKubeconfigServer("apiVersion: v1\nclusters: []\n", "10.0.0.10")
	if err == nil {
		t.Fatal("expected an error when there is no server address to rewrite")
	}
	if !strings.Contains(err.Error(), "10.0.0.10") {
		t.Errorf("error should say what the address ought to be: %v", err)
	}
}

// A kubeconfig already naming the right host is fine as-is.
func TestRewriteKubeconfigAlreadyCorrect(t *testing.T) {
	in := "    server: https://10.0.0.10:6443\n"
	out, err := rewriteKubeconfigServer(in, "10.0.0.10")
	if err != nil {
		t.Fatalf("already-correct kubeconfig should not error: %v", err)
	}
	if out != in {
		t.Errorf("out = %q, want unchanged", out)
	}
}

// --- waiting for the expected node count ---

// Counting only registered nodes let a lone server satisfy the wait while
// agents were still joining, so "cluster ready" could mean a third of one.
func TestWaitReadyRequiresExpectedNodeCount(t *testing.T) {
	const q = `sudo -n k3s kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'`
	only1 := fakeClient{check.MapExec{q: {Out: "server=True\n", Code: 0}}}

	err := waitReady(only1, 3, 100*time.Millisecond)
	if err == nil {
		t.Fatal("1 of 3 nodes Ready should not satisfy the wait")
	}
	if !strings.Contains(err.Error(), "1 of 3") {
		t.Errorf("error should say how many registered: %v", err)
	}

	all3 := fakeClient{check.MapExec{q: {Out: "server=True\nagent1=True\nagent2=True\n", Code: 0}}}
	if err := waitReady(all3, 3, time.Second); err != nil {
		t.Errorf("3 of 3 Ready should satisfy the wait: %v", err)
	}
}

// Registered-but-NotReady and never-registered have different causes, so the
// timeout message must distinguish them.
func TestReadySummaryDistinguishesFailureModes(t *testing.T) {
	notRegistered := readySummary([]string{"server=True"}, 3)
	if !strings.Contains(notRegistered, "registered") || !strings.Contains(notRegistered, "6443") {
		t.Errorf("missing-node summary unhelpful: %s", notRegistered)
	}
	notReady := readySummary([]string{"server=True", "agent1=False", "agent2=True"}, 3)
	if !strings.Contains(notReady, "agent1") {
		t.Errorf("should name the NotReady node: %s", notReady)
	}
	if strings.Contains(notReady, "agent2") {
		t.Errorf("should not name Ready nodes: %s", notReady)
	}
}

// --- HA control plane ---

// fakeHost records every command Setup issues, and answers the few probes it
// makes, so the install arguments can be asserted without a VM.
type fakeHost struct {
	name string
	rec  *[]string
}

func (f fakeHost) Run(cmd string) (string, int, error) {
	*f.rec = append(*f.rec, f.name+": "+cmd)
	switch {
	case strings.Contains(cmd, "node-token"):
		return "K10secret::server:node\n", 0, nil
	case strings.Contains(cmd, "hostname -I"):
		return "10.0.0.1\n", 0, nil
	case strings.Contains(cmd, "get nodes"):
		// enough Ready nodes to satisfy any arrangement these tests build
		return "a=True\nb=True\nc=True\nd=True\n", 0, nil
	case strings.Contains(cmd, "print-join-command"):
		return "kubeadm join 10.0.0.1:6443 --token abcdef.0123456789abcdef " +
			"--discovery-token-ca-cert-hash sha256:deadbeef\n", 0, nil
	}
	return "", 0, nil
}
func (f fakeHost) SudoRun(cmd string) (string, int, error) { return f.Run("sudo -n " + cmd) }
func (f fakeHost) Stream(cmd string, w io.Writer) (int, error) {
	*f.rec = append(*f.rec, f.name+": "+cmd)
	return 0, nil
}
func (f fakeHost) SudoPrefix() string { return "sudo " }

func fakeTargets(rec *[]string, names ...string) []Target {
	var out []Target
	for _, n := range names {
		out = append(out, Target{Node: ssh.Node{Host: n}, Client: fakeHost{name: n, rec: rec}})
	}
	return out
}

// etcd needs an odd member count to hold quorum. Four servers tolerate the
// same single failure as three and lose quorum at two, so building one is
// almost never what the operator meant.
func TestSetupRejectsEvenServerCount(t *testing.T) {
	var rec []string
	for _, n := range [][]string{{"s1", "s2"}, {"s1", "s2", "s3", "s4"}} {
		err := Setup(fakeTargets(&rec, n...), nil, Options{})
		if err == nil {
			t.Fatalf("%d servers was accepted", len(n))
		}
		if !strings.Contains(err.Error(), "quorum") {
			t.Errorf("error should explain quorum: %v", err)
		}
	}
	if err := Setup(nil, nil, Options{}); err == nil {
		t.Error("zero servers should be rejected")
	}
}

// A single server keeps k3s's default sqlite datastore; --cluster-init would
// switch it to embedded etcd for no reason.
func TestSingleServerKeepsSqlite(t *testing.T) {
	var rec []string
	if err := Setup(fakeTargets(&rec, "s1"), nil, Options{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	all := strings.Join(rec, "\n")
	if strings.Contains(all, "--cluster-init") {
		t.Errorf("a single server should not use --cluster-init:\n%s", all)
	}
	if !strings.Contains(all, "sh -s - server") {
		t.Errorf("no server install issued:\n%s", all)
	}
}

// Three servers: the first initialises embedded etcd, the other two join it.
func TestHAServersInitialiseThenJoin(t *testing.T) {
	var rec []string
	servers := fakeTargets(&rec, "s1", "s2", "s3")
	agents := fakeTargets(&rec, "a1")
	if err := Setup(servers, agents, Options{ServerExtraArgs: "--disable=traefik"}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var s1, s2, s3, a1 string
	for _, line := range rec {
		if !strings.Contains(line, "sh -s -") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "s1: "):
			s1 = line
		case strings.HasPrefix(line, "s2: "):
			s2 = line
		case strings.HasPrefix(line, "s3: "):
			s3 = line
		case strings.HasPrefix(line, "a1: "):
			a1 = line
		}
	}

	if !strings.Contains(s1, "--cluster-init") {
		t.Errorf("the first server must initialise the cluster: %s", s1)
	}
	if !strings.Contains(s1, "--disable=traefik") {
		t.Errorf("extra args were dropped from the first server: %s", s1)
	}
	for name, line := range map[string]string{"s2": s2, "s3": s3} {
		if strings.Contains(line, "--cluster-init") {
			t.Errorf("%s must join, not re-initialise: %s", name, line)
		}
		// The join address is the first server's host from the targets file,
		// not an address discovered on the node.
		if !strings.Contains(line, "--server https://s1:6443") {
			t.Errorf("%s did not join the first server: %s", name, line)
		}
		if !strings.Contains(line, "K3S_TOKEN='K10secret::server:node'") {
			t.Errorf("%s joined without the quoted token: %s", name, line)
		}
	}
	if !strings.Contains(a1, "K3S_URL=https://s1:6443") || strings.Contains(a1, "--server ") {
		t.Errorf("the agent should join with K3S_URL, not --server: %s", a1)
	}
}

// Servers are added one at a time: etcd learners join sequentially, and adding
// several at once can cost quorum.
func TestHAServersJoinSequentially(t *testing.T) {
	var rec []string
	if err := Setup(fakeTargets(&rec, "s1", "s2", "s3"), nil, Options{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	joinIdx, settleAfterJoin := -1, false
	for i, line := range rec {
		if strings.HasPrefix(line, "s2: ") && strings.Contains(line, "sh -s -") {
			joinIdx = i
		}
		if joinIdx >= 0 && i > joinIdx && strings.Contains(line, "get nodes") {
			settleAfterJoin = true
			break
		}
	}
	if !settleAfterJoin {
		t.Error("the cluster was not allowed to settle before the next server joined")
	}
}

// --- kubeadm bootstrap ---

func TestKubeadmDefaultsMatchTheCNI(t *testing.T) {
	// Flannel and the pod CIDR passed to `kubeadm init` must agree, or pods
	// get addresses the CNI will not route.
	flannel := KubeadmOptions{}
	if flannel.cni() != "flannel" || flannel.podCIDR() != "10.244.0.0/16" {
		t.Errorf("flannel defaults = %s / %s", flannel.cni(), flannel.podCIDR())
	}
	calico := KubeadmOptions{CNI: "Calico"}
	if calico.cni() != "calico" || calico.podCIDR() != "192.168.0.0/16" {
		t.Errorf("calico defaults = %s / %s", calico.cni(), calico.podCIDR())
	}
	// An explicit CIDR always wins.
	explicit := KubeadmOptions{CNI: "calico", PodCIDR: "10.99.0.0/16"}
	if explicit.podCIDR() != "10.99.0.0/16" {
		t.Errorf("explicit CIDR was overridden: %s", explicit.podCIDR())
	}
}

// kubeadm HA needs --upload-certs and an endpoint in front of the API
// servers. Saying so beats building half of it.
func TestKubeadmRefusesMultipleServers(t *testing.T) {
	var rec []string
	err := SetupKubeadm(fakeTargets(&rec, "s1", "s2"), nil, KubeadmOptions{})
	if err == nil {
		t.Fatal("multiple control-plane nodes were accepted")
	}
	for _, want := range []string{"upload-certs", "k3s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should explain the limitation and the alternative: %v", err)
		}
	}
	if err := SetupKubeadm(nil, nil, KubeadmOptions{}); err == nil {
		t.Error("zero servers should be rejected")
	}
}

// The prerequisite script must do the things kubeadm requires and does not do
// itself; each omission fails much later and confusingly.
func TestKubeadmPrereqScriptCoversTheRequirements(t *testing.T) {
	script := kubeadmPrereqScript("v1.31", AptMirror{})
	for _, want := range []string{
		"swapoff -a",   // kubelet refuses to start with swap on
		"br_netfilter", // pod traffic must be visible to iptables
		"bridge-nf-call-iptables",
		"ip_forward",
		"nf_conntrack_max",     // kube-proxy dies if it has to raise this and cannot
		"containerd",           // there must be a CRI to run containers
		"SystemdCgroup = true", // must match the kubelet's cgroup driver
		"conntrack",            // a hard kubeadm preflight requirement
		"socat",                // kubectl port-forward uses it on the node
		"kubelet kubeadm kubectl",
		"apt-mark hold", // pin, so an unattended upgrade cannot skew versions
		"pkgs.k8s.io/core:/stable:/v1.31",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("prerequisite script is missing %q", want)
		}
	}
	// The version must be threaded through, not hardcoded.
	if strings.Contains(kubeadmPrereqScript("v1.30", AptMirror{}), "stable:/v1.31") {
		t.Error("the requested version was ignored")
	}
}

func TestLastNonEmptyLine(t *testing.T) {
	out := "W0101 some warning\nkubeadm join 10.0.0.1:6443 --token abc --discovery-token-ca-cert-hash sha256:x\n\n"
	got := lastNonEmptyLine(out)
	if !strings.HasPrefix(got, "kubeadm join") {
		t.Errorf("lastNonEmptyLine = %q, want the join command", got)
	}
	if lastNonEmptyLine("   \n\n") != "" {
		t.Error("blank input should yield an empty string")
	}
}

// waitReady speaks k3s's dialect; on kubeadm the same query has to go through
// kubectl with the admin kubeconfig.
func TestKubeadmRunnerTranslatesTheKubectlDialect(t *testing.T) {
	var got string
	h := recordingHost{fn: func(cmd string) { got = cmd }}
	kubeadmRunner{h}.SudoRun(`k3s kubectl get nodes -o jsonpath='{...}'`)
	if strings.Contains(got, "k3s kubectl") {
		t.Errorf("k3s dialect leaked to a kubeadm node: %s", got)
	}
	if !strings.Contains(got, "kubectl --kubeconfig /etc/kubernetes/admin.conf") {
		t.Errorf("not translated: %s", got)
	}
}

type recordingHost struct{ fn func(string) }

func (r recordingHost) Run(cmd string) (string, int, error) { r.fn(cmd); return "", 0, nil }
func (r recordingHost) SudoRun(cmd string) (string, int, error) {
	return r.Run("sudo -n " + cmd)
}
func (r recordingHost) Stream(cmd string, w io.Writer) (int, error) { r.fn(cmd); return 0, nil }
func (r recordingHost) SudoPrefix() string                          { return "sudo " }

// A non-zero exit with no Go error is the normal remote failure; %w on a nil
// error prints "%!w(<nil>)" and tells the reader nothing.
func TestExitReasonReadsWell(t *testing.T) {
	if got := exitReason(1, nil); strings.Contains(got, "%!w") || !strings.Contains(got, "exit 1") {
		t.Errorf("exitReason(1, nil) = %q", got)
	}
	if got := exitReason(0, io.ErrUnexpectedEOF); got != io.ErrUnexpectedEOF.Error() {
		t.Errorf("exitReason should prefer a real error: %q", got)
	}
}

// The prerequisite script must run as root: it writes under /etc and loads
// kernel modules, and running it unprivileged fails line by line.
func TestKubeadmPrereqRunsAsRoot(t *testing.T) {
	var rec []string
	SetupKubeadm(fakeTargets(&rec, "s1"), nil, KubeadmOptions{})
	var prep string
	for _, line := range rec {
		if strings.Contains(line, "base64 -d") {
			prep = line
		}
	}
	if prep == "" {
		t.Fatalf("no prerequisite command was issued: %v", rec)
	}
	if !strings.Contains(prep, "sudo ") {
		t.Errorf("prerequisites are not run as root: %s", prep)
	}
	if !strings.Contains(prep, "bash -s") {
		t.Errorf("script is not fed to a shell: %s", prep)
	}
}

// A pinned version must skip the channel entirely. Live testing found
// update.k3s.io serving a Traefik default certificate from every one of its
// addresses, which broke `curl -sfL https://get.k3s.io | sh -` worldwide —
// pinning is the way past an outage in a service we do not control.
func TestVersionPinSkipsTheChannel(t *testing.T) {
	var rec []string
	if err := Setup(fakeTargets(&rec, "s1"), nil, Options{Version: "v1.31.2+k3s1"}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	all := strings.Join(rec, "\n")
	if !strings.Contains(all, "INSTALL_K3S_VERSION='v1.31.2+k3s1'") {
		t.Errorf("version not pinned:\n%s", all)
	}
	if strings.Contains(all, "INSTALL_K3S_CHANNEL") {
		t.Errorf("a pinned version still resolved a channel:\n%s", all)
	}
}

// Without a pin the channel is still used, so existing behaviour is unchanged.
func TestChannelUsedWhenNoVersionPinned(t *testing.T) {
	var rec []string
	if err := Setup(fakeTargets(&rec, "s1"), nil, Options{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	all := strings.Join(rec, "\n")
	if !strings.Contains(all, "INSTALL_K3S_CHANNEL=stable") {
		t.Errorf("channel missing:\n%s", all)
	}
}

// The agents and joining servers must honour the pin too, or a cluster ends up
// running two different k3s builds.
func TestVersionPinReachesAgents(t *testing.T) {
	var rec []string
	servers := fakeTargets(&rec, "s1")
	agents := fakeTargets(&rec, "a1")
	if err := Setup(servers, agents, Options{Version: "v1.31.2+k3s1"}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	for _, line := range rec {
		if strings.Contains(line, "sh -s - agent") && !strings.Contains(line, "INSTALL_K3S_VERSION=") {
			t.Errorf("agent install did not carry the pinned version: %s", line)
		}
	}
}

// A command that ran and exited non-zero has no error to wrap. Passing nil to
// %w printed "%!w(<nil>)" as the last thing an operator saw when an install
// failed on a real VM.
func TestInstallFailureMessageHasNoFormatVerbLeak(t *testing.T) {
	got := installFailure("server install", 1, nil).Error()
	if strings.Contains(got, "%!") {
		t.Errorf("format verb leaked into the message: %s", got)
	}
	if !strings.Contains(got, "exit 1") {
		t.Errorf("exit code missing from: %s", got)
	}
	wrapped := installFailure("server install", 0, errors.New("connection reset"))
	if !strings.Contains(wrapped.Error(), "connection reset") {
		t.Errorf("underlying error lost: %s", wrapped)
	}
}

// The address the other nodes join on comes from the targets file, because
// that is the address the operator chose and the one k3helper has just proved
// works by connecting over it.
//
// Live testing is what turned this up: `hostname -I` returns a cloud VM's
// public address first, and on air-gapped nodes with egress blocked that is
// the one address the cluster cannot use. The agents retried "failed to get CA
// certs" against it indefinitely while the server ran fine next to them.
func TestJoinAddressComesFromTheTargetsFile(t *testing.T) {
	var rec []string
	servers := fakeTargets(&rec, "10.104.0.13")
	agents := fakeTargets(&rec, "10.104.0.12")
	if err := Setup(servers, agents, Options{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	all := strings.Join(rec, "\n")
	if !strings.Contains(all, "K3S_URL=https://10.104.0.13:6443") {
		t.Errorf("the agent did not join on the server's configured address:\n%s", all)
	}
	// 10.0.0.1 is what the fake `hostname -I` reports.
	if strings.Contains(all, "https://10.0.0.1:6443") {
		t.Errorf("the join address was discovered on the node instead:\n%s", all)
	}
}

// --join-address covers the split case: k3helper reaches the nodes over one
// network and the cluster talks over another.
func TestJoinAddressOverride(t *testing.T) {
	var rec []string
	servers := fakeTargets(&rec, "203.0.113.10")
	agents := fakeTargets(&rec, "203.0.113.11")
	if err := Setup(servers, agents, Options{JoinAddress: "10.104.0.13"}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	all := strings.Join(rec, "\n")
	if !strings.Contains(all, "K3S_URL=https://10.104.0.13:6443") {
		t.Errorf("the override was ignored:\n%s", all)
	}
	if strings.Contains(all, "K3S_URL=https://203.0.113.10:6443") {
		t.Errorf("the public address was used despite the override:\n%s", all)
	}
}

// A local node has no host address to take, so discovery is still the fallback.
func TestJoinAddressFallsBackToDiscoveryForLocalNode(t *testing.T) {
	var rec []string
	servers := []Target{{Node: ssh.Node{Host: "localhost", Local: true}, Client: fakeHost{name: "s1", rec: &rec}}}
	agents := fakeTargets(&rec, "10.104.0.12")
	if err := Setup(servers, agents, Options{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	all := strings.Join(rec, "\n")
	if !strings.Contains(all, "K3S_URL=https://10.0.0.1:6443") {
		t.Errorf("a local server did not fall back to a discovered address:\n%s", all)
	}
}

// A fresh cloud VM answers sshd before cloud-init has finished with it, and in
// that window apt is still replacing ca-certificates — so every https download
// on the box fails TLS verification. Installing into that window produced
// "curl failed to verify the legitimacy of the server" on real DigitalOcean
// droplets, which reads like a firewall problem and is not one.
func TestSetupWaitsForCloudInitBeforeInstalling(t *testing.T) {
	var rec []string
	servers := fakeTargets(&rec, "s1")
	agents := fakeTargets(&rec, "a1")
	if err := Setup(servers, agents, Options{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	firstInstall, cloudInit := -1, -1
	for i, c := range rec {
		if cloudInit < 0 && strings.Contains(c, "cloud-init status --wait") {
			cloudInit = i
		}
		if firstInstall < 0 && strings.Contains(c, "get.k3s.io") {
			firstInstall = i
		}
	}
	if cloudInit < 0 {
		t.Fatal("cloud-init was never waited on")
	}
	if firstInstall < 0 {
		t.Fatal("nothing was installed")
	}
	if cloudInit > firstInstall {
		t.Errorf("waited for cloud-init at step %d, after installing at step %d", cloudInit, firstInstall)
	}
}

// kubeadm's prerequisites run apt, which collides with cloud-init's dpkg lock
// on a machine that has only just booted.
func TestKubeadmWaitsForCloudInitBeforeApt(t *testing.T) {
	var rec []string
	if err := SetupKubeadm(fakeTargets(&rec, "s1"), nil, KubeadmOptions{}); err != nil {
		t.Fatalf("SetupKubeadm: %v", err)
	}
	cloudInit, prereq := -1, -1
	for i, c := range rec {
		if cloudInit < 0 && strings.Contains(c, "cloud-init status --wait") {
			cloudInit = i
		}
		if prereq < 0 && strings.Contains(c, "base64 -d") {
			prereq = i
		}
	}
	if cloudInit < 0 {
		t.Fatal("cloud-init was never waited on")
	}
	if prereq >= 0 && cloudInit > prereq {
		t.Errorf("ran the prerequisites at step %d before waiting for cloud-init at step %d", prereq, cloudInit)
	}
}

// A machine without cloud-init must not be held up by the check.
func TestCloudInitWaitIsGuarded(t *testing.T) {
	var rec []string
	if err := Setup(fakeTargets(&rec, "s1"), nil, Options{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	var wait string
	for _, c := range rec {
		if strings.Contains(c, "cloud-init status --wait") {
			wait = c
		}
	}
	if !strings.Contains(wait, "command -v cloud-init") {
		t.Errorf("the wait is not guarded by a presence check: %s", wait)
	}
	if !strings.Contains(wait, "timeout 300") {
		t.Errorf("a stuck cloud-init would hang the install forever: %s", wait)
	}
}

// uploadHost is a fakeHost that can also receive files, so the offline path
// can be driven without a VM.
type uploadHost struct {
	fakeHost
	uploaded map[string]int64
	uname    string
	sums     map[string]string
}

func (u *uploadHost) Run(cmd string) (string, int, error) {
	if strings.Contains(cmd, "uname -m") {
		return u.uname + "\n", 0, nil
	}
	if strings.Contains(cmd, "sha256sum") {
		// Match the exact staged path, not a substring: "k3s" is also a
		// substring of "k3s-airgap-images.tar.zst", so a looser match hands
		// back the binary's hash for the image archive.
		for name, sum := range u.sums {
			if strings.Contains(cmd, "'"+remoteStage+"/"+name+"'") {
				return sum + "\n", 0, nil
			}
		}
		return "", 0, nil
	}
	return u.fakeHost.Run(cmd)
}

func (u *uploadHost) WriteFileFrom(path string, r io.Reader, mode os.FileMode, progress func(int64)) error {
	n, err := io.Copy(io.Discard, r)
	if u.uploaded == nil {
		u.uploaded = map[string]int64{}
	}
	u.uploaded[path] = n
	return err
}

// writeBundle creates a bundle on disk for the offline path to install from.
func writeBundle(t *testing.T, arch string) (string, *bundle.Manifest) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		bundle.BinaryName:  "#!/bin/false\n",
		bundle.ImagesName:  "not really a tarball",
		bundle.InstallName: "#!/bin/sh\n",
	}
	m := &bundle.Manifest{Version: "v1.31.2+k3s1", Arch: arch}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		switch name {
		case bundle.BinaryName:
			m.BinarySHA = hex.EncodeToString(sum[:])
		case bundle.ImagesName:
			m.ImagesSHA = hex.EncodeToString(sum[:])
		}
	}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, bundle.ManifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, m
}

// A bundle for the wrong architecture is refused before anything is uploaded.
// Finding out afterwards means a few hundred megabytes sent to a machine that
// was never going to run it.
func TestOfflineRefusesWrongArchBeforeUploading(t *testing.T) {
	var rec []string
	dir, _ := writeBundle(t, "arm64")
	host := &uploadHost{fakeHost: fakeHost{name: "s1", rec: &rec}, uname: "x86_64"}
	err := Setup([]Target{{Node: ssh.Node{Host: "s1"}, Client: host}}, nil, Options{BundleDir: dir})
	if err == nil {
		t.Fatal("an arm64 bundle was accepted on an amd64 node")
	}
	if !strings.Contains(err.Error(), "arm64") || !strings.Contains(err.Error(), "amd64") {
		t.Errorf("the error does not name both architectures: %v", err)
	}
	if len(host.uploaded) != 0 {
		t.Errorf("uploaded %d file(s) before checking the architecture", len(host.uploaded))
	}
}

// An upload that did not survive the link is caught by its hash, rather than
// becoming a cluster that installs cleanly and then will not run pods.
func TestOfflineDetectsACorruptedUpload(t *testing.T) {
	var rec []string
	dir, _ := writeBundle(t, "amd64")
	host := &uploadHost{
		fakeHost: fakeHost{name: "s1", rec: &rec},
		uname:    "x86_64",
		sums:     map[string]string{bundle.BinaryName: strings.Repeat("00", 32)},
	}
	err := Setup([]Target{{Node: ssh.Node{Host: "s1"}, Client: host}}, nil, Options{BundleDir: dir})
	if err == nil {
		t.Fatal("a corrupted upload was installed")
	}
	if !strings.Contains(err.Error(), "corrupted") {
		t.Errorf("unhelpful error for a bad hash: %v", err)
	}
}

// The happy path uploads all three files and installs with the download
// skipped, which is the whole point: the node never reaches the internet.
func TestOfflineInstallsWithoutDownloading(t *testing.T) {
	var rec []string
	dir, m := writeBundle(t, "amd64")
	host := &uploadHost{
		fakeHost: fakeHost{name: "s1", rec: &rec},
		uname:    "x86_64",
		sums:     map[string]string{bundle.BinaryName: m.BinarySHA, bundle.ImagesName: m.ImagesSHA},
	}
	if err := Setup([]Target{{Node: ssh.Node{Host: "s1"}, Client: host}}, nil,
		Options{BundleDir: dir}); err != nil {
		t.Fatalf("offline setup: %v", err)
	}
	if len(host.uploaded) != 3 {
		t.Errorf("uploaded %d files, want 3: %v", len(host.uploaded), host.uploaded)
	}
	all := strings.Join(rec, "\n")
	if !strings.Contains(all, "INSTALL_K3S_SKIP_DOWNLOAD=true") {
		t.Errorf("the installer was not told to skip the download:\n%s", all)
	}
	if strings.Contains(all, "get.k3s.io") {
		t.Errorf("an offline install still reached for the internet:\n%s", all)
	}
	// The staged copy is a few hundred megabytes on a real node.
	if !strings.Contains(all, "rm -rf '"+remoteStage+"'") {
		t.Errorf("the staging directory was left behind:\n%s", all)
	}
}

// A tunnelHost is a fakeHost that can also open a listener, so the --via-proxy
// path can be driven without a VM.
type tunnelHost struct {
	fakeHost
	l net.Listener
}

func (h *tunnelHost) ListenRemote(addr string) (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	h.l = l
	return l, err
}

// --via-proxy must point apt and containerd at the tunnel, and must take both
// settings away again. A node left pointing at a proxy that no longer exists
// cannot reach its own package manager, and "apt hangs on 127.0.0.1" points
// nowhere near k3helper.
func TestViaProxyConfiguresAndCleansUp(t *testing.T) {
	var rec []string
	host := &tunnelHost{fakeHost: fakeHost{name: "s1", rec: &rec}}
	err := Setup([]Target{{Node: ssh.Node{Host: "s1"}, Client: host}}, nil, Options{ViaProxy: true})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	all := strings.Join(rec, "\n")
	for _, want := range []string{
		`Acquire::http::Proxy`,
		`Acquire::https::Proxy`,
		aptProxyConf,
		containerdProxyConf,
		`HTTP_PROXY=http://127.0.0.1:`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the proxy was not configured (%q missing):\n%s", want, all)
		}
	}
	if !strings.Contains(all, "rm -f "+aptProxyConf) {
		t.Errorf("the apt proxy config was left on the node:\n%s", all)
	}
	// The install itself must carry the environment: the k3s installer is
	// fetched with curl, which reads http_proxy and nothing else.
	var install string
	for _, c := range rec {
		if strings.Contains(c, "get.k3s.io") {
			install = c
		}
	}
	// Once for curl, which fetches the installer, and once after sudo for the
	// installer itself, which fetches the k3s binary — sudo resets the
	// environment, so one copy is not enough.
	if n := strings.Count(install, "http_proxy=http://127.0.0.1:"); n < 2 {
		t.Errorf("the proxy environment appears %d time(s); it is needed either side of sudo: %s", n, install)
	}
}

// Cluster-internal traffic must not be sent through the operator's machine.
func TestViaProxyExemptsClusterTraffic(t *testing.T) {
	var rec []string
	host := &tunnelHost{fakeHost: fakeHost{name: "s1", rec: &rec}}
	if err := Setup([]Target{{Node: ssh.Node{Host: "s1"}, Client: host}}, nil,
		Options{ViaProxy: true}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	all := strings.Join(rec, "\n")
	for _, want := range []string{"10.0.0.0/8", ".svc", ".cluster.local"} {
		if !strings.Contains(all, want) {
			t.Errorf("NO_PROXY does not exempt %s:\n%s", want, all)
		}
	}
}

// Without --via-proxy nothing is tunnelled and nothing is written.
func TestNoProxyByDefault(t *testing.T) {
	var rec []string
	if err := Setup(fakeTargets(&rec, "s1"), nil, Options{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	all := strings.Join(rec, "\n")
	if strings.Contains(all, "Acquire::http::Proxy") || strings.Contains(all, "HTTP_PROXY") {
		t.Errorf("a proxy was configured without being asked for:\n%s", all)
	}
}

// --apt-mirror rewrites both source formats: Ubuntu 24.04 uses deb822
// .sources files and everything older uses one-line .list entries. Missing one
// leaves half the sources pointing at an archive the node cannot reach.
func TestAptMirrorRewritesBothSourceFormats(t *testing.T) {
	script := aptMirrorScript(AptMirror{URL: "https://nexus.corp/repository/ubuntu"})
	for _, want := range []string{
		"/etc/apt/sources.list.d/*.sources",
		"/etc/apt/sources.list.d/*.list",
		"/etc/apt/sources.list",
		"k3helper.bak",
		"nexus.corp/repository/ubuntu",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the rewrite does not cover %q:\n%s", want, script)
		}
	}
}

func TestAptMirrorIsOptional(t *testing.T) {
	if s := aptMirrorScript(AptMirror{}); s != "" {
		t.Errorf("an empty mirror produced a script:\n%s", s)
	}
	if !(AptMirror{}).empty() {
		t.Error("an empty AptMirror does not report itself empty")
	}
	if (AptMirror{K8sRepo: "https://nexus.corp/k8s"}).empty() {
		t.Error("a mirror with only a Kubernetes repo reports itself empty")
	}
}

// The Kubernetes packages come from their own service, mirrored separately —
// a site may have one mirror and not the other.
func TestK8sAptRepoOverride(t *testing.T) {
	script := kubeadmPrereqScript("v1.31", AptMirror{K8sRepo: "https://nexus.corp/repository/k8s"})
	if !strings.Contains(script, "https://nexus.corp/repository/k8s/Release.key") {
		t.Errorf("the repository key is still fetched upstream:\n%s", script)
	}
	if strings.Contains(script, "pkgs.k8s.io") {
		t.Errorf("pkgs.k8s.io is still referenced after an override:\n%s", script)
	}
}

func TestK8sAptRepoDefaultsUpstream(t *testing.T) {
	script := kubeadmPrereqScript("v1.31", AptMirror{})
	if !strings.Contains(script, "https://pkgs.k8s.io/core:/stable:/v1.31/deb/") {
		t.Errorf("the default repository is wrong:\n%s", script)
	}
}

// The proxy lives inside a running k3helper. If the operator's machine goes
// away mid-install, apt has no timeout of its own: it waits on the dead tunnel
// forever, holding the apt lock. One was found still holding it 31 minutes
// later, failing every later run on that node with a lock error that pointed
// nowhere near the cause.
func TestViaProxyGivesAptATimeout(t *testing.T) {
	var rec []string
	host := &tunnelHost{fakeHost: fakeHost{name: "s1", rec: &rec}}
	if err := Setup([]Target{{Node: ssh.Node{Host: "s1"}, Client: host}}, nil,
		Options{ViaProxy: true}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	all := strings.Join(rec, "\n")
	for _, want := range []string{
		`Acquire::http::Timeout`,
		`Acquire::https::Timeout`,
		`Acquire::http::Pipeline-Depth "0"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the apt configuration is missing %s:\n%s", want, all)
		}
	}
}

// Cluster traffic must never go through the tunnel. kubeadm talks to the API
// server it has just started, and with only localhost exempted it sent that
// through the proxy — which refused the node's own address, because an
// allowlist of internet hosts does not contain it, and the install failed.
func TestViaProxyExemptsTheClusterFromItself(t *testing.T) {
	var rec []string
	host := &tunnelHost{fakeHost: fakeHost{name: "s1", rec: &rec}}
	if err := Setup([]Target{{Node: ssh.Node{Host: "10.104.0.5"}, Client: host}}, nil,
		Options{ViaProxy: true}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	all := strings.Join(rec, "\n")
	// The private ranges cover a node's own address without having to know it.
	for _, want := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", ".cluster.local"} {
		if !strings.Contains(all, want) {
			t.Errorf("NO_PROXY does not exempt %s:\n%s", want, all)
		}
	}
	// And it has to reach the commands, not only containerd's unit file.
	var install string
	for _, c := range rec {
		if strings.Contains(c, "get.k3s.io") {
			install = c
		}
	}
	if !strings.Contains(install, "no_proxy=") || !strings.Contains(install, "10.0.0.0/8") {
		t.Errorf("the install command carries no cluster exemption: %s", install)
	}
}
