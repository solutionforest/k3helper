package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/solutionforest/k3helper/internal/troubleshoot"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var (
		targetsPath string
		jsonOut     bool
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Troubleshoot: gather evidence across host + k3s + cluster, rank root causes with fixes",
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := loadTargets(targetsPath)
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

			// Every node gets its own connection except the one we already
			// dialled for kubectl. A node we cannot reach is evidence, not
			// something to skip: silently dropping it lets doctor report a
			// healthy cluster while a node is down.
			hosts := map[string]ssh.Executor{}
			var unreachable []troubleshoot.UnreachableNode
			for _, n := range targets.Nodes {
				if n.Name == srvNode.Name {
					hosts[n.Name] = server
					continue
				}
				c, err := ssh.Dial(toSSHNode(n))
				if err != nil {
					unreachable = append(unreachable, troubleshoot.UnreachableNode{Name: n.Name, Reason: err.Error()})
					continue
				}
				defer c.Close()
				hosts[n.Name] = c
			}

			g := troubleshoot.Gatherer{Server: server, Hosts: hosts, Unreachable: unreachable}
			evidence := g.Collect()
			diagnoses := troubleshoot.Diagnose(evidence)

			out := cmd.OutOrStdout()

			// Machine-readable output: the fault-injection suite matches on
			// signature IDs, which are stable, rather than on display titles.
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				report := struct {
					Healthy     bool                           `json:"healthy"`
					Unreachable []troubleshoot.UnreachableNode `json:"unreachable,omitempty"`
					Kubeconfig  string                         `json:"kubeconfig_error,omitempty"`
					ProbeErrors map[string]string              `json:"probe_errors,omitempty"`
					Diagnoses   []troubleshoot.Diagnosis       `json:"diagnoses"`
				}{
					Healthy:     len(diagnoses) == 0,
					Unreachable: evidence.Unreachable,
					Kubeconfig:  evidence.KubeconfigError,
					ProbeErrors: evidence.ProbeErrors,
					Diagnoses:   diagnoses,
				}
				if err := enc.Encode(report); err != nil {
					return err
				}
				for _, d := range diagnoses {
					if d.SignatureID != "cluster.partial-evidence" {
						os.Exit(2)
					}
				}
				return nil
			}

			for _, u := range evidence.Unreachable {
				fmt.Fprintf(out, "✗ node %s unreachable: %s\n", u.Name, u.Reason)
			}
			// Name what could not be gathered: "Some evidence is missing" is
			// no use without saying which.
			if len(evidence.ProbeErrors) > 0 {
				names := make([]string, 0, len(evidence.ProbeErrors))
				for k := range evidence.ProbeErrors {
					names = append(names, k)
				}
				sort.Strings(names)
				for _, k := range names {
					fmt.Fprintf(out, "! could not gather %s: %s\n", k, evidence.ProbeErrors[k])
				}
			}
			w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			if evidence.KubeconfigError != "" {
				fmt.Fprintf(w, "! kubeconfig/cluster access problem:\t%s\n", evidence.KubeconfigError)
			}
			if len(diagnoses) == 0 {
				w.Flush()
				fmt.Fprintln(out, "✓ no issues detected — cluster looks healthy")
				return nil
			}
			fmt.Fprintf(out, "Found %d likely issue(s), ranked by confidence:\n\n", len(diagnoses))
			for i, d := range diagnoses {
				fmt.Fprintf(w, "%d.\t[%d%%]\t%s\n", i+1, d.Confidence, d.Title)
				fmt.Fprintf(w, " \tfix:\t%s\n", wrapText(d.Remediation, " \t      "))
			}
			w.Flush()
			// Incomplete evidence on its own is a caveat, not a fault: exiting
			// non-zero for it alone would turn every RBAC-scoped kubeconfig
			// into a red CI run.
			onlyPartial := true
			for _, d := range diagnoses {
				if d.SignatureID != "cluster.partial-evidence" {
					onlyPartial = false
				}
			}
			if onlyPartial {
				return nil
			}
			os.Exit(2)
			return nil
		},
	}
	cmd.Flags().StringVarP(&targetsPath, "targets", "t", "targets.yaml", "path to targets YAML")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit findings as JSON (signature IDs, for scripts)")
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
