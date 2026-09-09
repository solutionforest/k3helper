package cli

import (
	"fmt"
	"strings"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/tui"
	"github.com/spf13/cobra"
)

func newTUICmd() *cobra.Command {
	var (
		targetsPath string
		theme       string
	)
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Launch the interactive dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := loadTargets(targetsPath)
			if err != nil {
				return err
			}
			if err := tui.SetTheme(theme); err != nil {
				return err
			}
			m := tui.New(targets)
			// The whole file (not just the selected cluster) backs `:ctx`, so
			// the operator can switch clusters without leaving the TUI. A file
			// that fails to reload is not fatal: the dashboard still works for
			// the cluster already loaded, minus the switcher.
			if f, err := config.Load(targetsPath); err == nil {
				m = m.WithFile(f, targetsPath)
			}
			_, err = tui.NewProgram(m)
			return err
		},
	}
	cmd.Flags().StringVarP(&targetsPath, "targets", "t", "targets.yaml", "path to targets YAML")
	cmd.Flags().StringVar(&theme, "theme", "",
		fmt.Sprintf("skin: %s, or a path to a skin YAML file", strings.Join(tui.ThemeNames(), " | ")))
	return cmd
}
