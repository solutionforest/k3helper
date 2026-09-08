package cli

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/tui"
	"github.com/spf13/cobra"
)

func newTUICmd() *cobra.Command {
	var targetsPath string
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Launch the interactive dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := config.LoadTargets(targetsPath)
			if err != nil {
				return err
			}
			_, err = tui.NewProgram(tui.New(targets))
			return err
		},
	}
	cmd.Flags().StringVarP(&targetsPath, "targets", "t", "targets.yaml", "path to targets YAML")
	return cmd
}

// keep lipgloss referenced for future styling in this package
var _ = lipgloss.NewStyle
