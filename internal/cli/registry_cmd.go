package cli

import (
	"fmt"
	"strings"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/registry"
	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/solutionforest/k3helper/internal/transport"
	"github.com/spf13/cobra"
)

func newRegistryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Configure the image registries the cluster pulls from",
		Long: `Configure private registries and mirrors on every node.

Registries are declared in the targets file:

  registries:
    - host: docker-registry.example.net
      username: ci
      password_env: REGISTRY_PASSWORD   # read from the environment, not stored here
      ca_file: /etc/ssl/certs/internal-ca.crt

` + "`vm setup`" + ` applies them during the install, before the first image pull.
` + "`registry apply`" + ` pushes them to a cluster that already exists.`,
	}
	cmd.AddCommand(newRegistryApplyCmd(), newRegistryShowCmd())
	return cmd
}

func newRegistryApplyCmd() *cobra.Command {
	var (
		targetsPath string
		distro      string
		noRestart   bool
		regFlags    registryFlags
	)
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Write the registry configuration to every node",
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := loadTargets(targetsPath)
			if err != nil {
				return err
			}
			if err := transport.RequireHosts(targets, "registry apply"); err != nil {
				return err
			}
			regs, err := regFlags.registriesFor(targets)
			if err != nil {
				return err
			}
			if len(regs) == 0 {
				return fmt.Errorf(
					"no registries to apply: add a `registries:` block to %s, or pass --registry", targetsPath)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "applying %d registr%s to %d node(s): %s\n",
				len(regs), plural(len(regs), "y", "ies"), len(targets.Nodes),
				strings.Join(registry.Hosts(regs), ", "))

			for _, n := range targets.Nodes {
				c, err := ssh.Dial(n.SSH())
				if err != nil {
					return fmt.Errorf("connect to %s (%s): %w", n.Name, n.Host, err)
				}
				err = func() error {
					defer c.Close()
					if err := applyRegistries(c, distro, regs); err != nil {
						return err
					}
					if noRestart {
						return nil
					}
					// Neither k3s nor containerd re-reads this file on its
					// own, so an apply without a restart changes nothing that
					// the operator can observe — and they would conclude the
					// registry configuration does not work.
					return registry.Restart(c, distro)
				}()
				if err != nil {
					return fmt.Errorf("%s: %w", n.Name, err)
				}
				fmt.Fprintf(out, "  ✓ %s\n", n.Name)
			}
			if noRestart {
				fmt.Fprintln(out, "\nNot restarted: the new configuration takes effect when k3s "+
					"(or containerd, on kubeadm) next starts. Drop --no-restart to apply it now.")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&targetsPath, "targets", "t", "targets.yaml", "path to targets YAML")
	cmd.Flags().StringVar(&distro, "distro", "k3s", "distribution on these nodes: k3s or kubeadm")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false,
		"write the files but do not restart k3s/containerd (the change only takes effect on the next restart)")
	regFlags.bind(cmd)
	return cmd
}

func newRegistryShowCmd() *cobra.Command {
	var (
		targetsPath string
		distro      string
		regFlags    registryFlags
	)
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the configuration that would be written, with passwords redacted",
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := loadTargets(targetsPath)
			if err != nil {
				return err
			}
			regs, err := regFlags.registriesFor(targets)
			if err != nil {
				return err
			}
			if len(regs) == 0 {
				return fmt.Errorf("no registries declared in %s", targetsPath)
			}
			out := cmd.OutOrStdout()
			// Redacted, because the obvious use of this command is to paste
			// its output into a ticket.
			if distro == "kubeadm" {
				for _, r := range registry.Redact(regs) {
					fmt.Fprintf(out, "── %s/%s/hosts.toml ──\n%s\n", registry.CertsDir, r.Host, registry.RenderHostsToml(r))
				}
				return nil
			}
			body, err := registry.RenderK3s(registry.Redact(regs))
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "── %s ──\n%s", registry.K3sPath, body)
			return nil
		},
	}
	cmd.Flags().StringVarP(&targetsPath, "targets", "t", "targets.yaml", "path to targets YAML")
	cmd.Flags().StringVar(&distro, "distro", "k3s", "distribution on these nodes: k3s or kubeadm")
	regFlags.bind(cmd)
	return cmd
}

// applyRegistries writes the registry configuration for one node.
func applyRegistries(c *ssh.Client, distro string, regs []config.Registry) error {
	if distro == "kubeadm" {
		return registry.ApplyKubeadm(c, regs)
	}
	return registry.ApplyK3s(c, regs)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
