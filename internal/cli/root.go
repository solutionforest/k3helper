package cli

import (
	"github.com/spf13/cobra"
)

var version = "0.1.0"

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "k3helper",
		Short: "Portable k3s/k8s helper: VM setup, YAML verify/generate, deploy, doctor",
		Long: `k3helper is a portable k3s/kubernetes helper with a TUI.

  vm setup    install k3s on target VMs over SSH
  verify      validate Kubernetes YAML manifests
  gen         generate correct Kubernetes YAML
  deploy      quick deploy manifests to the cluster
  check       run cluster/node/k3s health checks
  doctor      troubleshoot: find issues + remediation
  tui         launch the interactive dashboard`,
		SilenceUsage: true,
	}
	root.AddCommand(newVersionCmd())
	root.AddCommand(newVerifyCmd())
	root.AddCommand(newGenCmd())
	root.AddCommand(newCheckCmd())
	root.AddCommand(newVMCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newDeployCmd())
	root.AddCommand(newTUICmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.Printf("k3helper %s\n", version)
			return nil
		},
	}
}
