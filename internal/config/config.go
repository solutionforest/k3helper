package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/solutionforest/k3helper/internal/ssh"
	"sigs.k8s.io/yaml"
)

// HostKeyPolicy is the process-wide SSH host key policy, set once from the
// CLI flags. A node's own insecure_host_key still wins for that node.
var HostKeyPolicy = ssh.HostKeyVerify

// Node is a single SSH-reachable machine that hosts (or will host) k3s.
// A node with local: true is the machine k3helper itself runs on; it needs
// no host, user or key because commands are executed directly.
type Node struct {
	Name  string `json:"name"`
	Role  string `json:"role"` // "server" or "agent"
	Host  string `json:"host"`
	Port  int    `json:"port"`
	User  string `json:"user"`
	Key   string `json:"key"` // path to private key
	Local bool   `json:"local"`
	// InsecureHostKey disables SSH host key verification for this node.
	// Intended for throwaway environments whose addresses churn, such as the
	// test sandbox — never for a machine you care about.
	InsecureHostKey bool `json:"insecure_host_key,omitempty"`
}

// SSH converts a targets-file node into connection details. Every caller
// goes through this: hand-copying the fields is how `local` and the host key
// policy each got silently dropped from one call site.
func (n Node) SSH() ssh.Node {
	mode := HostKeyPolicy
	if n.InsecureHostKey {
		mode = ssh.HostKeyInsecure
	}
	return ssh.Node{
		Host: n.Host, Port: n.Port, User: n.User, Key: n.Key,
		Local: n.Local, HostKey: mode,
	}
}

// Targets is one cluster: a name and the machines that make it up.
type Targets struct {
	Cluster string `json:"cluster"`
	Nodes   []Node `json:"nodes"`
}

// File is a targets file. It accepts two shapes:
//
//	cluster: prod          # single cluster, the original format
//	nodes: [...]
//
//	clusters:              # several clusters in one file
//	  - cluster: prod
//	    nodes: [...]
//	  - cluster: staging
//	    nodes: [...]
//	current: prod          # optional; defaults to the first
//
// Both are supported permanently — a one-cluster file should not have to
// grow a list to keep working.
type File struct {
	// single-cluster form
	Cluster string `json:"cluster,omitempty"`
	Nodes   []Node `json:"nodes,omitempty"`
	// multi-cluster form
	Clusters []Targets `json:"clusters,omitempty"`
	Current  string    `json:"current,omitempty"`
}

// Load reads a targets file in either shape and validates every cluster in it.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// The most common first-run failure. Say how to fix it rather
			// than leaving the user to guess the file format.
			return nil, fmt.Errorf(
				"no targets file at %s\n\nCreate one with:\n"+
					"  k3helper init --server <host> --agent <host> --user <user> --key <path>\n"+
					"  k3helper init --local          # this machine, no SSH\n"+
					"  k3helper init                  # a template to edit\n\n"+
					"Or point at an existing file with -t/--targets.", path)
		}
		return nil, fmt.Errorf("read targets: %w", err)
	}
	f := &File{}
	if err := yaml.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("parse targets %s: %w", path, err)
	}
	if len(f.Clusters) > 0 && (f.Cluster != "" || len(f.Nodes) > 0) {
		return nil, fmt.Errorf("invalid targets %s: use either top-level cluster/nodes or a clusters list, not both", path)
	}
	if len(f.Clusters) == 0 {
		f.Clusters = []Targets{{Cluster: f.Cluster, Nodes: f.Nodes}}
		f.Cluster, f.Nodes = "", nil
	}
	seen := map[string]bool{}
	for i := range f.Clusters {
		if err := f.Clusters[i].Validate(); err != nil {
			return nil, fmt.Errorf("invalid targets %s: %w", path, err)
		}
		if seen[f.Clusters[i].Cluster] {
			return nil, fmt.Errorf("invalid targets %s: duplicate cluster %q", path, f.Clusters[i].Cluster)
		}
		seen[f.Clusters[i].Cluster] = true
	}
	if f.Current != "" && !seen[f.Current] {
		return nil, fmt.Errorf("invalid targets %s: current: %q is not one of the defined clusters (%s)",
			path, f.Current, strings.Join(f.Names(), ", "))
	}
	return f, nil
}

// Names lists the cluster names in file order.
func (f *File) Names() []string {
	out := make([]string, 0, len(f.Clusters))
	for _, c := range f.Clusters {
		out = append(out, c.Cluster)
	}
	return out
}

// Select returns the named cluster. An empty name returns `current` when set,
// otherwise the first cluster — so single-cluster files need no ceremony.
func (f *File) Select(name string) (*Targets, error) {
	if name == "" {
		name = f.Current
	}
	if name == "" {
		return &f.Clusters[0], nil
	}
	for i := range f.Clusters {
		if f.Clusters[i].Cluster == name {
			return &f.Clusters[i], nil
		}
	}
	return nil, fmt.Errorf("no cluster %q in targets (have: %s)", name, strings.Join(f.Names(), ", "))
}

// LoadTargets reads a targets file and returns the selected cluster. It is the
// single-cluster entry point every command uses.
func LoadTargets(path string) (*Targets, error) {
	return LoadTargetsContext(path, "")
}

// LoadTargetsContext reads a targets file and returns the named cluster.
func LoadTargetsContext(path, context string) (*Targets, error) {
	f, err := Load(path)
	if err != nil {
		return nil, err
	}
	return f.Select(context)
}

// Validate checks required fields and role values.
func (t *Targets) Validate() error {
	if t.Cluster == "" {
		return fmt.Errorf("cluster name is required")
	}
	if len(t.Nodes) == 0 {
		return fmt.Errorf("at least one node is required")
	}
	seen := map[string]bool{}
	servers := 0
	locals := 0
	for i, n := range t.Nodes {
		if n.Name == "" {
			return fmt.Errorf("node[%d]: name is required", i)
		}
		if seen[n.Name] {
			return fmt.Errorf("node[%d]: duplicate name %q", i, n.Name)
		}
		seen[n.Name] = true
		if n.Local {
			locals++
			// host is never dialled for a local node, but it is printed in
			// check/doctor output, so give it something readable.
			if n.Host == "" {
				t.Nodes[i].Host = "localhost"
			}
		} else {
			if n.Host == "" {
				return fmt.Errorf("node %q: host is required (or set local: true)", n.Name)
			}
			if n.User == "" {
				return fmt.Errorf("node %q: user is required (or set local: true)", n.Name)
			}
		}
		if n.Role != "server" && n.Role != "agent" {
			return fmt.Errorf("node %q: role must be \"server\" or \"agent\", got %q", n.Name, n.Role)
		}
		if n.Role == "server" {
			servers++
		}
		if n.Port == 0 {
			t.Nodes[i].Port = 22
		}
	}
	if servers == 0 {
		return fmt.Errorf("at least one server node is required")
	}
	if locals > 1 {
		return fmt.Errorf("only one node can be local: true (k3helper runs on exactly one machine)")
	}
	return nil
}

// Server returns the first server node.
func (t *Targets) Server() (*Node, error) {
	for i := range t.Nodes {
		if t.Nodes[i].Role == "server" {
			return &t.Nodes[i], nil
		}
	}
	return nil, fmt.Errorf("no server node in targets")
}

// Servers returns every server node, in file order. The first is the one that
// initialises the cluster when several are present.
func (t *Targets) Servers() []Node {
	var out []Node
	for _, n := range t.Nodes {
		if n.Role == "server" {
			out = append(out, n)
		}
	}
	return out
}

// Agent returns all agent nodes.
func (t *Targets) Agents() []Node {
	var out []Node
	for _, n := range t.Nodes {
		if n.Role == "agent" {
			out = append(out, n)
		}
	}
	return out
}
