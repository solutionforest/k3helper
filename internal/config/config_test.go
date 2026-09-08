package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "targets.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadTargetsValid(t *testing.T) {
	path := writeTemp(t, `
cluster: sandbox
nodes:
  - name: server
    role: server
    host: 127.0.0.1
    port: 2221
    user: sandbox
    key: /tmp/key
  - name: agent1
    role: agent
    host: 127.0.0.1
    port: 2222
    user: sandbox
    key: /tmp/key
`)
	targets, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if targets.Cluster != "sandbox" {
		t.Errorf("cluster = %q, want sandbox", targets.Cluster)
	}
	if len(targets.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(targets.Nodes))
	}
	srv, err := targets.Server()
	if err != nil || srv.Name != "server" {
		t.Errorf("Server() = %v, %v", srv, err)
	}
	if len(targets.Agents()) != 1 {
		t.Errorf("Agents() = %d, want 1", len(targets.Agents()))
	}
}

func TestLoadTargetsDefaultPort(t *testing.T) {
	path := writeTemp(t, `
cluster: c
nodes:
  - name: n1
    role: server
    host: 10.0.0.1
    user: ubuntu
`)
	targets, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if targets.Nodes[0].Port != 22 {
		t.Errorf("port = %d, want default 22", targets.Nodes[0].Port)
	}
}

func TestLoadTargetsErrors(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"no cluster", "nodes:\n  - name: n\n    role: server\n    host: h\n    user: u\n", "cluster name is required"},
		{"no nodes", "cluster: c\nnodes: []\n", "at least one node"},
		{"no server", "cluster: c\nnodes:\n  - name: n\n    role: agent\n    host: h\n    user: u\n", "at least one server"},
		{"bad role", "cluster: c\nnodes:\n  - name: n\n    role: worker\n    host: h\n    user: u\n", "role must be"},
		{"missing host", "cluster: c\nnodes:\n  - name: n\n    role: server\n    user: u\n", "host is required"},
		{"missing user", "cluster: c\nnodes:\n  - name: n\n    role: server\n    host: h\n", "user is required"},
		{"dup name", "cluster: c\nnodes:\n  - name: n\n    role: server\n    host: h\n    user: u\n  - name: n\n    role: agent\n    host: h2\n    user: u\n", "duplicate name"},
		{"bad yaml", "cluster: [\n", "parse targets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, tc.content)
			_, err := LoadTargets(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadTargetsMissingFile(t *testing.T) {
	_, err := LoadTargets("/nonexistent/targets.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
