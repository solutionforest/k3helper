package cli

import (
	"fmt"
	"strings"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/proxy"
	"github.com/solutionforest/k3helper/internal/registry"
	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/solutionforest/k3helper/internal/transport"
	"github.com/solutionforest/k3helper/internal/vm"
	"github.com/spf13/cobra"
)

func newVMCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm",
		Short: "Set up k3s on target VMs over SSH",
	}
	cmd.AddCommand(newVMSetupCmd())
	return cmd
}

// registryFlags are the one-off overrides for `vm setup` and `registry apply`.
// A targets file that lists registries needs none of them; they exist for the
// single-registry case where writing a file first is friction.
type registryFlags struct {
	host     string
	user     string
	password string
	caFile   string
	insecure bool
}

func (f *registryFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.host, "registry", "",
		"private registry or mirror host, e.g. docker-registry.example.net (adds to any in the targets file)")
	cmd.Flags().StringVar(&f.user, "registry-user", "", "username for --registry")
	cmd.Flags().StringVar(&f.password, "registry-password", "", "password for --registry (prefer password_env in the targets file)")
	cmd.Flags().StringVar(&f.caFile, "registry-ca-file", "", "path ON THE NODE to the CA that signed the registry certificate")
	cmd.Flags().BoolVar(&f.insecure, "registry-insecure", false, "skip TLS verification for --registry (test registries only)")
}

// registriesFor combines the targets file's registries with the flags.
func (f *registryFlags) registriesFor(targets *config.Targets) ([]config.Registry, error) {
	regs, err := targets.ResolvedRegistries()
	if err != nil {
		return nil, err
	}
	if f.host == "" {
		if f.user != "" || f.password != "" || f.caFile != "" || f.insecure {
			return nil, fmt.Errorf("--registry-* flags need --registry to say which registry they describe")
		}
		return regs, nil
	}
	extra := config.Registry{
		Host: f.host, Username: f.user, Password: f.password,
		CAFile: f.caFile, InsecureSkipVerify: f.insecure,
	}
	// Validated through the same path as a file entry, so `--registry-user`
	// without a password fails here rather than at pull time on the node.
	probe := config.Targets{Cluster: targets.Cluster, Nodes: targets.Nodes, Registries: append(regs, extra)}
	if err := probe.Validate(); err != nil {
		return nil, err
	}
	return probe.Registries, nil
}

func newVMSetupCmd() *cobra.Command {
	var (
		targetsPath string
		channel     string
		token       string
		kubeconfig  string
		extraArgs   string
		distro      string
		k8sVersion  string
		k3sVersion  string
		bundleDir   string
		joinAddress string
		aptMirror   string
		k8sAptRepo  string
		viaProxy    bool
		proxyAllow  []string
		cni         string
		noConntrack bool
		regFlags    registryFlags
	)
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install k3s server + agents on all nodes in targets file",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Flag combinations are settled before anything is dialled. A
			// contradiction is not worth an SSH round trip to every node to
			// discover, and one of these decides whether the nodes are given
			// a route to the internet at all.
			if err := checkSetupFlags(distro, bundleDir, k3sVersion, viaProxy, proxyAllow); err != nil {
				return err
			}
			if viaProxy {
				// Said out loud, every time. Lending an isolated machine a
				// route out is the operator's decision to make knowingly, and
				// in some environments it is not theirs to make at all.
				fmt.Fprintf(cmd.OutOrStdout(),
					"--via-proxy: these nodes will reach the internet through this machine for the "+
						"length of the install, and only through it.\n"+
						"  allowed: %s\n"+
						"  the tunnel and its configuration are removed when the install finishes.\n\n",
					strings.Join(proxyAllowSummary(proxyAllow), ", "))
			}

			targets, err := loadTargets(targetsPath)
			if err != nil {
				return err
			}
			if err := transport.RequireHosts(targets, "vm setup"); err != nil {
				return err
			}
			// Servers first, then agents. Multiple servers switch k3s to
			// embedded etcd; the first one initialises the cluster.
			var servers, agents []vm.Target
			var conns []*ssh.Client
			// Kept concretely: FetchKubeconfig needs the real client, not the
			// narrower interface Setup takes.
			var firstServer *ssh.Client
			defer func() {
				for _, c := range conns {
					c.Close()
				}
			}()
			for _, n := range targets.Nodes {
				c, err := ssh.Dial(n.SSH())
				if err != nil {
					return fmt.Errorf("connect to %s (%s): %w", n.Name, n.Host, err)
				}
				conns = append(conns, c)
				t := vm.Target{Node: n.SSH(), Client: c}
				if n.Role == "server" {
					if firstServer == nil {
						firstServer = c
					}
					servers = append(servers, t)
				} else {
					agents = append(agents, t)
				}
			}
			if len(servers) == 0 {
				return fmt.Errorf("no server node in targets")
			}
			srvNode := &targets.Nodes[0]
			for i := range targets.Nodes {
				if targets.Nodes[i].Role == "server" {
					srvNode = &targets.Nodes[i]
					break
				}
			}

			// Registries are written before the install, not after: the very
			// first thing a fresh node does is pull images, and a mirror
			// configured afterwards would be too late for exactly the pulls
			// an air-gapped or credentialled environment needs it for.
			regs, err := regFlags.registriesFor(targets)
			if err != nil {
				return err
			}
			if len(regs) > 0 {
				which := "k3s"
				if distro == "kubeadm" {
					which = "kubeadm"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "configuring %d registr%s on %d node(s): %s\n",
					len(regs), plural(len(regs), "y", "ies"), len(conns), strings.Join(registry.Hosts(regs), ", "))
				for i, c := range conns {
					if err := applyRegistries(c, which, regs); err != nil {
						return fmt.Errorf("%s: %w", targets.Nodes[i].Name, err)
					}
				}
			}

			if distro == "kubeadm" {
				if err := vm.SetupKubeadm(servers, agents, vm.KubeadmOptions{
					Version:             k8sVersion,
					CNI:                 cni,
					InitExtraArgs:       extraArgs,
					SkipConntrackTuning: noConntrack,
					JoinAddress:         joinAddress,
					Mirror:              vm.AptMirror{URL: aptMirror, K8sRepo: k8sAptRepo},
					ViaProxy:            viaProxy,
					ProxyAllow:          proxyAllow,
					Progress:            cmd.OutOrStdout(),
				}); err != nil {
					return err
				}
				if kubeconfig != "" {
					if err := vm.FetchKubeadmKubeconfig(firstServer, kubeconfig, srvNode.Host); err != nil {
						return fmt.Errorf("fetch kubeconfig: %w", err)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "kubeconfig written to %s\n", kubeconfig)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "✓ cluster ready")
				return nil
			}
			if distro != "" && distro != "k3s" {
				return fmt.Errorf("unknown --distro %q: use k3s or kubeadm", distro)
			}

			err = vm.Setup(servers, agents, vm.Options{
				Channel:         channel,
				Version:         k3sVersion,
				BundleDir:       bundleDir,
				JoinAddress:     joinAddress,
				Mirror:          vm.AptMirror{URL: aptMirror, K8sRepo: k8sAptRepo},
				ViaProxy:        viaProxy,
				ProxyAllow:      proxyAllow,
				Token:           token,
				ServerExtraArgs: extraArgs,
				AgentExtraArgs:  agentExtraArgs,
				Progress:        cmd.OutOrStdout(),
			})
			if err != nil {
				return err
			}
			if kubeconfig != "" {
				if err := vm.FetchKubeconfig(firstServer, kubeconfig, srvNode.Host); err != nil {
					return fmt.Errorf("fetch kubeconfig: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "kubeconfig written to %s\n", kubeconfig)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "✓ cluster ready")
			return nil
		},
	}
	cmd.Flags().StringVarP(&targetsPath, "targets", "t", "targets.yaml", "path to targets YAML")
	cmd.Flags().StringVar(&channel, "channel", "stable", "k3s release channel (stable/latest/vX.Y)")
	cmd.Flags().StringVar(&token, "token", "", "join token (generated by server if empty)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "write fetched kubeconfig to this path")
	cmd.Flags().StringVar(&extraArgs, "server-extra-args", "", "extra args for the server install (k3s install script, or `kubeadm init`)")
	cmd.Flags().StringVar(&distro, "distro", "k3s", "distribution to install: k3s or kubeadm")
	cmd.Flags().StringVar(&k8sVersion, "k8s-version", "", "Kubernetes minor series for kubeadm, e.g. v1.31")
	cmd.Flags().StringVar(&k3sVersion, "k3s-version", "",
		"pin an exact k3s release, e.g. v1.31.2+k3s1 (skips the update.k3s.io channel lookup)")
	cmd.Flags().StringVar(&bundleDir, "bundle", "",
		"install from an offline bundle (see `k3helper bundle k3s`); the nodes need no internet")
	cmd.Flags().StringVar(&joinAddress, "join-address", "",
		"address the other nodes dial to reach the first server (default: its host from the targets file)")
	cmd.Flags().StringVar(&aptMirror, "apt-mirror", "",
		"point the nodes' package manager at this archive instead of the distribution's, "+
			"e.g. https://nexus.corp/repository/ubuntu")
	cmd.Flags().StringVar(&k8sAptRepo, "k8s-apt-repo", "",
		"mirror of the Kubernetes package repository (default: pkgs.k8s.io); kubeadm only")
	cmd.Flags().BoolVar(&viaProxy, "via-proxy", false,
		"lend the nodes this machine's internet connection for the length of the install, "+
			"over the SSH connection already open to them")
	cmd.Flags().StringArrayVar(&proxyAllow, "proxy-allow", nil,
		"extra host allowed through --via-proxy; repeat for more, or \"*\" for anything")
	cmd.Flags().StringVar(&cni, "cni", "flannel", "CNI for kubeadm: flannel or calico")
	cmd.Flags().BoolVar(&noConntrack, "no-conntrack-tuning", false,
		"stop kube-proxy managing nf_conntrack_max; needed where that sysctl is read-only or capped (nested VMs, containers)")
	cmd.Flags().StringVar(&agentExtraArgs, "agent-extra-args", "", "extra args for k3s agent install")
	regFlags.bind(cmd)
	return cmd
}

var agentExtraArgs string

func toSSHNode(n config.Node) ssh.Node {
	return n.SSH()
}

// proxyAllowSummary describes what --via-proxy will permit, for the notice
// printed before it is opened.
func proxyAllowSummary(extra []string) []string {
	for _, a := range extra {
		if a == "*" {
			return []string{"anything (--proxy-allow \"*\")"}
		}
	}
	out := []string{fmt.Sprintf("%d default hosts (distribution mirrors, pkgs.k8s.io, registry.k8s.io, docker.io)",
		len(proxy.DefaultAllow))}
	if len(extra) > 0 {
		out = append(out, strings.Join(extra, ", "))
	}
	return out
}

// checkSetupFlags rejects combinations that cannot mean what they say.
//
// Silently ignoring one of these is how an operator asks for an offline
// install, gets an online one, and finds out on an air-gapped node from a TLS
// error at the worst possible moment.
func checkSetupFlags(distro, bundleDir, k3sVersion string, viaProxy bool, proxyAllow []string) error {
	if distro == "kubeadm" {
		if bundleDir != "" {
			return fmt.Errorf("--bundle builds a k3s bundle and only the k3s installer can use it; " +
				"an offline kubeadm install needs distribution packages and registry.k8s.io images. " +
				"Use --apt-mirror and --k8s-apt-repo to point at your own mirrors, --via-proxy to " +
				"lend the nodes this machine's connection, or --distro k3s, which installs fully offline")
		}
		if k3sVersion != "" {
			return fmt.Errorf("--k3s-version applies to --distro k3s; for kubeadm use --k8s-version")
		}
	}
	if bundleDir != "" && k3sVersion != "" {
		return fmt.Errorf("--bundle and --k3s-version contradict each other: " +
			"a bundle already contains one exact k3s release, recorded in its bundle.json")
	}
	if viaProxy && bundleDir != "" {
		return fmt.Errorf("--via-proxy and --bundle are two answers to the same problem: " +
			"a bundle installs with no network at all, so lending the nodes one does nothing")
	}
	if len(proxyAllow) > 0 && !viaProxy {
		return fmt.Errorf("--proxy-allow only means something with --via-proxy")
	}
	return nil
}
