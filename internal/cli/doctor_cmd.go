package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/solutionforest/k3helper/internal/troubleshoot"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var (
		targetsPath string
		jsonOut     bool
		watch       time.Duration
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Troubleshoot: gather evidence across host + k3s + cluster, rank root causes with fixes",
		RunE: func(cmd *cobra.Command, args []string) error {
			if watch > 0 {
				if jsonOut {
					return fmt.Errorf("--watch and --json cannot be combined; --json is a single snapshot for scripts")
				}
				return watchLoop(cmd, targetsPath, watch)
			}
			res, err := diagnoseOnce(targetsPath)
			if err != nil {
				return err
			}
			evidence, diagnoses := res.evidence, res.diagnoses

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
	cmd.Flags().DurationVar(&watch, "watch", 0, "re-run continuously at this interval (e.g. 30s); Ctrl-C to stop")
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

// watchLoop re-runs the diagnosis on an interval until interrupted.
//
// It reconnects every pass rather than holding connections open: a node going
// away is one of the things being watched for, and a stale connection would
// report the last state it saw instead.
func watchLoop(cmd *cobra.Command, targetsPath string, every time.Duration) error {
	out := cmd.OutOrStdout()
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lastSummary string
	for pass := 1; ; pass++ {
		findings, err := diagnoseOnce(targetsPath)
		stamp := time.Now().Format("15:04:05")

		summary := "healthy"
		if err != nil {
			summary = "error: " + err.Error()
		} else if len(findings.diagnoses) > 0 {
			ids := make([]string, 0, len(findings.diagnoses))
			for _, d := range findings.diagnoses {
				ids = append(ids, d.SignatureID)
			}
			summary = strings.Join(ids, ",")
		}

		// Only redraw in full when something changed; otherwise tick quietly,
		// so a long watch does not bury the moment things went wrong.
		if summary != lastSummary {
			fmt.Fprintf(out, "\n──── %s ─────────────────────────────────\n", stamp)
			switch {
			case err != nil:
				fmt.Fprintf(out, "%s\n", errStyleWatch(err.Error()))
			case len(findings.diagnoses) == 0:
				for _, u := range findings.unreachable {
					fmt.Fprintf(out, "✗ node %s unreachable: %s\n", u.Name, u.Reason)
				}
				fmt.Fprintln(out, "✓ no issues detected — cluster looks healthy")
			default:
				printDiagnoses(out, findings)
			}
			lastSummary = summary
		} else {
			fmt.Fprintf(out, "%s  unchanged (%s)\n", stamp, summary)
		}

		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "\nstopped.")
			return nil
		case <-time.After(every):
		}
		_ = pass
	}
}

func errStyleWatch(msg string) string { return "! " + msg }

// diagnosis is one complete pass: the evidence gathered and what it means.
type diagnosis struct {
	evidence    troubleshoot.Evidence
	diagnoses   []troubleshoot.Diagnosis
	unreachable []troubleshoot.UnreachableNode
}

// diagnoseOnce connects, gathers and ranks. Connections are opened and closed
// per pass so `--watch` observes the cluster as it is now, not as it was when
// the first connection succeeded.
func diagnoseOnce(targetsPath string) (diagnosis, error) {
	targets, err := loadTargets(targetsPath)
	if err != nil {
		return diagnosis{}, err
	}
	srvNode, err := targets.Server()
	if err != nil {
		return diagnosis{}, err
	}
	server, err := ssh.Dial(srvNode.SSH())
	if err != nil {
		return diagnosis{}, fmt.Errorf("connect to server: %w", err)
	}
	defer server.Close()

	// Every node gets its own connection except the one already dialled for
	// kubectl. A node we cannot reach is evidence, not something to skip:
	// silently dropping it lets doctor report a healthy cluster while a node
	// is down.
	hosts := map[string]ssh.Executor{}
	var unreachable []troubleshoot.UnreachableNode
	for _, n := range targets.Nodes {
		if n.Name == srvNode.Name {
			hosts[n.Name] = server
			continue
		}
		c, err := ssh.Dial(n.SSH())
		if err != nil {
			unreachable = append(unreachable, troubleshoot.UnreachableNode{Name: n.Name, Reason: err.Error()})
			continue
		}
		defer c.Close()
		hosts[n.Name] = c
	}

	var serverNames []string
	for _, n := range targets.Servers() {
		serverNames = append(serverNames, n.Name)
	}
	g := troubleshoot.Gatherer{
		Server: server, Hosts: hosts, Unreachable: unreachable, ServerNodes: serverNames,
	}
	evidence := g.Collect()
	return diagnosis{
		evidence:    evidence,
		diagnoses:   troubleshoot.Diagnose(evidence),
		unreachable: unreachable,
	}, nil
}

// printDiagnoses renders a ranked findings list.
func printDiagnoses(out io.Writer, d diagnosis) {
	for _, u := range d.unreachable {
		fmt.Fprintf(out, "✗ node %s unreachable: %s\n", u.Name, u.Reason)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(out, "Found %d likely issue(s), ranked by confidence:\n\n", len(d.diagnoses))
	for i, x := range d.diagnoses {
		fmt.Fprintf(w, "%d.\t[%d%%]\t%s\n", i+1, x.Confidence, x.Title)
		fmt.Fprintf(w, " \tfix:\t%s\n", wrapText(x.Remediation, " \t      "))
	}
	w.Flush()
}
