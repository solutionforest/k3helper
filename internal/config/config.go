package config

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Node is a single SSH-reachable machine that hosts (or will host) k3s.
type Node struct {
	Name string `json:"name"`
	Role string `json:"role"` // "server" or "agent"
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	Key  string `json:"key"` // path to private key
}

// Targets is the top-level targets file describing a cluster.
type Targets struct {
	Cluster string `json:"cluster"`
	Nodes   []Node `json:"nodes"`
}

// LoadTargets reads and validates a targets YAML file.
func LoadTargets(path string) (*Targets, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read targets: %w", err)
	}
	t := &Targets{}
	if err := yaml.Unmarshal(data, t); err != nil {
		return nil, fmt.Errorf("parse targets %s: %w", path, err)
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("invalid targets %s: %w", path, err)
	}
	return t, nil
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
	for i, n := range t.Nodes {
		if n.Name == "" {
			return fmt.Errorf("node[%d]: name is required", i)
		}
		if seen[n.Name] {
			return fmt.Errorf("node[%d]: duplicate name %q", i, n.Name)
		}
		seen[n.Name] = true
		if n.Host == "" {
			return fmt.Errorf("node %q: host is required", n.Name)
		}
		if n.User == "" {
			return fmt.Errorf("node %q: user is required", n.Name)
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
