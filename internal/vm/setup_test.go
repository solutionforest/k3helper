package vm

import (
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
