package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/solutionforest/k3helper/internal/kyaml"
	"github.com/spf13/cobra"
)

func newVerifyCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "verify <file...>",
		Short: "Validate Kubernetes YAML manifests (syntax + structure)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			anyFailed := false
			for _, path := range args {
				data, err := os.ReadFile(path)
				if err != nil {
					return fmt.Errorf("read %s: %w", path, err)
				}
				res := kyaml.Verify(data)
				if jsonOut {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					if err := enc.Encode(res); err != nil {
						return err
					}
				} else {
					printVerifyResult(cmd, path, res)
				}
				if !res.OK {
					anyFailed = true
				}
			}
			if anyFailed {
				return fmt.Errorf("validation failed")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

func printVerifyResult(cmd *cobra.Command, path string, res *kyaml.Result) {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	if res.OK {
		fmt.Fprintf(w, "✓ %s", path)
		for _, d := range res.Documents {
			fmt.Fprintf(w, "\t%s/%s (%s)\n", d.Kind, d.Name, d.APIVer)
		}
		if len(res.Documents) == 0 {
			fmt.Fprintln(w)
		}
	} else {
		fmt.Fprintf(w, "✗ %s\n", path)
		for _, issue := range res.Issues {
			field := issue.Field
			if field != "" {
				field = " [" + field + "]"
			}
			fmt.Fprintf(w, "  \tline %d%s\t%s\n", issue.Line, field, issue.Message)
		}
	}
	w.Flush()
}

func newGenCmd() *cobra.Command {
	var (
		namespace string
		image     string
		replicas  int
		port      int
		outPath   string
	)
	cmd := &cobra.Command{
		Use:   "gen <kind> <name>",
		Short: "Generate correct Kubernetes YAML (deployment, service, ingress, ...)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := kyaml.Generate(kyaml.GenParams{
				Kind:      args[0],
				Name:      args[1],
				Namespace: namespace,
				Image:     image,
				Replicas:  replicas,
				Port:      port,
			})
			if err != nil {
				return err
			}
			// self-check: generated output must always verify
			if res := kyaml.Verify([]byte(out)); !res.OK {
				return fmt.Errorf("internal error: generated manifest failed verification: %+v", res.Issues)
			}
			if outPath != "" {
				return os.WriteFile(outPath, []byte(out), 0o644)
			}
			_, err = cmd.OutOrStdout().Write([]byte(out))
			return err
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "target namespace")
	cmd.Flags().StringVarP(&image, "image", "i", "", "container image")
	cmd.Flags().IntVarP(&replicas, "replicas", "r", 1, "replica count")
	cmd.Flags().IntVarP(&port, "port", "p", 80, "port")
	cmd.Flags().StringVarP(&outPath, "out", "o", "", "write to file instead of stdout")
	return cmd
}
