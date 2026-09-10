package cli

import (
	"fmt"
	"runtime"

	"github.com/solutionforest/k3helper/internal/bundle"
	"github.com/spf13/cobra"
)

func newBundleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Assemble what an air-gapped node needs to install Kubernetes",
		Long: `Build an offline install bundle on a machine that has internet, so nodes
that do not have any can still be built.

The usual install reaches out three times: to update.k3s.io to turn "stable"
into a version, to the GitHub release for the k3s binary, and to a registry for
every image a pod pulls. A bundle covers all three — the binary, the airgap
image archive k3s imports into containerd on first start, and the installer
itself.

  k3helper bundle k3s --version v1.31.2+k3s1 -o ./k3s-bundle
  k3helper vm setup -t targets.yaml --bundle ./k3s-bundle`,
	}
	cmd.AddCommand(newBundleK3sCmd())
	return cmd
}

func newBundleK3sCmd() *cobra.Command {
	var (
		version string
		arch    string
		out     string
	)
	cmd := &cobra.Command{
		Use:   "k3s",
		Short: "Download a k3s release and its airgap images into a directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			if version == "" {
				return fmt.Errorf("--version is required (e.g. v1.31.2+k3s1)\n\n" +
					"A bundle cannot resolve a channel — not needing update.k3s.io is the point of it.\n" +
					"Releases are listed at https://github.com/k3s-io/k3s/releases")
			}
			m, err := bundle.Fetch(bundle.Options{
				Version:  version,
				Arch:     arch,
				Dir:      out,
				Progress: cmd.OutOrStdout(),
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"\nNext, from a machine that can reach the nodes:\n"+
					"  k3helper vm setup -t targets.yaml --bundle %s\n", out)
			_ = m
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "k3s release to fetch, e.g. v1.31.2+k3s1")
	// The nodes' architecture, which is usually but not always this machine's:
	// bundles get built on a laptop for servers that are not one.
	cmd.Flags().StringVar(&arch, "arch", runtime.GOARCH, "architecture of the target nodes (amd64 or arm64)")
	cmd.Flags().StringVarP(&out, "out", "o", "k3s-bundle", "directory to write the bundle into")
	return cmd
}
