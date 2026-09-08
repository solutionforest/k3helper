package cli

import (
	"github.com/solutionforest/k3helper/internal/config"
	"github.com/spf13/cobra"
)

var version = "0.1.0"

// contextName is the --context value: which cluster to use from a
// multi-cluster targets file. Empty means the file's `current`, or its only
// cluster.
var contextName string

// loadTargets resolves the targets file honouring --context. Every command
// goes through this so the flag cannot be silently ignored by one of them.
func loadTargets(path string) (*config.Targets, error) {
	return config.LoadTargetsContext(path, contextName)
}

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
  ctx         list clusters defined in the targets file
  tui         launch the interactive dashboard`,
		SilenceUsage: true,
	}
	// --context selects a cluster from a multi-cluster targets file. It is
	// persistent so every subcommand honours it without repeating the flag.
	root.PersistentFlags().StringVar(&contextName, "context", "", "cluster to use from a multi-cluster targets file")
	root.AddCommand(newVersionCmd())
	root.AddCommand(newCtxCmd())
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
