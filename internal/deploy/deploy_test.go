package deploy

import (
	"os"
	"path/filepath"
	"regexp"
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
	exec := newFakeExec(check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f '<remote>' 2>&1": {
			Out: "deployment.apps/web created (server dry run)\n", Code: 0,
		},
	})
	res, err := Deploy(exec, writeManifest(t), Options{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Applied) != 1 || res.Applied[0] != "deployment.apps/web" {
		t.Errorf("applied = %v", res.Applied)
	}
}

// TestDeployUploadsManifest: the manifest lives on the caller's machine, so
// Deploy must ship it to the target and clean it up rather than assuming
// kubectl can already see the path.
func TestDeployUploadsManifest(t *testing.T) {
	exec := newFakeExec(check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f '<remote>' 2>&1": {
			Out: "namespace/x created (server dry run)\n", Code: 0,
		},
	})
	local := writeManifest(t)
	want, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Deploy(exec, local, Options{DryRun: true}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(exec.written) != 1 {
		t.Fatalf("uploads = %d, want 1", len(exec.written))
	}
	for path, data := range exec.written {
		if !strings.HasPrefix(path, "/tmp/k3helper-") {
			t.Errorf("upload path = %q, want a /tmp/k3helper- temp path", path)
		}
		if string(data) != string(want) {
			t.Errorf("uploaded contents = %q, want %q", data, want)
		}
	}
	if len(exec.removed) != 1 {
		t.Errorf("removed = %v, want the temp file cleaned up", exec.removed)
	}
}

// TestDeployAppliesNamespaceOverride: --namespace must actually reach kubectl.
// It used to be parsed, stored in Options and never read, so deploys landed in
// the default namespace while reporting success.
func TestDeployAppliesNamespaceOverride(t *testing.T) {
	exec := newFakeExec(check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -n 'staging' -f '<remote>' 2>&1": {
			Out: "deployment.apps/web created (server dry run)\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply -n 'staging' -f '<remote>' 2>&1": {
			Out: "deployment.apps/web created\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml rollout status -n 'staging' 'deployment.apps/web' --timeout=5s 2>&1": {
			Out: "deployment \"web\" successfully rolled out\n", Code: 0,
		},
	})
	res, err := Deploy(exec, writeManifest(t), Options{Namespace: "staging", WaitTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.RolledOut) != 1 {
		t.Errorf("rolledOut = %v, want the deployment; namespace flag likely not threaded through", res.RolledOut)
	}
}

// TestDeployRejectsNamespaceConflict: silently redirecting a document that
// names its own namespace is how resources land in the wrong place.
func TestDeployRejectsNamespaceConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ns.yaml")
	manifest := "apiVersion: v1\nkind: Service\nmetadata:\n  name: web\n  namespace: prod\nspec:\n  ports:\n    - port: 80\n"
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := newFakeExec(check.MapExec{})
	_, err := Deploy(exec, path, Options{Namespace: "staging"})
	if err == nil {
		t.Fatal("expected a conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "conflicts with the manifest") {
		t.Errorf("err = %v, want a namespace conflict error", err)
	}
	if len(exec.written) != 0 {
		t.Error("nothing should have been uploaded after a conflict")
	}
}

// A document that agrees with the flag, or omits the namespace, is fine.
func TestDeployAllowsMatchingOrAbsentNamespace(t *testing.T) {
	for name, manifest := range map[string]string{
		"matching": "apiVersion: v1\nkind: Service\nmetadata:\n  name: web\n  namespace: staging\nspec:\n  ports:\n    - port: 80\n",
		"absent":   "apiVersion: v1\nkind: Service\nmetadata:\n  name: web\nspec:\n  ports:\n    - port: 80\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.yaml")
			if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			exec := newFakeExec(check.MapExec{
				"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -n 'staging' -f '<remote>' 2>&1": {
					Out: "service/web created (server dry run)\n", Code: 0,
				},
			})
			if _, err := Deploy(exec, path, Options{Namespace: "staging", DryRun: true}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDeployRejectsInvalidNamespace(t *testing.T) {
	for _, bad := range []string{"Staging", "has space", "x'; rm -rf /; '", "-lead", "trail-"} {
		exec := newFakeExec(check.MapExec{})
		if _, err := Deploy(exec, writeManifest(t), Options{Namespace: bad}); err == nil {
			t.Errorf("namespace %q accepted, want rejection", bad)
		}
	}
}

func TestDeployMissingLocalManifest(t *testing.T) {
	exec := newFakeExec(check.MapExec{})
	_, err := Deploy(exec, filepath.Join(t.TempDir(), "nope.yaml"), Options{DryRun: true})
	if err == nil {
		t.Fatal("expected an error for a manifest that does not exist locally")
	}
	if !strings.Contains(err.Error(), "read manifest") {
		t.Errorf("err = %v, want a read-manifest error", err)
	}
}

func TestDeployDryRunValidationFailure(t *testing.T) {
	exec := newFakeExec(check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f '<remote>' 2>&1": {
			Out: "error: unable to recognize \"bad.yaml\"\n", Code: 1,
		},
	})
	_, err := Deploy(exec, writeManifest(t), Options{DryRun: true})
	if err == nil {
		t.Fatal("expected validation failure")
	}
	if !strings.Contains(err.Error(), "dry-run validation failed") {
		t.Errorf("err = %v", err)
	}
}

func TestDeployApplyThenRolloutWait(t *testing.T) {
	exec := newFakeExec(check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f '<remote>' 2>&1": {
			Out: "deployment.apps/web created (server dry run)\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply -f '<remote>' 2>&1": {
			Out: "deployment.apps/web created\nservice/web-svc created\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml rollout status 'deployment.apps/web' --timeout=5s 2>&1": {
			Out: "deployment \"web\" successfully rolled out\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get 'service/web-svc' -o name 2>&1": {
			Out: "service/web-svc\n", Code: 0,
		},
	})
	res, err := Deploy(exec, writeManifest(t), Options{WaitTimeout: 5 * time.Second})
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
	exec := newFakeExec(check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f '<remote>' 2>&1": {
			Out: "deployment.apps/web created (server dry run)\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply -f '<remote>' 2>&1": {
			Out: "deployment.apps/web created\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml rollout status 'deployment.apps/web' --timeout=5s 2>&1": {
			Out: "error: deployment \"web\" exceeded its progress deadline\n", Code: 1,
		},
	})
	res, err := Deploy(exec, writeManifest(t), Options{WaitTimeout: 5 * time.Second})
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

// fakeExec adapts check.MapExec to the deploy.Executor interface, recording
// the uploads Deploy performs. Commands are matched with the generated remote
// path normalised back to a stable placeholder so cases stay readable.
type fakeExec struct {
	cmds    check.MapExec
	written map[string][]byte
	removed []string
}

func newFakeExec(cmds check.MapExec) *fakeExec {
	return &fakeExec{cmds: cmds, written: map[string][]byte{}}
}

func (f *fakeExec) Run(cmd string) (string, int, error) {
	return f.cmds.Run(normalizeRemotePath(cmd))
}

func (f *fakeExec) WriteFile(remotePath string, data []byte, mode os.FileMode) error {
	f.written[remotePath] = data
	return nil
}

func (f *fakeExec) RemoveFile(remotePath string) error {
	f.removed = append(f.removed, remotePath)
	return nil
}

// normalizeRemotePath rewrites the random /tmp/k3helper-<hex>-<base> path
// Deploy generates into "<remote>" so test fixtures can be written literally.
var remotePathRe = regexp.MustCompile(`/tmp/k3helper-[0-9a-f]{8}-[^' ]*`)

func normalizeRemotePath(cmd string) string {
	return remotePathRe.ReplaceAllString(cmd, "<remote>")
}

// writeManifest creates a local manifest file for Deploy to upload.
func writeManifest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- diff against live state ---

// kubectl diff signals "there are differences" with exit 1. Treating that as
// a failure would make every changed manifest look broken.
func TestDeployDiffExitOneIsNotAnError(t *testing.T) {
	const diff = `diff -u -N /tmp/LIVE/web /tmp/MERGED/web
--- /tmp/LIVE/web
+++ /tmp/MERGED/web
@@ -5,7 +5,7 @@
   replicas: 1
-  image: nginx:1.24
+  image: nginx:1.25`
	exec := newFakeExec(check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f '<remote>' 2>&1": {
			Out: "deployment.apps/web configured (server dry run)\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml diff -f '<remote>' 2>&1": {
			Out: diff, Code: 1,
		},
	})
	res, err := Deploy(exec, writeManifest(t), Options{Diff: true, DryRun: true})
	if err != nil {
		t.Fatalf("exit 1 from kubectl diff must not be an error: %v", err)
	}
	if !strings.Contains(res.Diff, "nginx:1.25") {
		t.Errorf("diff not captured: %q", res.Diff)
	}
}

// Exit 0 means the manifest matches live state exactly.
func TestDeployDiffNoChanges(t *testing.T) {
	exec := newFakeExec(check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f '<remote>' 2>&1": {
			Out: "deployment.apps/web unchanged (server dry run)\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml diff -f '<remote>' 2>&1": {
			Out: "", Code: 0,
		},
	})
	res, err := Deploy(exec, writeManifest(t), Options{Diff: true, DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Diff != "" {
		t.Errorf("Diff = %q, want empty when live state matches", res.Diff)
	}
}

// Anything above exit 1 is a genuine failure and must surface.
func TestDeployDiffRealFailureSurfaces(t *testing.T) {
	exec := newFakeExec(check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f '<remote>' 2>&1": {
			Out: "deployment.apps/web configured (server dry run)\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml diff -f '<remote>' 2>&1": {
			Out: "error: unable to reach the server", Code: 2,
		},
	})
	_, err := Deploy(exec, writeManifest(t), Options{Diff: true, DryRun: true})
	if err == nil {
		t.Fatal("expected an error for a genuine kubectl diff failure")
	}
	if !strings.Contains(err.Error(), "kubectl diff failed") {
		t.Errorf("err = %v", err)
	}
}

// The diff must be taken before anything is applied, and must honour -n.
func TestDeployDiffRunsBeforeApplyAndHonoursNamespace(t *testing.T) {
	exec := newFakeExec(check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -n 'staging' -f '<remote>' 2>&1": {
			Out: "deployment.apps/web configured (server dry run)\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml diff -n 'staging' -f '<remote>' 2>&1": {
			Out: "some diff", Code: 1,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply -n 'staging' -f '<remote>' 2>&1": {
			Out: "deployment.apps/web configured\n", Code: 0,
		},
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml rollout status -n 'staging' 'deployment.apps/web' --timeout=5s 2>&1": {
			Out: "deployment \"web\" successfully rolled out\n", Code: 0,
		},
	})
	res, err := Deploy(exec, writeManifest(t), Options{Diff: true, Namespace: "staging", WaitTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Diff != "some diff" {
		t.Errorf("Diff = %q", res.Diff)
	}
	if len(res.RolledOut) != 1 {
		t.Errorf("deploy should still proceed after the diff: %+v", res)
	}
}

// Without --diff, no diff command runs at all.
func TestDeployWithoutDiffFlagSkipsDiff(t *testing.T) {
	exec := newFakeExec(check.MapExec{
		"sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml apply --dry-run=server -f '<remote>' 2>&1": {
			Out: "deployment.apps/web configured (server dry run)\n", Code: 0,
		},
	})
	res, err := Deploy(exec, writeManifest(t), Options{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Diff != "" {
		t.Errorf("Diff = %q, want empty when --diff was not requested", res.Diff)
	}
}

// A manifest may declare its own namespace while --namespace is not given.
// Waiting for the rollout without -n polled the default namespace and reported
// a failure for a deploy that had actually succeeded.
func TestDeployWaitsInTheManifestsOwnNamespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	manifest := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: prod\n" +
		"spec:\n  replicas: 1\n  selector:\n    matchLabels:\n      app: web\n" +
		"  template:\n    metadata:\n      labels:\n        app: web\n" +
		"    spec:\n      containers:\n        - name: c\n          image: nginx:1.25\n"
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	base := func(args string) string {
		return "sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml " + args + " 2>&1"
	}
	exec := newFakeExec(check.MapExec{
		base("apply --dry-run=server -f '<remote>'"): {Out: "deployment.apps/web created (server dry run)\n", Code: 0},
		base("apply -f '<remote>'"):                  {Out: "deployment.apps/web created\n", Code: 0},
		// the wait must carry -n 'prod', taken from the manifest
		base("rollout status -n 'prod' 'deployment.apps/web' --timeout=5s"): {
			Out: "deployment \"web\" successfully rolled out\n", Code: 0,
		},
	})
	res, err := Deploy(exec, path, Options{WaitTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.RolledOut) != 1 {
		t.Errorf("rolledOut = %v, errors = %v; the wait probably ran without -n", res.RolledOut, res.Errors)
	}
}

// The comment said daemonset; the condition omitted it, so a DaemonSet whose
// pods never started was reported as rolled out because the object existed.
func TestDaemonSetUsesRolloutStatus(t *testing.T) {
	base := func(args string) string {
		return "sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml " + args + " 2>&1"
	}
	exec := newFakeExec(check.MapExec{
		base("apply --dry-run=server -f '<remote>'"): {Out: "daemonset.apps/agent created (server dry run)\n", Code: 0},
		base("apply -f '<remote>'"):                  {Out: "daemonset.apps/agent created\n", Code: 0},
		base("rollout status 'daemonset.apps/agent' --timeout=5s"): {
			Out: "error: daemon set \"agent\" rollout stuck\n", Code: 1,
		},
	})
	res, err := Deploy(exec, writeManifest(t), Options{WaitTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Errorf("a stuck DaemonSet should be reported, got errors=%v rolledOut=%v", res.Errors, res.RolledOut)
	}
}

// A manifest may place resources in different namespaces. Waiting for all of
// them in one namespace reported a failure for a deploy that succeeded.
func TestDeployWaitsPerResourceNamespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multi.yaml")
	doc := func(name, ns string) string {
		return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: " + name +
			"\n  namespace: " + ns + "\nspec:\n  replicas: 1\n  selector:\n    matchLabels:\n      app: " + name +
			"\n  template:\n    metadata:\n      labels:\n        app: " + name +
			"\n    spec:\n      containers:\n        - name: c\n          image: nginx:1.25\n"
	}
	if err := os.WriteFile(path, []byte(doc("a", "team-a")+"---\n"+doc("b", "team-b")), 0o600); err != nil {
		t.Fatal(err)
	}
	base := func(args string) string {
		return "sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml " + args + " 2>&1"
	}
	exec := newFakeExec(check.MapExec{
		base("apply --dry-run=server -f '<remote>'"): {
			Out: "deployment.apps/a created (server dry run)\ndeployment.apps/b created (server dry run)\n", Code: 0,
		},
		base("apply -f '<remote>'"): {
			Out: "deployment.apps/a created\ndeployment.apps/b created\n", Code: 0,
		},
		// each wait must carry its own document's namespace
		base("rollout status -n 'team-a' 'deployment.apps/a' --timeout=5s"): {
			Out: "deployment \"a\" successfully rolled out\n", Code: 0,
		},
		base("rollout status -n 'team-b' 'deployment.apps/b' --timeout=5s"): {
			Out: "deployment \"b\" successfully rolled out\n", Code: 0,
		},
	})
	res, err := Deploy(exec, path, Options{WaitTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.RolledOut) != 2 {
		t.Errorf("rolledOut = %v, errors = %v; each resource should be waited for in its own namespace",
			res.RolledOut, res.Errors)
	}
}

func TestNamespaceForResolution(t *testing.T) {
	declared := map[string]string{"deployment/web": "prod", "service/web-svc": "prod"}
	cases := []struct{ applied, override, want string }{
		// --namespace always wins
		{"deployment.apps/web", "staging", "staging"},
		// group suffix is stripped when matching the manifest
		{"deployment.apps/web", "", "prod"},
		{"service/web-svc", "", "prod"},
		// a resource the manifest did not place gets kubectl's default
		{"configmap/other", "", ""},
		{"malformed", "", ""},
	}
	for _, tc := range cases {
		if got := namespaceFor(tc.applied, tc.override, declared); got != tc.want {
			t.Errorf("namespaceFor(%q, %q) = %q, want %q", tc.applied, tc.override, got, tc.want)
		}
	}
}
