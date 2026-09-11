package vm

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// SetupKubeadm bootstraps a kubeadm cluster: prerequisites on every node,
// `kubeadm init` on the first server, then joins.
//
// This is deliberately narrower than the k3s path. k3s ships one binary that
// makes every decision for you; kubeadm expects a container runtime, kernel
// modules, sysctls, a package repository and a CNI to be arranged first, and
// gets those wrong silently rather than loudly. Each step here is explicit so
// a failure names the step that failed.
func SetupKubeadm(servers []Target, agents []Target, opts KubeadmOptions) error {
	progressf := func(format string, a ...interface{}) {
		if opts.Progress != nil {
			fmt.Fprintf(opts.Progress, format+"\n", a...)
		}
	}
	if len(servers) == 0 {
		return fmt.Errorf("at least one server is required")
	}
	if len(servers) > 1 {
		// Joining more control-plane nodes needs a certificate key from
		// `kubeadm init --upload-certs` and a load balancer in front of the
		// API servers. Saying so beats building half of it.
		return fmt.Errorf(
			"kubeadm HA is not supported yet: joining extra control-plane nodes needs " +
				"--upload-certs and an API server endpoint in front of them. Use one server, " +
				"or k3s (which handles this) for HA")
	}

	all := append(append([]Target{}, servers...), agents...)

	// 1. prerequisites, every node
	for _, t := range all {
		// Before apt runs, or it collides with cloud-init's dpkg lock on a
		// machine that has only just booted.
		waitForCloudInit(t, opts.Progress)
		progressf("[%s] preparing host (containerd, kernel modules, sysctls, kubeadm)...", t.Node.Host)
		// The script is multi-line and contains quotes of its own, so it is
		// shipped base64-encoded and fed to a root shell rather than being
		// interpolated into one.
		run := fmt.Sprintf(`printf %%s %s | base64 -d | %sbash -s`,
			shellQuote(base64.StdEncoding.EncodeToString([]byte(kubeadmPrereqScript(opts.Version)))),
			t.Client.SudoPrefix())
		if code, err := streamSudo(t.Client, run, opts.Progress); err != nil || code != 0 {
			return fmt.Errorf("prepare %s: %s", t.Node.Host, exitReason(code, err))
		}
	}

	// 2. control plane
	first := servers[0]

	// Pin the address the API server advertises, because the join command the
	// agents are handed later is built from it.
	//
	// kubeadm's default is the address of the default route's interface, which
	// on a cloud VM is the public one — the same trap the k3s path fell into,
	// where agents on a private network were handed a public address they
	// could not reach and retried against it indefinitely. The address from
	// the targets file is the one the operator chose and the one k3helper has
	// just proved works by connecting over it.
	advertise := ""
	if ip, err := resolveJoinAddress(opts.JoinAddress, first, first.Client); err == nil && net.ParseIP(ip) != nil {
		advertise = " --apiserver-advertise-address=" + shellQuote(ip)
		progressf("[%s] API server will advertise %s", first.Node.Host, ip)
	}

	progressf("[%s] kubeadm init (pod network %s)...", first.Node.Host, opts.podCIDR())
	initCmd := fmt.Sprintf(
		`%skubeadm init --pod-network-cidr=%s%s%s`,
		first.Client.SudoPrefix(), shellQuote(opts.podCIDR()), advertise, withSpace(opts.InitExtraArgs))
	if opts.SkipConntrackTuning {
		// A config file rather than flags: kube-proxy's conntrack settings
		// have no command-line equivalent on kubeadm init.
		cfg := kubeadmConfig(opts.podCIDR())
		if _, code, err := first.Client.Run(fmt.Sprintf(
			`printf %%s %s | base64 -d | %stee /etc/kubernetes/k3helper-init.yaml >/dev/null`,
			shellQuote(base64.StdEncoding.EncodeToString([]byte(cfg))), first.Client.SudoPrefix(),
		)); err != nil || code != 0 {
			return fmt.Errorf("write kubeadm config: %s", exitReason(code, err))
		}
		initCmd = fmt.Sprintf(`%skubeadm init --config /etc/kubernetes/k3helper-init.yaml%s%s`,
			first.Client.SudoPrefix(), advertise, withSpace(opts.InitExtraArgs))
	}
	if code, err := streamSudo(first.Client, initCmd, opts.Progress); err != nil || code != 0 {
		return fmt.Errorf("kubeadm init failed: %s", exitReason(code, err))
	}

	// kubectl for the login user, so `check` and `doctor` work without sudo
	// gymnastics afterwards.
	if _, code, err := first.Client.SudoRun(
		`mkdir -p $HOME/.kube && cp -f /etc/kubernetes/admin.conf $HOME/.kube/config && chown $(id -u):$(id -g) $HOME/.kube/config`,
	); err != nil || code != 0 {
		progressf("[%s] warning: could not install a user kubeconfig", first.Node.Host)
	}

	// 3. CNI — without one every node stays NotReady, which looks like a
	// broken install rather than a missing component.
	progressf("[%s] installing CNI (%s)...", first.Node.Host, opts.cni())
	cniCmd := fmt.Sprintf(`%skubectl --kubeconfig /etc/kubernetes/admin.conf apply -f %s`,
		first.Client.SudoPrefix(), shellQuote(opts.cniManifest()))
	if code, err := streamSudo(first.Client, cniCmd, opts.Progress); err != nil || code != 0 {
		return fmt.Errorf("install CNI: %s", exitReason(code, err))
	}

	// 4. join
	if len(agents) > 0 {
		progressf("[%s] generating join command...", first.Node.Host)
		out, code, err := first.Client.SudoRun(`kubeadm token create --print-join-command`)
		if err != nil || code != 0 {
			return fmt.Errorf("create join token (exit %d): %s", code, strings.TrimSpace(out))
		}
		join := strings.TrimSpace(lastNonEmptyLine(out))
		if !strings.Contains(join, "kubeadm join") {
			return fmt.Errorf("unexpected join command: %q", join)
		}
		for _, a := range agents {
			progressf("[%s] joining cluster...", a.Node.Host)
			if code, err := streamSudo(a.Client, a.Client.SudoPrefix()+join, opts.Progress); err != nil || code != 0 {
				return fmt.Errorf("join %s failed: %s", a.Node.Host, exitReason(code, err))
			}
		}
	}

	progressf("waiting for %d node(s) to become ready...", len(all))
	return waitReady(kubeadmRunner{first.Client}, len(all),
		time.Duration(opts.readyTimeout())*time.Second)
}

// KubeadmOptions control the kubeadm bootstrap.
type KubeadmOptions struct {
	// Version is the Kubernetes minor series to install, e.g. "v1.31".
	Version string
	// PodCIDR must match what the CNI expects; the default matches Flannel.
	PodCIDR string
	// CNI is "flannel" or "calico".
	CNI string
	// InitExtraArgs is appended to `kubeadm init`.
	InitExtraArgs string
	// JoinAddress overrides the address the API server advertises, which is
	// the address the join command hands to every agent. Empty takes it from
	// the targets file.
	JoinAddress string
	// SkipConntrackTuning stops kube-proxy managing nf_conntrack_max.
	//
	// kube-proxy raises that sysctl at startup and dies if it cannot. On hosts
	// where /proc/sys/net/netfilter is read-only or capped below what it wants
	// — nested VMs, containers, some managed images — that failure takes
	// Service routing down, and the CNI then cannot reach the API service
	// either. The symptom (a crashlooping CNI) points nowhere near the cause.
	SkipConntrackTuning bool
	Progress            io.Writer
	ReadyTimeout        int // seconds; 0 uses a sensible default
}

func (o KubeadmOptions) version() string {
	if o.Version == "" {
		return "v1.31"
	}
	return o.Version
}

func (o KubeadmOptions) podCIDR() string {
	if o.PodCIDR != "" {
		return o.PodCIDR
	}
	if o.cni() == "calico" {
		return "192.168.0.0/16"
	}
	return "10.244.0.0/16" // Flannel's default; init must agree with the CNI
}

func (o KubeadmOptions) cni() string {
	if o.CNI == "" {
		return "flannel"
	}
	return strings.ToLower(o.CNI)
}

func (o KubeadmOptions) cniManifest() string {
	if o.cni() == "calico" {
		return "https://raw.githubusercontent.com/projectcalico/calico/v3.28.0/manifests/calico.yaml"
	}
	return "https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml"
}

func (o KubeadmOptions) readyTimeout() int {
	if o.ReadyTimeout > 0 {
		return o.ReadyTimeout
	}
	return 240
}

// kubeadmPrereqScript prepares a host for kubeadm.
//
// Every line here is something kubeadm requires and does not do for you, and
// whose absence produces a confusing failure much later: swap on makes the
// kubelet refuse to start, the br_netfilter module and its sysctls are what
// let pod traffic be seen by iptables, and without a CRI there is nothing to
// run containers with.
func kubeadmPrereqScript(version string) string {
	if version == "" {
		version = "v1.31"
	}
	return `set -e
export DEBIAN_FRONTEND=noninteractive

# kubelet refuses to start with swap enabled.
swapoff -a || true
sed -i.bak '/[[:space:]]swap[[:space:]]/s/^/#/' /etc/fstab 2>/dev/null || true

# Pod traffic must traverse iptables, and nodes must forward it.
modprobe overlay || true
modprobe br_netfilter || true
modprobe nf_conntrack || true
printf 'overlay\nbr_netfilter\nnf_conntrack\n' > /etc/modules-load.d/k8s.conf

# nf_conntrack_max is raised here rather than left to kube-proxy. kube-proxy
# writes it itself only when the running value is lower than it wants, and on
# hosts where /proc/sys/net/netfilter is read-only that write fails and takes
# kube-proxy down with it — which stops Service routing, which then stops the
# CNI reaching the API server. Setting it high up front means kube-proxy finds
# nothing to change.
printf 'net.bridge.bridge-nf-call-iptables  = 1\nnet.bridge.bridge-nf-call-ip6tables = 1\nnet.ipv4.ip_forward                 = 1\nnet.netfilter.nf_conntrack_max      = 1048576\n' > /etc/sysctl.d/k8s.conf
sysctl --system >/dev/null 2>&1 || true

apt-get update -qq
# conntrack is a hard kubeadm preflight requirement; socat is what
# "kubectl port-forward" uses on the node; ethtool and iptables are needed by
# most CNIs. Missing any of them fails late and unhelpfully.
apt-get install -y -qq apt-transport-https ca-certificates curl gpg containerd \
  conntrack socat ethtool iptables

# containerd's shipped default disables CRI; kubeadm needs it, and the cgroup
# driver must match the kubelet's (systemd).
mkdir -p /etc/containerd
containerd config default > /etc/containerd/config.toml
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
systemctl restart containerd
systemctl enable containerd

install -m 755 -d /etc/apt/keyrings
curl -fsSL https://pkgs.k8s.io/core:/stable:/` + version + `/deb/Release.key |
  gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg --yes
echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/` + version + `/deb/ /" \
  > /etc/apt/sources.list.d/kubernetes.list
apt-get update -qq
apt-get install -y -qq kubelet kubeadm kubectl
apt-mark hold kubelet kubeadm kubectl >/dev/null
systemctl enable kubelet
`
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// kubeadmRunner adapts a Host to the Runner waitReady expects, translating the
// k3s kubectl invocation into kubeadm's.
type kubeadmRunner struct{ h Host }

func (k kubeadmRunner) Run(cmd string) (string, int, error) { return k.h.Run(cmd) }

func (k kubeadmRunner) SudoRun(cmd string) (string, int, error) {
	// waitReady asks in k3s's dialect; on kubeadm the same query goes through
	// kubectl with the admin kubeconfig.
	cmd = strings.Replace(cmd, "k3s kubectl", "kubectl --kubeconfig /etc/kubernetes/admin.conf", 1)
	return k.h.SudoRun(cmd)
}

// exitReason describes a failed remote command. A non-zero exit with no Go
// error is the normal case — formatting that with %w prints "%!w(<nil>)" and
// tells the reader nothing.
func exitReason(code int, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("exit %d (see the output above)", code)
}

// kubeadmConfig is an init configuration that turns off kube-proxy's conntrack
// tuning while keeping the pod network the CNI expects.
func kubeadmConfig(podCIDR string) string {
	return `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
networking:
  podSubnet: ` + podCIDR + `
---
apiVersion: kubeproxy.config.k8s.io/v1alpha1
kind: KubeProxyConfiguration
conntrack:
  # 0 leaves nf_conntrack_max alone. kube-proxy otherwise raises it at startup
  # and exits if the write is refused, which is fatal on a host where that
  # sysctl is read-only or capped.
  maxPerCore: 0
  min: 0
`
}
