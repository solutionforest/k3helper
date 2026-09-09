package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/solutionforest/k3helper/internal/kyaml"
	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/spf13/cobra"
)

func newVerifyCmd() *cobra.Command {
	var (
		jsonOut     bool
		serverDry   bool
		targetsPath string
	)
	cmd := &cobra.Command{
		Use:   "verify <file...>",
		Short: "Validate Kubernetes YAML manifests (syntax + structure, optionally against a live API server)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// --dry-run=server adds validation layer 3: submit each manifest to
			// the real API server, which catches unknown fields, admission
			// rejections and CRD schemas that offline rules cannot.
			var server *ssh.Client
			if serverDry {
				targets, err := loadTargets(targetsPath)
				if err != nil {
					return err
				}
				srvNode, err := targets.Server()
				if err != nil {
					return err
				}
				server, err = ssh.Dial(toSSHNode(*srvNode))
				if err != nil {
					return fmt.Errorf("connect to server: %w", err)
				}
				defer server.Close()
			}

			anyFailed := false
			for _, path := range args {
				data, err := os.ReadFile(path)
				if err != nil {
					return fmt.Errorf("read %s: %w", path, err)
				}
				var res *kyaml.Result
				if server != nil {
					res, err = kyaml.VerifyLive(server, data, path)
					if err != nil {
						return fmt.Errorf("verify %s against server: %w", path, err)
					}
				} else {
					res = kyaml.Verify(data)
				}
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
	cmd.Flags().BoolVar(&serverDry, "dry-run-server", false, "also validate against the live API server (kubectl apply --dry-run=server)")
	cmd.Flags().StringVarP(&targetsPath, "targets", "t", "targets.yaml", "path to targets YAML (used with --dry-run-server)")
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
			// Line 0 means "no line to point at" (server-reported issues).
			loc := "  "
			if issue.Line > 0 {
				loc = fmt.Sprintf("line %d", issue.Line)
			}
			fmt.Fprintf(w, "  \t%s%s\t%s\n", loc, field, issue.Message)
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

		pullSecret   string
		registryHost string
		registryUser string
		registryPass string
		registryMail string
	)
	cmd := &cobra.Command{
		Use:   "gen <kind> <name>",
		Short: "Generate correct Kubernetes YAML (deployment, service, ingress, ...)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := kyaml.Generate(kyaml.GenParams{
				Kind:             args[0],
				Name:             args[1],
				Namespace:        namespace,
				Image:            image,
				Replicas:         replicas,
				Port:             port,
				ImagePullSecret:  pullSecret,
				Registry:         registryHost,
				RegistryUser:     registryUser,
				RegistryPassword: registryPass,
				RegistryEmail:    registryMail,
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
	cmd.Flags().StringVar(&pullSecret, "image-pull-secret", "",
		"name of a pull secret to reference from the generated pod spec")
	cmd.Flags().StringVar(&registryHost, "docker-registry", "",
		"generate a registry pull secret for this host (with `gen secret <name>`)")
	cmd.Flags().StringVar(&registryUser, "registry-user", "", "username for --docker-registry")
	cmd.Flags().StringVar(&registryPass, "registry-password", "", "password for --docker-registry")
	cmd.Flags().StringVar(&registryMail, "registry-email", "", "email for --docker-registry (optional; some registries want one)")
	return cmd
}
