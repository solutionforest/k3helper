package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/solutionforest/k3helper/internal/troubleshoot"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var targetsPath string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Troubleshoot: gather evidence across host + k3s + cluster, rank root causes with fixes",
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := config.LoadTargets(targetsPath)
			if err != nil {
				return err
			}
			srvNode, err := targets.Server()
			if err != nil {
				return err
			}
			server, err := ssh.Dial(toSSHNode(*srvNode))
			if err != nil {
				return fmt.Errorf("connect to server: %w", err)
			}
			defer server.Close()

			hosts := map[string]ssh.Executor{}
			for _, n := range targets.Nodes {
				if n.Role == "agent" {
					if c, err := ssh.Dial(toSSHNode(n)); err == nil {
						defer c.Close()
						hosts[n.Name] = c
					}
				} else {
					hosts[n.Name] = server
				}
			}

			g := troubleshoot.Gatherer{Server: server, Hosts: hosts}
			evidence := g.Collect()
			diagnoses := troubleshoot.Diagnose(evidence)

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			if evidence.KubeconfigError != "" {
				fmt.Fprintf(w, "! kubeconfig/cluster access problem:\t%s\n", evidence.KubeconfigError)
			}
			if len(diagnoses) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "✓ no issues detected — cluster looks healthy")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Found %d likely issue(s), ranked by confidence:\n\n", len(diagnoses))
			for i, d := range diagnoses {
				fmt.Fprintf(w, "%d.\t[%d%%]\t%s\n", i+1, d.Confidence, d.Title)
				fmt.Fprintf(w, " \tfix:\t%s\n", wrapText(d.Remediation, " \t      "))
			}
			w.Flush()
			os.Exit(2)
			return nil
		},
	}
	cmd.Flags().StringVarP(&targetsPath, "targets", "t", "targets.yaml", "path to targets YAML")
	return cmd
}

func wrapText(s string, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	lineLen := 0
	for i, word := range words {
		if i > 0 && lineLen+1+len(word) > 80 {
			b.WriteString("\n" + indent)
			lineLen = 0
		} else if i > 0 {
			b.WriteString(" ")
			lineLen++
		}
		b.WriteString(word)
		lineLen += len(word)
	}
	return b.String()
}
