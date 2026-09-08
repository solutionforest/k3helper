package cli

import (
	"fmt"
	"time"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/deploy"
	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/spf13/cobra"
)

func newDeployCmd() *cobra.Command {
	var (
		targetsPath string
		namespace   string
		dryRun      bool
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
			targets, err := config.LoadTargets(targetsPath)
			if err != nil {
				return err
			}
			srv, err := targets.Server()
			if err != nil {
				return err
			}
			server, err := ssh.Dial(toSSHNode(*srv))
			if err != nil {
				return fmt.Errorf("connect to server: %w", err)
			}
			defer server.Close()

			res, err := deploy.Deploy(server, manifest, deploy.Options{
				Namespace:   namespace,
				DryRun:      dryRun,
				WaitTimeout: wait,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
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
	cmd.Flags().DurationVar(&wait, "wait", 120*time.Second, "rollout wait timeout per resource")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}
