//go:build !windows

package kubectl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solutionforest/k3helper/internal/kube"
	"github.com/solutionforest/k3helper/internal/troubleshoot"
)

// fakeKubectl puts a stub kubectl at the front of PATH.
//
// The unit tests above stop at the argument list. This runs the whole path —
// command string, parse, argv, child process, output — because the failure
// this transport is most likely to have is an argument that survives parsing
// and then means something different to a real kubectl.
func fakeKubectl(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunExecutesKubectl(t *testing.T) {
	// Echo the arguments back so the test can assert on exactly what kubectl
	// was called with.
	fakeKubectl(t, `echo "$@"`)
	l := Local{Kubeconfig: "/k/c.yaml", Context: "prod"}

	out, code, err := l.Run(l.Base() + " get pods -n 'kube-system' -o json")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	got := strings.TrimSpace(out)
	want := "--kubeconfig /k/c.yaml --context prod get pods -n kube-system -o json"
	if got != want {
		t.Errorf("kubectl got %q, want %q", got, want)
	}
}

// A non-zero exit is a result, not an error: `kubectl diff` reports 1 for
// "there are differences" and callers read the code.
func TestRunReportsExitCode(t *testing.T) {
	fakeKubectl(t, `echo "boom" >&2; exit 3`)
	l := Local{Kubeconfig: "/k/c.yaml"}
	out, code, err := l.Run(l.Base() + " diff -f '/tmp/x.yaml' 2>&1")
	if err != nil {
		t.Fatalf("Run returned an error for a non-zero exit: %v", err)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("2>&1 did not merge stderr: %q", out)
	}
}

// 2>/dev/null exists because kubectl writes deprecation warnings to stderr and
// a merged stream puts them in front of the JSON, so every parse fails on a
// perfectly healthy cluster.
func TestStderrIsDroppedWhenAsked(t *testing.T) {
	fakeKubectl(t, `echo "Warning: v1 Endpoints is deprecated" >&2; echo '{"items":[]}'`)
	l := Local{Kubeconfig: "/k/c.yaml"}

	quiet, _, err := l.Run(l.Base() + " get endpoints -A -o json 2>/dev/null")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(quiet), "{") {
		t.Errorf("stderr leaked into JSON output: %q", quiet)
	}

	merged, _, err := l.Run(l.Base() + " get endpoints -A -o json")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(merged, "deprecated") {
		t.Errorf("stderr was dropped without being asked: %q", merged)
	}
}

// The whole point of the transport: the existing cluster-layer code, unchanged,
// driven through a kubeconfig instead of over SSH.
func TestListPodsThroughTransport(t *testing.T) {
	fakeKubectl(t, `cat <<'JSON'
{"items":[
 {"metadata":{"name":"web-1","namespace":"default","labels":{"app":"web"}},
  "spec":{"nodeName":"node-a"},
  "status":{"phase":"Running","podIP":"10.0.0.5","startTime":"2026-01-01T00:00:00Z",
   "containerStatuses":[{"ready":true,"restartCount":2}]}}
]}
JSON`)
	l := Local{Kubeconfig: "/k/c.yaml"}
	pods, err := kube.ListPods(l, "default")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("got %d pods, want 1", len(pods))
	}
	p := pods[0]
	if p.Name != "web-1" || p.Namespace != "default" {
		t.Errorf("pod = %s/%s, want default/web-1", p.Namespace, p.Name)
	}
	if p.Restarts != 2 {
		t.Errorf("restarts = %d, want 2", p.Restarts)
	}
	if p.Node != "node-a" {
		t.Errorf("node = %q, want node-a", p.Node)
	}
}

// A gather through a kubeconfig must produce cluster evidence and no host
// evidence, without recording the absent host layer as a failed probe.
func TestGatherThroughTransport(t *testing.T) {
	fakeKubectl(t, `
case "$*" in
  *"get nodes"*) echo '{"items":[{"metadata":{"name":"node-a"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}' ;;
  *) echo '{"items":[]}' ;;
esac`)
	l := Local{Kubeconfig: "/k/c.yaml"}
	e := troubleshoot.Gatherer{Server: l, NoHostLayer: true}.Collect()

	if e.KubeconfigError != "" {
		t.Errorf("cluster layer failed through the transport: %s", e.KubeconfigError)
	}
	if !e.HostLayerUnavailable {
		t.Error("the missing host layer was not recorded")
	}
	if len(e.HostMetrics) != 0 || len(e.K3sService) != 0 {
		t.Error("host evidence appeared for a cluster with no hosts")
	}
	// Certificate collection probes for the k3s binary first. The transport
	// refuses that with exit 127, which must read as "not applicable" and not
	// as a probe that failed.
	if _, recorded := e.ProbeErrors["certificates"]; recorded {
		t.Error("the k3s certificate probe was recorded as a failure on a non-k3s cluster")
	}
}
