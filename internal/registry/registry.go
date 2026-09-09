// Package registry configures the container registries a cluster pulls from:
// private registries, mirrors, and the credentials and CAs they need.
//
// The two distributions want it written in different places and formats. k3s
// reads a single /etc/rancher/k3s/registries.yaml and hands the result to its
// embedded containerd. A kubeadm node has a containerd of its own, which reads
// per-host directories under /etc/containerd/certs.d. Both are produced here
// so the rest of the tool can stay unaware of the difference.
package registry

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/solutionforest/k3helper/internal/config"
)

// K3sPath is where k3s reads its registry configuration.
const K3sPath = "/etc/rancher/k3s/registries.yaml"

// CertsDir is containerd's per-host configuration directory, used on kubeadm
// nodes.
const CertsDir = "/etc/containerd/certs.d"

// k3s registries.yaml, as k3s defines it.
type k3sFile struct {
	Mirrors map[string]k3sMirror `json:"mirrors,omitempty"`
	Configs map[string]k3sConfig `json:"configs,omitempty"`
}

type k3sMirror struct {
	Endpoint []string `json:"endpoint,omitempty"`
}

type k3sConfig struct {
	Auth *k3sAuth `json:"auth,omitempty"`
	TLS  *k3sTLS  `json:"tls,omitempty"`
}

type k3sAuth struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type k3sTLS struct {
	CAFile             string `json:"ca_file,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
}

// RenderK3s produces the contents of /etc/rancher/k3s/registries.yaml.
//
// A registry with no endpoint override still gets a mirror entry pointing at
// itself. That looks redundant and is not: without it, k3s applies the
// `configs` block only to a registry it already knows about, and a private
// host would be contacted with neither the CA nor the credentials.
func RenderK3s(regs []config.Registry) (string, error) {
	if len(regs) == 0 {
		return "", nil
	}
	f := k3sFile{Mirrors: map[string]k3sMirror{}, Configs: map[string]k3sConfig{}}
	for _, r := range regs {
		f.Mirrors[r.Host] = k3sMirror{Endpoint: []string{r.URL()}}

		cfg := k3sConfig{}
		if r.HasAuth() {
			cfg.Auth = &k3sAuth{Username: r.Username, Password: r.Password}
		}
		if r.CAFile != "" || r.InsecureSkipVerify {
			cfg.TLS = &k3sTLS{CAFile: r.CAFile, InsecureSkipVerify: r.InsecureSkipVerify}
		}
		if cfg.Auth != nil || cfg.TLS != nil {
			// Keyed by the endpoint's host:port, which is what containerd
			// matches on — keying by the image-reference name silently fails
			// to apply when the endpoint points somewhere else.
			f.Configs[endpointKey(r)] = cfg
		}
	}
	out, err := yaml.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("render registries.yaml: %w", err)
	}
	return "# Written by k3helper. Edits here are replaced on the next apply.\n" + string(out), nil
}

// endpointKey is the host[:port] containerd matches a config block against.
func endpointKey(r config.Registry) string {
	u := r.URL()
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	return strings.TrimSuffix(strings.SplitN(u, "/", 2)[0], "/")
}

// RenderHostsToml produces containerd's certs.d/<host>/hosts.toml for one
// registry, used on kubeadm nodes.
//
// Credentials are deliberately absent: containerd's own auth lives in
// config.toml, whose schema has moved between containerd 1.x and 2.x, and
// writing to a schema we cannot verify on the node would be a silent failure
// at pull time. ApplyKubeadm refuses credentials and says what to use instead.
func RenderHostsToml(r config.Registry) string {
	var b strings.Builder
	b.WriteString("# Written by k3helper. Edits here are replaced on the next apply.\n")
	fmt.Fprintf(&b, "server = %q\n\n", r.URL())
	fmt.Fprintf(&b, "[host.%q]\n", r.URL())
	b.WriteString("  capabilities = [\"pull\", \"resolve\"]\n")
	if r.CAFile != "" {
		fmt.Fprintf(&b, "  ca = %q\n", r.CAFile)
	}
	if r.InsecureSkipVerify {
		b.WriteString("  skip_verify = true\n")
	}
	return b.String()
}

// Redact returns the registries with passwords replaced, for printing.
func Redact(regs []config.Registry) []config.Registry {
	out := make([]config.Registry, 0, len(regs))
	for _, r := range regs {
		if r.Password != "" {
			r.Password = "********"
		}
		out = append(out, r)
	}
	return out
}

// --- applying to a node ------------------------------------------------------

// Node is what applying needs from a connection. *ssh.Client satisfies it.
type Node interface {
	Run(cmd string) (string, int, error)
	SudoRun(cmd string) (string, int, error)
	WriteFile(remotePath string, data []byte, mode os.FileMode) error
	RemoveFile(remotePath string) error
}

// ApplyK3s writes registries.yaml on a k3s node.
//
// The file is staged in the invoking user's temporary directory with 0600 and
// then installed with sudo, rather than piped through `sudo tee`: a password
// on a command line is visible in `ps` to every user on the machine for as
// long as the command runs.
func ApplyK3s(n Node, regs []config.Registry) error {
	body, err := RenderK3s(regs)
	if err != nil {
		return err
	}
	if body == "" {
		return nil
	}
	return installFile(n, K3sPath, body, "0600")
}

// ApplyKubeadm writes containerd's per-host configuration on a kubeadm node.
func ApplyKubeadm(n Node, regs []config.Registry) error {
	for _, r := range regs {
		if r.HasAuth() {
			return fmt.Errorf(
				"registry %q: k3helper does not write registry credentials into containerd's config on kubeadm nodes.\n"+
					"containerd's auth schema differs between 1.x and 2.x, and writing one we cannot verify on the node "+
					"fails silently at pull time.\n"+
					"Use a pull secret instead: `k3helper gen secret --docker-registry %s --registry-user %s --registry-password ... > pull-secret.yaml`, "+
					"apply it, and reference it from the workload's imagePullSecrets.\n"+
					"Mirrors, CAs and insecure_skip_verify are applied normally — drop the credentials from this entry to configure those",
				r.Host, r.Host, r.Username)
		}
	}
	for _, r := range regs {
		dir := CertsDir + "/" + r.Host
		if out, code, err := n.SudoRun("mkdir -p " + shellQuote(dir)); err != nil || code != 0 {
			return fmt.Errorf("create %s: %s", dir, firstLine(out))
		}
		if err := installFile(n, dir+"/hosts.toml", RenderHostsToml(r), "0644"); err != nil {
			return err
		}
	}
	// containerd only reads certs.d when config_path is set, and the default
	// config does not set it. Without this the files are written and ignored,
	// which is the worst of both outcomes.
	if err := ensureCertsPath(n); err != nil {
		return err
	}
	return nil
}

// ensureCertsPath points containerd's CRI plugin at certs.d if it is not
// already, leaving an existing setting alone.
func ensureCertsPath(n Node) error {
	out, _, _ := n.SudoRun(`grep -l 'config_path' /etc/containerd/config.toml 2>/dev/null`)
	if strings.TrimSpace(out) != "" {
		return nil // already configured, possibly to somewhere else on purpose
	}
	// Appended rather than rewritten: containerd's config is not ours, and
	// regenerating it would discard the cgroup driver kubeadm depends on.
	cmd := fmt.Sprintf(`printf '%%s\n' '' '[plugins."io.containerd.grpc.v1.cri".registry]' `+
		`'  config_path = "%s"' >> /etc/containerd/config.toml`, CertsDir)
	if out, code, err := n.SudoRun(cmd); err != nil || code != 0 {
		return fmt.Errorf("point containerd at %s: %s", CertsDir, firstLine(out))
	}
	return nil
}

// installFile stages content and installs it with the given mode.
func installFile(n Node, path, body, mode string) error {
	tmp := fmt.Sprintf("/tmp/k3helper-registry-%d", len(path))
	if err := n.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return fmt.Errorf("stage %s: %w", path, err)
	}
	defer n.RemoveFile(tmp)

	dir := path[:strings.LastIndex(path, "/")]
	if out, code, err := n.SudoRun("mkdir -p " + shellQuote(dir)); err != nil || code != 0 {
		return fmt.Errorf("create %s: %s", dir, firstLine(out))
	}
	out, code, err := n.SudoRun(fmt.Sprintf("install -m %s -o root -g root %s %s",
		mode, shellQuote(tmp), shellQuote(path)))
	if err != nil || code != 0 {
		return fmt.Errorf("write %s: %s", path, firstLine(out))
	}
	return nil
}

// Restart restarts whatever reads the configuration just written, because
// neither k3s nor containerd re-reads it on its own.
func Restart(n Node, distro string) error {
	units := []string{"k3s", "k3s-agent"}
	if distro == "kubeadm" {
		units = []string{"containerd"}
	}
	for _, unit := range units {
		// Wrapped in `sh -c`, because sudo applies to the first command in a
		// list and nothing after it: `sudo systemctl is-active X && systemctl
		// restart X` runs the restart unprivileged, where it fails, and the
		// trailing `|| true` swallows the failure. The apply then reports
		// success while k3s keeps serving the configuration it started with —
		// which is exactly what this did until a live cluster showed it.
		out, code, err := n.SudoRun(fmt.Sprintf(
			`sh -c 'systemctl is-active %s >/dev/null 2>&1 && systemctl restart %s 2>&1 || true'`, unit, unit))
		if err != nil {
			return fmt.Errorf("restart %s: %w", unit, err)
		}
		if code != 0 && strings.TrimSpace(out) != "" {
			return fmt.Errorf("restart %s: %s", unit, firstLine(out))
		}
	}
	return nil
}

// Hosts lists the registry hosts, sorted, for messages.
func Hosts(regs []config.Registry) []string {
	out := make([]string, 0, len(regs))
	for _, r := range regs {
		out = append(out, r.Host)
	}
	sort.Strings(out)
	return out
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "no output"
	}
	return s
}
