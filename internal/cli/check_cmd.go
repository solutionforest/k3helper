package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/solutionforest/k3helper/internal/check"
	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/spf13/cobra"
)

func newCheckCmd() *cobra.Command {
	var (
		targetsPath string
		strict      bool
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Run host/k3s health checks on all target nodes",
		Long: `Run host and Kubernetes-service health checks on every target node.

Exit code: 0 when nothing failed, 2 when a check failed or a node was
unreachable. Warnings are printed but do not change the exit code unless
--strict is given: a swap warning should not fail a pipeline the same way a
dead k3s does.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := loadTargets(targetsPath)
			if err != nil {
				return err
			}
			if targets.Mode() == config.ModeKubeconfig {
				// Every check in this command reads the machine. Say so once,
				// plainly, instead of printing a screen of skipped rows for a
				// cluster that was never going to have them.
				fmt.Fprintf(cmd.OutOrStdout(),
					"cluster %q is reached through a kubeconfig: %s\n\n"+
						"Run `k3helper doctor` for the checks that work through the API server.\n",
					targets.Cluster, check.HostReason)
				return nil
			}
			anyFail, anyWarn := false, false
			for _, node := range targets.Nodes {
				client, err := ssh.Dial(node.SSH())
				if err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "✗ %s: unreachable: %v\n", node.Name, err)
					anyFail = true
					continue
				}
				runner := check.NewRunner(check.HostChecks(node.Role)...)
				results := runner.RunAll(check.Context{Exec: execAdapter{client}, Node: node.Name})
				client.Close()

				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintf(w, "\n== node %s (%s) ==\n", node.Name, node.Role)
				for _, r := range results {
					fmt.Fprintf(w, "%s\t%s\t%s\n", r.Status.Icon(), r.Status, r.Summary)
					if r.Status == check.Fail || r.Status == check.Warn {
						if r.Remediation != "" {
							fmt.Fprintf(w, " \t↳ fix:\t%s\n", r.Remediation)
						}
					}
					switch r.Status {
					case check.Fail:
						anyFail = true
					case check.Warn:
						anyWarn = true
					}
				}
				w.Flush()
			}
			if anyWarn && !anyFail {
				fmt.Fprintln(cmd.OutOrStdout(), "\nwarnings only — exit 0 (use --strict to fail on warnings)")
			}
			if anyFail || (strict && anyWarn) {
				os.Exit(2)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&targetsPath, "targets", "t", "targets.yaml", "path to targets YAML")
	cmd.Flags().BoolVar(&strict, "strict", false, "exit 2 on warnings as well as failures")
	return cmd
}

// execAdapter bridges ssh.Client to check.Executor.
type execAdapter struct{ c *ssh.Client }

func (a execAdapter) Run(cmd string) (string, int, error) {
	out, code, err := a.c.Run(cmd)
	if err != nil {
		return out, code, err
	}
	return out, code, nil
}
