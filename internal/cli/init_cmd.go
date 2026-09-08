package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

func newInitCmd() *cobra.Command {
	var (
		outPath  string
		cluster  string
		server   string
		agents   []string
		user     string
		key      string
		local    bool
		force    bool
		insecure bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a targets.yaml describing your nodes",
		Long: `Create a targets file describing the machines k3helper will manage.

Examples:
  # three nodes over SSH
  k3helper init --server 10.0.0.10 --agent 10.0.0.11 --agent 10.0.0.12 \
      --user ubuntu --key ~/.ssh/id_ed25519

  # this machine, no SSH (provider browser console)
  k3helper init --local

  # a template to fill in by hand
  k3helper init`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := os.Stat(outPath); err == nil && !force {
				return fmt.Errorf("%s already exists; pass --force to overwrite it", outPath)
			}

			targets, err := buildTargets(cluster, server, agents, user, key, local, insecure)
			if err != nil {
				return err
			}
			// Render before validating: Validate fills in defaults (it sets a
			// local node's host to "localhost"), and echoing those back into
			// the file would clutter it with values the user never chose.
			body := initHeader(local) + renderTargets(targets)

			// A targets file that cannot be loaded back is worse than none, so
			// validate a copy and then re-parse what we are about to write.
			check := *targets
			check.Nodes = append([]config.Node(nil), targets.Nodes...)
			if err := check.Validate(); err != nil {
				return fmt.Errorf("generated targets are invalid: %w", err)
			}
			var roundTrip config.File
			if err := yaml.Unmarshal([]byte(body), &roundTrip); err != nil {
				return fmt.Errorf("generated targets do not parse: %w", err)
			}

			if err := os.WriteFile(outPath, []byte(body), 0o600); err != nil {
				return fmt.Errorf("write %s: %w", outPath, err)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "✓ wrote %s\n\n", outPath)
			for _, n := range targets.Nodes {
				where := n.Host
				if n.Local {
					where = "this machine (local)"
				}
				fmt.Fprintf(out, "  %-8s %-6s %s\n", n.Name, n.Role, where)
			}
			fmt.Fprintf(out, "\nNext: k3helper check -t %s\n", outPath)
			return nil
		},
	}
	cmd.Flags().StringVarP(&outPath, "out", "o", "targets.yaml", "path to write")
	cmd.Flags().StringVar(&cluster, "cluster", "my-cluster", "cluster name")
	cmd.Flags().StringVar(&server, "server", "", "server node address (host or host:port)")
	cmd.Flags().StringArrayVar(&agents, "agent", nil, "agent node address; repeat for more")
	cmd.Flags().StringVarP(&user, "user", "u", "root", "SSH user for every node")
	cmd.Flags().StringVarP(&key, "key", "k", "~/.ssh/id_ed25519", "SSH private key for every node")
	cmd.Flags().BoolVar(&local, "local", false, "single node: the machine k3helper runs on, no SSH")
	cmd.Flags().BoolVar(&insecure, "insecure-host-key", false, "skip SSH host key verification for these nodes")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	return cmd
}

// buildTargets assembles the cluster from the flags, falling back to a
// placeholder template when no addresses were given.
func buildTargets(cluster, server string, agents []string, user, key string, local, insecure bool) (*config.Targets, error) {
	t := &config.Targets{Cluster: cluster}

	if local {
		if server != "" || len(agents) > 0 {
			return nil, fmt.Errorf("--local describes a single node; do not combine it with --server/--agent")
		}
		t.Nodes = []config.Node{{Name: "server", Role: "server", Local: true}}
		return t, nil
	}

	if server == "" && len(agents) == 0 {
		// No addresses: emit a template with obvious placeholders rather than
		// failing, so `k3helper init` alone still gets you something to edit.
		server = "10.0.0.10"
		agents = []string{"10.0.0.11"}
	}
	if server == "" {
		return nil, fmt.Errorf("--server is required when agents are given (a cluster needs a server node)")
	}

	host, port, err := splitHostPort(server)
	if err != nil {
		return nil, err
	}
	t.Nodes = append(t.Nodes, config.Node{
		Name: "server", Role: "server", Host: host, Port: port,
		User: user, Key: key, InsecureHostKey: insecure,
	})
	for i, a := range agents {
		host, port, err := splitHostPort(a)
		if err != nil {
			return nil, err
		}
		t.Nodes = append(t.Nodes, config.Node{
			Name: fmt.Sprintf("agent%d", i+1), Role: "agent", Host: host, Port: port,
			User: user, Key: key, InsecureHostKey: insecure,
		})
	}
	return t, nil
}

// splitHostPort accepts "host" or "host:port"; IPv6 must be bracketed.
func splitHostPort(s string) (string, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0, fmt.Errorf("empty node address")
	}
	if strings.HasPrefix(s, "[") {
		end := strings.LastIndex(s, "]")
		if end < 0 {
			return "", 0, fmt.Errorf("invalid address %q: missing closing bracket", s)
		}
		host := s[1:end]
		rest := s[end+1:]
		if rest == "" {
			return host, 22, nil
		}
		port, err := strconv.Atoi(strings.TrimPrefix(rest, ":"))
		if err != nil {
			return "", 0, fmt.Errorf("invalid port in %q", s)
		}
		return host, port, nil
	}
	// A bare IPv6 address has several colons and no port.
	if strings.Count(s, ":") > 1 {
		return s, 22, nil
	}
	host, portStr, found := strings.Cut(s, ":")
	if !found {
		return s, 22, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port in %q", s)
	}
	return host, port, nil
}

// renderTargets writes the YAML by hand. Marshalling would sort the keys
// alphabetically (host before name before role), which reads badly in a file
// meant to be edited, and would emit empty values for fields left unset.
func renderTargets(t *config.Targets) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cluster: %s\n", t.Cluster)
	b.WriteString("nodes:\n")
	for _, n := range t.Nodes {
		fmt.Fprintf(&b, "  - name: %s\n", n.Name)
		fmt.Fprintf(&b, "    role: %s\n", n.Role)
		if n.Local {
			b.WriteString("    local: true\n")
			continue
		}
		fmt.Fprintf(&b, "    host: %s\n", n.Host)
		if n.Port != 0 && n.Port != 22 {
			fmt.Fprintf(&b, "    port: %d\n", n.Port)
		}
		fmt.Fprintf(&b, "    user: %s\n", n.User)
		fmt.Fprintf(&b, "    key: %s\n", n.Key)
		if n.InsecureHostKey {
			b.WriteString("    insecure_host_key: true\n")
		}
	}
	return b.String()
}

func initHeader(local bool) string {
	h := `# k3helper targets — the machines this cluster runs on.
#
#   role:  server | agent            (at least one server)
#   port:  defaults to 22
#   key:   path to an SSH private key; ~ is expanded
#
# Several clusters can live in one file:
#   clusters:
#     - cluster: prod
#       nodes: [...]
#     - cluster: staging
#       nodes: [...]
#   current: prod
# then select one with --context <name>.
`
	if local {
		h += `#
# local: true means "the machine k3helper is running on" — commands run
# through /bin/sh instead of SSH, so no host, user or key is needed.
`
	}
	return h
}
