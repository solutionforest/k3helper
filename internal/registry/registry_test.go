package registry

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/solutionforest/k3helper/internal/config"
)

func TestRenderK3sMirrorAndConfig(t *testing.T) {
	out, err := RenderK3s([]config.Registry{{
		Host: "docker-registry.example.net", Username: "ci", Password: "s3cr3t",
		CAFile: "/etc/ssl/certs/internal-ca.crt",
	}})
	if err != nil {
		t.Fatalf("RenderK3s: %v", err)
	}
	var f k3sFile
	if err := yaml.Unmarshal([]byte(out), &f); err != nil {
		t.Fatalf("rendered file does not parse: %v\n%s", err, out)
	}
	// A registry that is its own endpoint still needs a mirror entry, or k3s
	// never applies the config block to it.
	m, ok := f.Mirrors["docker-registry.example.net"]
	if !ok || len(m.Endpoint) != 1 || m.Endpoint[0] != "https://docker-registry.example.net" {
		t.Fatalf("mirror entry = %+v", f.Mirrors)
	}
	cfg, ok := f.Configs["docker-registry.example.net"]
	if !ok {
		t.Fatalf("no config block: %+v", f.Configs)
	}
	if cfg.Auth == nil || cfg.Auth.Username != "ci" || cfg.Auth.Password != "s3cr3t" {
		t.Errorf("auth = %+v", cfg.Auth)
	}
	if cfg.TLS == nil || cfg.TLS.CAFile != "/etc/ssl/certs/internal-ca.crt" {
		t.Errorf("tls = %+v", cfg.TLS)
	}
}

// containerd matches config blocks against the endpoint it dials, so a mirror
// pointing elsewhere must key its credentials by the endpoint — keying by the
// image-reference name means the credentials are silently never sent.
func TestRenderK3sKeysConfigByEndpoint(t *testing.T) {
	out, err := RenderK3s([]config.Registry{{
		Host: "docker.io", Endpoint: "https://mirror.example.net:5000", Username: "u", Password: "p",
	}})
	if err != nil {
		t.Fatalf("RenderK3s: %v", err)
	}
	var f k3sFile
	if err := yaml.Unmarshal([]byte(out), &f); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Mirrors["docker.io"]; !ok {
		t.Errorf("mirror should be keyed by the image-reference name: %+v", f.Mirrors)
	}
	if _, ok := f.Configs["mirror.example.net:5000"]; !ok {
		t.Errorf("config should be keyed by the endpoint host:port: %+v", f.Configs)
	}
}

func TestRenderK3sEmpty(t *testing.T) {
	out, err := RenderK3s(nil)
	if err != nil || out != "" {
		t.Errorf("no registries should render nothing, got %q (%v)", out, err)
	}
}

func TestRenderHostsToml(t *testing.T) {
	out := RenderHostsToml(config.Registry{
		Host: "reg.example.net", CAFile: "/etc/ssl/ca.crt",
	})
	for _, want := range []string{`server = "https://reg.example.net"`, `ca = "/etc/ssl/ca.crt"`, `capabilities`} {
		if !strings.Contains(out, want) {
			t.Errorf("hosts.toml missing %q:\n%s", want, out)
		}
	}
	insecure := RenderHostsToml(config.Registry{Host: "reg.example.net", InsecureSkipVerify: true})
	if !strings.Contains(insecure, "skip_verify = true") {
		t.Errorf("insecure registry:\n%s", insecure)
	}
}

func TestRedactLeavesTheOriginalAlone(t *testing.T) {
	regs := []config.Registry{{Host: "r", Username: "u", Password: "hunter2"}}
	red := Redact(regs)
	if red[0].Password != "********" {
		t.Errorf("password not redacted: %q", red[0].Password)
	}
	if regs[0].Password != "hunter2" {
		t.Error("Redact must not mutate its input — the caller still has to apply it")
	}
}

// fakeNode records what would run on the machine.
type fakeNode struct {
	files map[string]string
	cmds  []string
	fail  string // substring of a command that should fail
}

func newFakeNode() *fakeNode { return &fakeNode{files: map[string]string{}} }

func (f *fakeNode) Run(cmd string) (string, int, error) { return f.run(cmd) }

func (f *fakeNode) SudoRun(cmd string) (string, int, error) { return f.run("sudo " + cmd) }

func (f *fakeNode) run(cmd string) (string, int, error) {
	f.cmds = append(f.cmds, cmd)
	if f.fail != "" && strings.Contains(cmd, f.fail) {
		return "permission denied", 1, nil
	}
	return "", 0, nil
}

func (f *fakeNode) WriteFile(path string, data []byte, mode os.FileMode) error {
	f.files[path] = string(data)
	return nil
}

func (f *fakeNode) RemoveFile(path string) error {
	delete(f.files, path)
	return nil
}

func (f *fakeNode) ranContaining(sub string) bool {
	for _, c := range f.cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// The password must never appear in a command: a command line is visible in
// `ps` to every user on the machine.
func TestApplyK3sStagesTheFileInsteadOfEchoingTheSecret(t *testing.T) {
	n := newFakeNode()
	err := ApplyK3s(n, []config.Registry{{Host: "reg.example.net", Username: "ci", Password: "s3cr3t"}})
	if err != nil {
		t.Fatalf("ApplyK3s: %v", err)
	}
	for _, c := range n.cmds {
		if strings.Contains(c, "s3cr3t") {
			t.Fatalf("password leaked into a command line: %q", c)
		}
	}
	if !n.ranContaining("install -m 0600") {
		t.Errorf("registries.yaml must be installed 0600; commands: %v", n.cmds)
	}
	if !n.ranContaining(K3sPath) {
		t.Errorf("nothing wrote %s: %v", K3sPath, n.cmds)
	}
	// The staged copy is removed afterwards.
	if len(n.files) != 0 {
		t.Errorf("temporary file left behind: %v", n.files)
	}
}

func TestApplyK3sReportsAFailedWrite(t *testing.T) {
	n := newFakeNode()
	n.fail = "install -m"
	err := ApplyK3s(n, []config.Registry{{Host: "reg.example.net"}})
	if err == nil {
		t.Fatal("a failed install must be an error, not a silent no-op")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error should carry the node's own message: %v", err)
	}
}

// Writing credentials into a containerd config schema we cannot verify would
// fail silently at pull time; refusing says so and points somewhere that works.
func TestApplyKubeadmRefusesCredentialsWithAWayForward(t *testing.T) {
	n := newFakeNode()
	err := ApplyKubeadm(n, []config.Registry{{Host: "reg.example.net", Username: "ci", Password: "p"}})
	if err == nil {
		t.Fatal("expected credentials on a kubeadm node to be refused")
	}
	if !strings.Contains(err.Error(), "gen secret") || !strings.Contains(err.Error(), "imagePullSecrets") {
		t.Errorf("refusal should name the alternative: %v", err)
	}
	if len(n.cmds) > 0 {
		t.Errorf("nothing should have been written before refusing: %v", n.cmds)
	}
}

func TestApplyKubeadmWritesCertsDAndPointsContainerdAtIt(t *testing.T) {
	n := newFakeNode()
	if err := ApplyKubeadm(n, []config.Registry{{Host: "reg.example.net", CAFile: "/etc/ssl/ca.crt"}}); err != nil {
		t.Fatalf("ApplyKubeadm: %v", err)
	}
	if !n.ranContaining(CertsDir + "/reg.example.net/hosts.toml") {
		t.Errorf("hosts.toml not written: %v", n.cmds)
	}
	// Files under certs.d are ignored unless config_path points at them.
	if !n.ranContaining("config_path") {
		t.Errorf("containerd was never pointed at %s: %v", CertsDir, n.cmds)
	}
}

func TestApplyKubeadmLeavesAnExistingConfigPathAlone(t *testing.T) {
	n := &fakeNode{files: map[string]string{}}
	// grep finds config_path already set.
	n.cmds = nil
	existing := &configuredNode{fakeNode: n}
	if err := ApplyKubeadm(existing, []config.Registry{{Host: "reg.example.net"}}); err != nil {
		t.Fatalf("ApplyKubeadm: %v", err)
	}
	for _, c := range n.cmds {
		if strings.Contains(c, ">> /etc/containerd/config.toml") {
			t.Fatal("must not append to a containerd config that already sets config_path")
		}
	}
}

// configuredNode answers the config_path probe as a node that already has one.
type configuredNode struct{ *fakeNode }

func (c *configuredNode) SudoRun(cmd string) (string, int, error) {
	if strings.Contains(cmd, "grep -l 'config_path'") {
		c.fakeNode.cmds = append(c.fakeNode.cmds, cmd)
		return "/etc/containerd/config.toml\n", 0, nil
	}
	return c.fakeNode.SudoRun(cmd)
}

// sudo applies to the first command of a list and nothing after it, so the
// restart has to be inside a single privileged shell. Without this the apply
// reports success and k3s keeps serving the configuration it started with.
func TestRestartRunsTheWholeCommandAsRoot(t *testing.T) {
	n := newFakeNode()
	if err := Restart(n, "k3s"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	for _, c := range n.cmds {
		if !strings.Contains(c, "restart") {
			continue
		}
		if !strings.HasPrefix(c, "sudo sh -c '") {
			t.Errorf("restart is not wrapped in a privileged shell: %q", c)
		}
	}
}

func TestRestartTargetsTheRightUnits(t *testing.T) {
	n := newFakeNode()
	if err := Restart(n, "k3s"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !n.ranContaining("systemctl restart k3s") || !n.ranContaining("systemctl restart k3s-agent") {
		t.Errorf("k3s restart should cover server and agent units: %v", n.cmds)
	}
	k := newFakeNode()
	if err := Restart(k, "kubeadm"); err != nil {
		t.Fatalf("Restart(kubeadm): %v", err)
	}
	if !k.ranContaining("systemctl restart containerd") {
		t.Errorf("kubeadm restart should target containerd: %v", k.cmds)
	}
	if k.ranContaining("restart k3s") {
		t.Errorf("kubeadm node has no k3s unit: %v", k.cmds)
	}
}

func TestHostsSorted(t *testing.T) {
	got := Hosts([]config.Registry{{Host: "b"}, {Host: "a"}})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("Hosts = %v", got)
	}
}
