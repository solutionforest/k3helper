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
	var targetsPath string
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Run host/k3s health checks on all target nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := config.LoadTargets(targetsPath)
			if err != nil {
				return err
			}
			anyFail := false
			for _, node := range targets.Nodes {
				client, err := ssh.Dial(ssh.Node{Host: node.Host, Port: node.Port, User: node.User, Key: node.Key})
				if err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "✗ %s: unreachable: %v\n", node.Name, err)
					anyFail = true
					continue
				}
				runner := check.NewRunner(
					check.DiskUsageCheck{},
					check.MemoryCheck{},
					check.SwapCheck{},
					check.CgroupCheck{},
					check.K3sServiceCheck{Role: node.Role},
				)
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
						anyFail = true
					}
				}
				w.Flush()
			}
			if anyFail {
				os.Exit(2)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&targetsPath, "targets", "t", "targets.yaml", "path to targets YAML")
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
