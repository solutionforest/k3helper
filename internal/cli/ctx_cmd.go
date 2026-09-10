package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/spf13/cobra"
)

func newCtxCmd() *cobra.Command {
	var targetsPath string
	cmd := &cobra.Command{
		Use:     "ctx",
		Aliases: []string{"context", "contexts"},
		Short:   "List the clusters defined in the targets file",
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := config.Load(targetsPath)
			if err != nil {
				return err
			}
			active, err := f.Select(contextName)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			// REACHED is worth a column of its own: it decides which commands
			// are available, and "0 nodes" alone looks like a broken file
			// rather than a kubeconfig cluster.
			fmt.Fprintln(w, "\tCLUSTER\tREACHED\tNODES\tSERVER")
			for i := range f.Clusters {
				c := &f.Clusters[i]
				marker := " "
				if c == active {
					marker = "*"
				}
				server := "-"
				if c.Mode() == config.ModeKubeconfig {
					server = c.Kubeconfig
					if c.KubeContext != "" {
						server += " (" + c.KubeContext + ")"
					}
				} else if s, err := c.Server(); err == nil {
					server = s.Host
					if s.Local {
						server = "local"
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", marker, c.Cluster, c.Mode(), len(c.Nodes), server)
			}
			w.Flush()
			if len(f.Clusters) > 1 {
				fmt.Fprintf(cmd.OutOrStdout(), "\nSelect one with --context <name>.\n")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&targetsPath, "targets", "t", "targets.yaml", "path to targets YAML")
	return cmd
}
