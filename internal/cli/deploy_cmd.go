package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/solutionforest/k3helper/internal/deploy"
	"github.com/solutionforest/k3helper/internal/transport"
	"github.com/spf13/cobra"
)

func newDeployCmd() *cobra.Command {
	var (
		targetsPath string
		namespace   string
		dryRun      bool
		showDiff    bool
		wait        time.Duration
	)
	cmd := &cobra.Command{
		Use:   "deploy -f <manifest>",
		Short: "Quick deploy: verify (server dry-run) then apply then wait for rollout",
		RunE: func(cmd *cobra.Command, args []string) error {
			manifest, _ := cmd.Flags().GetString("file")
			if manifest == "" {
				return fmt.Errorf("--file is required")
			}
			targets, err := loadTargets(targetsPath)
			if err != nil {
				return err
			}
			server, err := transport.Server(targets)
			if err != nil {
				return err
			}
			defer server.Close()

			res, err := deploy.Deploy(server, manifest, deploy.Options{
				Namespace:   namespace,
				DryRun:      dryRun,
				WaitTimeout: wait,
				Diff:        showDiff,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if showDiff {
				if res.Diff == "" {
					fmt.Fprintln(out, "= no changes against live cluster state")
				} else {
					fmt.Fprintln(out, renderDiff(res.Diff))
				}
			}
			for _, a := range res.Applied {
				fmt.Fprintf(out, "✓ applied %s\n", a)
			}
			for _, r := range res.RolledOut {
				fmt.Fprintf(out, "✓ rolled out %s\n", r)
			}
			for _, e := range res.Errors {
				fmt.Fprintf(out, "✗ %s\n", e)
			}
			if dryRun {
				fmt.Fprintln(out, "(dry-run only, nothing applied)")
			}
			if len(res.Errors) > 0 {
				return fmt.Errorf("deploy completed with %d error(s)", len(res.Errors))
			}
			return nil
		},
	}
	cmd.Flags().StringP("file", "f", "", "manifest file to deploy")
	cmd.Flags().StringVarP(&targetsPath, "targets", "t", "targets.yaml", "path to targets YAML")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "target namespace override")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate only (server dry-run)")
	cmd.Flags().BoolVar(&showDiff, "diff", false, "show a diff against live cluster state before applying")
	cmd.Flags().DurationVar(&wait, "wait", 120*time.Second, "rollout wait timeout per resource")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

// renderDiff colours a unified diff so additions and removals are legible at
// a glance. Context lines are left alone.
func renderDiff(diff string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			b.WriteString(diffHeaderStyle.Render(line))
		case strings.HasPrefix(line, "+"):
			b.WriteString(diffAddStyle.Render(line))
		case strings.HasPrefix(line, "-"):
			b.WriteString(diffDelStyle.Render(line))
		default:
			b.WriteString(line)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

var (
	diffAddStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	diffDelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	diffHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
)
