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

func TestLoadTargetsLocalNode(t *testing.T) {
	// The browser-console layout: k3helper runs on the server itself and only
	// SSHes outward to the agents.
	path := writeTemp(t, `
cluster: prod
nodes:
  - name: server
    role: server
    local: true
  - name: agent1
    role: agent
    host: 10.0.0.11
    user: root
    key: /root/.ssh/k3helper
`)
	targets, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	srv, err := targets.Server()
	if err != nil {
		t.Fatalf("Server(): %v", err)
	}
	if !srv.Local {
		t.Error("server node lost local: true")
	}
	if srv.Host != "localhost" {
		t.Errorf("host = %q, want localhost default for display", srv.Host)
	}
	if targets.Agents()[0].Local {
		t.Error("agent should not be local")
	}
}

func TestLoadTargetsRejectsTwoLocalNodes(t *testing.T) {
	path := writeTemp(t, `
cluster: c
nodes:
  - name: server
    role: server
    local: true
  - name: agent1
    role: agent
    local: true
`)
	_, err := LoadTargets(path)
	if err == nil {
		t.Fatal("expected error for two local nodes")
	}
	if !contains(err.Error(), "only one node can be local") {
		t.Errorf("error = %q", err.Error())
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

// --- multi-cluster targets files ---

// The original single-cluster format must keep working unchanged.
func TestLoadSingleClusterFormatStillWorks(t *testing.T) {
	path := writeTemp(t, `
cluster: solo
nodes:
  - name: n1
    role: server
    host: 10.0.0.1
    user: ubuntu
`)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Clusters) != 1 || f.Clusters[0].Cluster != "solo" {
		t.Fatalf("clusters = %+v, want one named solo", f.Clusters)
	}
	targets, err := LoadTargets(path)
	if err != nil || targets.Cluster != "solo" {
		t.Errorf("LoadTargets = %+v, %v", targets, err)
	}
}

func TestLoadMultiClusterFormat(t *testing.T) {
	path := writeTemp(t, `
clusters:
  - cluster: prod
    nodes:
      - name: p1
        role: server
        host: 10.0.0.1
        user: ubuntu
  - cluster: staging
    nodes:
      - name: s1
        role: server
        host: 10.0.1.1
        user: ubuntu
current: staging
`)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := f.Names(); len(got) != 2 || got[0] != "prod" || got[1] != "staging" {
		t.Errorf("Names() = %v, want [prod staging] in file order", got)
	}

	// no explicit context -> `current`
	sel, err := f.Select("")
	if err != nil || sel.Cluster != "staging" {
		t.Errorf("Select(\"\") = %v, %v; want the `current` cluster", sel, err)
	}
	// explicit context wins over `current`
	sel, err = f.Select("prod")
	if err != nil || sel.Cluster != "prod" {
		t.Errorf("Select(prod) = %v, %v", sel, err)
	}
	if sel.Nodes[0].Host != "10.0.0.1" {
		t.Errorf("selected the wrong cluster's nodes: %+v", sel.Nodes)
	}
}

// Without `current`, the first cluster is used so the flag stays optional.
func TestSelectDefaultsToFirstCluster(t *testing.T) {
	path := writeTemp(t, `
clusters:
  - cluster: a
    nodes: [{name: n, role: server, host: h, user: u}]
  - cluster: b
    nodes: [{name: n, role: server, host: h, user: u}]
`)
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	sel, err := f.Select("")
	if err != nil || sel.Cluster != "a" {
		t.Errorf("Select(\"\") = %v, %v; want the first cluster", sel, err)
	}
}

func TestSelectUnknownContextListsChoices(t *testing.T) {
	path := writeTemp(t, `
clusters:
  - cluster: prod
    nodes: [{name: n, role: server, host: h, user: u}]
  - cluster: staging
    nodes: [{name: n, role: server, host: h, user: u}]
`)
	f, _ := Load(path)
	_, err := f.Select("nope")
	if err == nil {
		t.Fatal("expected an error for an unknown context")
	}
	// The message must tell the operator what they can pick.
	for _, want := range []string{"nope", "prod", "staging"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestMultiClusterFileErrors(t *testing.T) {
	cases := []struct {
		name, content, wantErr string
	}{
		{
			"both formats at once",
			"cluster: a\nnodes: [{name: n, role: server, host: h, user: u}]\nclusters:\n  - cluster: b\n    nodes: [{name: n, role: server, host: h, user: u}]\n",
			"not both",
		},
		{
			"duplicate cluster names",
			"clusters:\n  - cluster: a\n    nodes: [{name: n, role: server, host: h, user: u}]\n  - cluster: a\n    nodes: [{name: n, role: server, host: h, user: u}]\n",
			"duplicate cluster",
		},
		{
			"current names a cluster that is not defined",
			"clusters:\n  - cluster: a\n    nodes: [{name: n, role: server, host: h, user: u}]\ncurrent: ghost\n",
			"not one of the defined clusters",
		},
		{
			"a cluster in the list is itself invalid",
			"clusters:\n  - cluster: a\n    nodes: [{name: n, role: worker, host: h, user: u}]\n",
			"role must be",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tc.content))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadTargetsContextSelects(t *testing.T) {
	path := writeTemp(t, `
clusters:
  - cluster: prod
    nodes: [{name: p, role: server, host: 10.0.0.1, user: u}]
  - cluster: staging
    nodes: [{name: s, role: server, host: 10.0.1.1, user: u}]
`)
	targets, err := LoadTargetsContext(path, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if targets.Cluster != "staging" || targets.Nodes[0].Host != "10.0.1.1" {
		t.Errorf("got %+v, want the staging cluster", targets)
	}
}
