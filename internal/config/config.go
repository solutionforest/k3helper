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
	// Registries are the image registries every node in this cluster pulls
	// from. Cluster-level rather than per-node: a mirror configured on some
	// nodes and not others produces pods that run on two machines out of
	// three, which is a miserable thing to debug.
	Registries []Registry `json:"registries,omitempty"`
}

// Registry is a container image registry the nodes pull from — a private
// registry, or a mirror in front of a public one.
type Registry struct {
	// Host is the registry as it appears in an image reference:
	// "docker-registry.example.net", or "docker.io" to mirror the default.
	Host string `json:"host"`
	// Endpoint is the URL to actually contact. Defaults to https://<host>,
	// which is what a registry that is its own endpoint needs; set it to point
	// a well-known name at a mirror.
	Endpoint string `json:"endpoint,omitempty"`

	Username string `json:"username,omitempty"`
	// Password is the credential in the file. PasswordEnv names an
	// environment variable to read it from instead, so a targets file can be
	// committed to a repository without a secret in it.
	Password    string `json:"password,omitempty"`
	PasswordEnv string `json:"password_env,omitempty"`

	// CAFile is a path *on the node* to the CA that signed the registry's
	// certificate — the fix for an internal CA, and the one that keeps TLS
	// verification on.
	CAFile string `json:"ca_file,omitempty"`
	// InsecureSkipVerify turns certificate verification off. It exists
	// because test registries with self-signed certificates exist; it is not
	// what you want in front of a registry that holds your images.
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty"`
}

// URL is the endpoint to contact for this registry.
func (r Registry) URL() string {
	if r.Endpoint != "" {
		return r.Endpoint
	}
	return "https://" + r.Host
}

// HasAuth reports whether credentials were supplied.
func (r Registry) HasAuth() bool { return r.Username != "" || r.Password != "" }

// Resolve fills the password from the environment when password_env is used.
//
// It fails loudly on an unset variable rather than writing an empty
// credential: an anonymous pull against a private registry fails later, from
// a different machine, as "unauthorized", which is a long way from the cause.
func (r Registry) Resolve() (Registry, error) {
	if r.PasswordEnv == "" {
		return r, nil
	}
	v, ok := os.LookupEnv(r.PasswordEnv)
	if !ok || v == "" {
		return r, fmt.Errorf("registry %q: password_env %s is not set in this environment",
			r.Host, r.PasswordEnv)
	}
	r.Password = v
	return r, nil
}

// ResolvedRegistries returns the cluster's registries with password_env
// expanded.
func (t *Targets) ResolvedRegistries() ([]Registry, error) {
	out := make([]Registry, 0, len(t.Registries))
	for _, r := range t.Registries {
		resolved, err := r.Resolve()
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
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
	// Registries at file level apply to every cluster that does not declare
	// its own. A cluster's own list replaces this one rather than merging
	// with it: two lists combined by host is a surprise waiting to happen,
	// and "this cluster pulls from somewhere else" is the reason to override.
	Registries []Registry `json:"registries,omitempty"`
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
		f.Clusters = []Targets{{Cluster: f.Cluster, Nodes: f.Nodes, Registries: f.Registries}}
		f.Cluster, f.Nodes = "", nil
	} else {
		// File-level registries are the default for clusters that declare none.
		for i := range f.Clusters {
			if len(f.Clusters[i].Registries) == 0 {
				f.Clusters[i].Registries = f.Registries
			}
		}
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
	return t.validateRegistries()
}

// validateRegistries rejects registry entries that cannot mean what they say.
func (t *Targets) validateRegistries() error {
	seen := map[string]bool{}
	for i, r := range t.Registries {
		if r.Host == "" {
			return fmt.Errorf("registries[%d]: host is required (the name as it appears in an image reference)", i)
		}
		if seen[r.Host] {
			return fmt.Errorf("registries[%d]: duplicate host %q", i, r.Host)
		}
		seen[r.Host] = true
		if strings.Contains(r.Host, "://") {
			return fmt.Errorf("registry %q: host is a name, not a URL — put the URL in `endpoint`", r.Host)
		}
		if r.Password != "" && r.PasswordEnv != "" {
			return fmt.Errorf("registry %q: set password or password_env, not both", r.Host)
		}
		if r.Username == "" && (r.Password != "" || r.PasswordEnv != "") {
			return fmt.Errorf("registry %q: a password without a username cannot authenticate", r.Host)
		}
		if r.Username != "" && r.Password == "" && r.PasswordEnv == "" {
			return fmt.Errorf("registry %q: username %q has no password (use password, or password_env to keep it out of this file)",
				r.Host, r.Username)
		}
		if r.CAFile != "" && r.InsecureSkipVerify {
			return fmt.Errorf("registry %q: ca_file and insecure_skip_verify contradict each other — a CA is given, so verification can stay on",
				r.Host)
		}
		if r.Endpoint != "" && !strings.Contains(r.Endpoint, "://") {
			return fmt.Errorf("registry %q: endpoint %q needs a scheme (https:// or http://)", r.Host, r.Endpoint)
		}
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
