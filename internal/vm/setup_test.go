package vm

import (
	"io"

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
		if !strings.Contains(line, "--server https://10.0.0.1:6443") {
			t.Errorf("%s did not join the first server: %s", name, line)
		}
		if !strings.Contains(line, "K3S_TOKEN='K10secret::server:node'") {
			t.Errorf("%s joined without the quoted token: %s", name, line)
		}
	}
	if !strings.Contains(a1, "K3S_URL=https://10.0.0.1:6443") || strings.Contains(a1, "--server ") {
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
	script := kubeadmPrereqScript("v1.31")
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
	if strings.Contains(kubeadmPrereqScript("v1.30"), "stable:/v1.31") {
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
