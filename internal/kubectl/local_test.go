package kubectl

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/solutionforest/k3helper/internal/kube"
)

func TestBase(t *testing.T) {
	tests := []struct {
		name string
		l    Local
		want string
	}{
		{"no context", Local{Kubeconfig: "/home/a/.kube/config"},
			"kubectl --kubeconfig /home/a/.kube/config"},
		{"with context", Local{Kubeconfig: "/k/c.yaml", Context: "prod"},
			"kubectl --kubeconfig /k/c.yaml --context prod"},
		{"path with spaces is quoted", Local{Kubeconfig: `C:\Users\Al An\.kube\config`},
			`kubectl --kubeconfig 'C:\Users\Al An\.kube\config'`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.l.Base(); got != tc.want {
				t.Errorf("Base() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The commands here are the real shapes k3helper builds — copied from
// troubleshoot/gather.go, deploy/deploy.go and kube/kube.go. If one of those
// grows a shape this transport cannot parse, this test is where it shows up.
func TestParseRealCommands(t *testing.T) {
	l := Local{Kubeconfig: "/k/c.yaml", Context: "prod"}
	b := l.Base()
	prefix := []string{"--kubeconfig", "/k/c.yaml", "--context", "prod"}

	tests := []struct {
		name string
		cmd  string
		want []string
		mode stderrMode
	}{
		{
			name: "nodes json with stderr dropped",
			cmd:  b + " get nodes -o json 2>/dev/null",
			want: []string{"get", "nodes", "-o", "json"},
			mode: stderrDrop,
		},
		{
			name: "pods all namespaces",
			cmd:  b + " get pods -A -o json 2>/dev/null",
			want: []string{"get", "pods", "-A", "-o", "json"},
			mode: stderrDrop,
		},
		{
			name: "quoted namespace",
			cmd:  b + " get pods -n 'kube-system' -o json",
			want: []string{"get", "pods", "-n", "kube-system", "-o", "json"},
			mode: stderrMerge,
		},
		{
			name: "jsonpath keeps its braces and slash",
			cmd:  b + " get deployment coredns -n kube-system -o jsonpath={.status.readyReplicas}/{.spec.replicas} 2>/dev/null",
			want: []string{"get", "deployment", "coredns", "-n", "kube-system",
				"-o", "jsonpath={.status.readyReplicas}/{.spec.replicas}"},
			mode: stderrDrop,
		},
		{
			name: "apply dry-run with merged stderr",
			cmd:  b + " apply --dry-run=server -n 'default' -f '/tmp/k3helper-ab12-app.yaml' 2>&1",
			want: []string{"apply", "--dry-run=server", "-n", "default", "-f", "/tmp/k3helper-ab12-app.yaml"},
			mode: stderrMerge,
		},
		{
			name: "rollout status",
			cmd:  b + " rollout status -n 'web' 'deployment/api' --timeout=120s 2>&1",
			want: []string{"rollout", "status", "-n", "web", "deployment/api", "--timeout=120s"},
			mode: stderrMerge,
		},
		{
			name: "label selector",
			cmd:  b + " get pods -n 'default' -l 'app=web,tier=front' -o json",
			want: []string{"get", "pods", "-n", "default", "-l", "app=web,tier=front", "-o", "json"},
			mode: stderrMerge,
		},
		{
			name: "no arguments at all",
			cmd:  b,
			want: nil,
			mode: stderrMerge,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, mode, err := l.parse(tc.cmd)
			if err != nil {
				t.Fatalf("parse(%q) error: %v", tc.cmd, err)
			}
			if mode != tc.mode {
				t.Errorf("stderr mode = %v, want %v", mode, tc.mode)
			}
			want := append(append([]string{}, prefix...), tc.want...)
			if !reflect.DeepEqual(args, want) {
				t.Errorf("parse(%q)\n got %q\nwant %q", tc.cmd, args, want)
			}
		})
	}
}

// A name with a quote in it is quoted by kube.shellQuote as '\” — three
// tokens of punctuation that must come back as one literal quote.
func TestParseEmbeddedQuote(t *testing.T) {
	l := Local{Kubeconfig: "/k/c.yaml"}
	cmd := l.Base() + ` describe -n 'ns' ` + `'pod'\''s-name'`
	args, _, err := l.parse(cmd)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := args[len(args)-1]
	if got != "pod's-name" {
		t.Errorf("last arg = %q, want %q", got, "pod's-name")
	}
}

func TestParseRejectsHostCommands(t *testing.T) {
	l := Local{Kubeconfig: "/k/c.yaml"}
	// Every one of these is a real host-layer probe from check/ or gather.go.
	for _, cmd := range []string{
		`df -P / | tail -1 | awk '{print $5}'`,
		`free -m | awk '/^Mem:/{print $7}'`,
		`systemctl is-active k3s`,
		`test -x /usr/local/bin/k3s || command -v k3s >/dev/null 2>&1`,
		`test -e /etc/kubernetes/admin.conf`,
		`sudo -n k3s certificate check 2>&1`,
		`hostname`,
		`date +%s`,
		// The k3s kubectl base is not this transport's base: a command built
		// for a node must not be run here with the node's paths.
		`sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get nodes -o json`,
	} {
		if _, _, err := l.parse(cmd); !errors.Is(err, ErrHostLayer) {
			t.Errorf("parse(%q) error = %v, want ErrHostLayer", cmd, err)
		}
	}
}

// Refusal has to arrive as exit 127, not as a Go error: callers probe with
// `command -v` and read the code, and an error would abort the diagnosis
// instead of recording that one probe found nothing.
func TestRunRefusesHostCommandWithExit127(t *testing.T) {
	l := Local{Kubeconfig: "/k/c.yaml"}
	out, code, err := l.Run(`systemctl is-active k3s`)
	if err != nil {
		t.Fatalf("Run returned error %v, want nil", err)
	}
	if code != 127 {
		t.Errorf("exit code = %d, want 127", code)
	}
	if !strings.Contains(out, "kubeconfig cluster") {
		t.Errorf("output %q does not explain why", out)
	}
}

func TestRunWithoutKubeconfigIsAnError(t *testing.T) {
	var l Local
	if _, _, err := l.Run("kubectl get pods"); err == nil {
		t.Fatal("empty kubeconfig accepted; it would silently use ~/.kube/config")
	}
}

func TestSplitRejectsShellMetacharacters(t *testing.T) {
	for _, s := range []string{
		`get pods | head -1`,
		`get pods; rm -rf /`,
		`get pods $(whoami)`,
		"get pods `whoami`",
		`get pods > /tmp/out`,
		`get pods & echo hi`,
	} {
		if _, err := split(s); err == nil {
			t.Errorf("split(%q) accepted a shell construct it cannot run", s)
		}
	}
}

func TestSplitUnbalancedQuote(t *testing.T) {
	if _, err := split(`get pods -n 'default`); err == nil {
		t.Error("unbalanced quote accepted")
	}
}

// kube.Builder must take the transport's own base instead of probing for a
// distribution. Probing would fail every arm and silently produce k3s
// commands for a cluster that is not k3s.
func TestKubeBuilderUsesTransportBase(t *testing.T) {
	l := Local{Kubeconfig: "/k/c.yaml", Context: "prod"}
	got := kube.Builder(l)("get pods -A")
	want := l.Base() + " get pods -A"
	if got != want {
		t.Errorf("Builder produced %q, want %q", got, want)
	}
}

func TestWriteAndRemoveFile(t *testing.T) {
	l := Local{Kubeconfig: "/k/c.yaml"}
	path := filepath.Join(t.TempDir(), "nested", "manifest.yaml")
	if err := l.WriteFile(path, []byte("kind: Pod\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Manifests can carry Secrets; the mode has to survive the umask.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	if err := l.RemoveFile(path); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file still present after RemoveFile")
	}
	// Removing what is not there is not an error: callers defer it.
	if err := l.RemoveFile(path); err != nil {
		t.Errorf("second RemoveFile: %v", err)
	}
}

func TestStageDirIsWritable(t *testing.T) {
	if dir := (Local{}).StageDir(); dir == "" {
		t.Fatal("StageDir is empty")
	}
}
