package cli

import (
	"fmt"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/spf13/cobra"
)

// version is overridden at build time by the Makefile's ldflags. The literal
// here is the fallback for a bare `go build`, so keep it in step with
// Makefile's VERSION or `go run` reports a stale number.
var version = "0.2.0"

// contextName is the --context value: which cluster to use from a
// multi-cluster targets file. Empty means the file's `current`, or its only
// cluster.
var contextName string

// Host key policy flags. Verification is on by default; both opt-outs are
// explicit because silently trusting any key is how an SSH session gets
// intercepted — and vm setup pipes an install script to sudo sh over it.
var (
	insecureHostKey  bool
	acceptNewHostKey bool
)

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

  vm setup    install k3s or kubeadm on target VMs over SSH
  verify      validate Kubernetes YAML manifests
  gen         generate correct Kubernetes YAML
  deploy      quick deploy manifests to the cluster
  check       run cluster/node/k3s health checks
  doctor      troubleshoot: find issues + remediation (--watch to keep looking)
  ctx         list clusters defined in the targets file
  init        create a targets.yaml describing your nodes
  tui         interactive dashboard + resource browser`,
		SilenceUsage: true,
	}
	// --context selects a cluster from a multi-cluster targets file. It is
	// persistent so every subcommand honours it without repeating the flag.
	root.PersistentFlags().StringVar(&contextName, "context", "", "cluster to use from a multi-cluster targets file")
	root.PersistentFlags().BoolVar(&insecureHostKey, "insecure-host-key", false, "skip SSH host key verification entirely")
	root.PersistentFlags().BoolVar(&acceptNewHostKey, "accept-new-host-key", false, "trust unknown hosts on first use and record them in known_hosts")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if insecureHostKey && acceptNewHostKey {
			return fmt.Errorf("--insecure-host-key and --accept-new-host-key are mutually exclusive")
		}
		switch {
		case insecureHostKey:
			config.HostKeyPolicy = ssh.HostKeyInsecure
		case acceptNewHostKey:
			config.HostKeyPolicy = ssh.HostKeyAcceptNew
		default:
			config.HostKeyPolicy = ssh.HostKeyVerify
		}
		return nil
	}
	root.AddCommand(newVersionCmd())
	root.AddCommand(newInitCmd())
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
			// Not cmd.Printf: cobra sends that to stderr, so `$(k3helper
			// version)` captured nothing and scripts pinning a version saw an
			// empty string.
			fmt.Fprintf(cmd.OutOrStdout(), "k3helper %s\n", version)
			return nil
		},
	}
}
