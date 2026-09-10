package cli

import (
	"fmt"
	"strings"

	"github.com/solutionforest/k3helper/internal/config"
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
		cni         string
		noConntrack bool
		regFlags    registryFlags
	)
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install k3s server + agents on all nodes in targets file",
		RunE: func(cmd *cobra.Command, args []string) error {
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
