package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/k3helper/internal/check"
)

func TestParseApplied(t *testing.T) {
	out := `deployment.apps/web created
service/web-svc created
configmap/web-config unchanged
deployment.apps/other configured
error line without format`
	names := parseApplied(out)
	want := []string{"deployment.apps/web", "service/web-svc", "configmap/web-config", "deployment.apps/other"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestDeployDryRunSuccess(t *testing.T) {
	exec := fakeExec{check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f m.yaml 2>&1": {
			Out: "deployment.apps/web created (server dry run)\n", Code: 0,
		},
	}}
	res, err := Deploy(exec, "m.yaml", Options{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Applied) != 1 || res.Applied[0] != "deployment.apps/web" {
		t.Errorf("applied = %v", res.Applied)
	}
}

func TestDeployDryRunValidationFailure(t *testing.T) {
	exec := fakeExec{check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f bad.yaml 2>&1": {
			Out: "error: unable to recognize \"bad.yaml\"\n", Code: 1,
		},
	}}
	_, err := Deploy(exec, "bad.yaml", Options{DryRun: true})
	if err == nil {
		t.Fatal("expected validation failure")
	}
	if !strings.Contains(err.Error(), "dry-run validation failed") {
		t.Errorf("err = %v", err)
	}
}

func TestDeployApplyThenRolloutWait(t *testing.T) {
	exec := fakeExec{check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f m.yaml 2>&1": {
			Out: "deployment.apps/web created (server dry run)\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply -f m.yaml 2>&1": {
			Out: "deployment.apps/web created\nservice/web-svc created\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml rollout status deployment.apps/web --timeout=5s 2>&1": {
			Out: "deployment \"web\" successfully rolled out\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get service/web-svc -o name 2>&1": {
			Out: "service/web-svc\n", Code: 0,
		},
	}}
	res, err := Deploy(exec, "m.yaml", Options{WaitTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.RolledOut) != 2 {
		t.Errorf("rolledOut = %v, want 2 entries", res.RolledOut)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %v", res.Errors)
	}
}

func TestDeployRolloutFailureRecorded(t *testing.T) {
	exec := fakeExec{check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f m.yaml 2>&1": {
			Out: "deployment.apps/web created (server dry run)\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply -f m.yaml 2>&1": {
			Out: "deployment.apps/web created\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml rollout status deployment.apps/web --timeout=5s 2>&1": {
			Out: "error: deployment \"web\" exceeded its progress deadline\n", Code: 1,
		},
	}}
	res, err := Deploy(exec, "m.yaml", Options{WaitTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("deploy itself should not error, only record: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Errorf("errors = %v, want 1", res.Errors)
	}
	if len(res.RolledOut) != 0 {
		t.Errorf("rolledOut = %v, want empty", res.RolledOut)
	}
}

func TestWaitReadyIntegration(t *testing.T) {
	if os.Getenv("K3HELPER_SANDBOX") != "1" {
		t.Skip("sandbox only")
	}
	_ = filepath.Join // keep import
}

// fakeExec adapts check.MapExec to the deploy.Executor interface.
type fakeExec struct{ check.MapExec }

func (f fakeExec) Run(cmd string) (string, int, error) { return f.MapExec.Run(cmd) }
