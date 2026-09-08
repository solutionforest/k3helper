package vm

import (
	"strings"
	"testing"

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
