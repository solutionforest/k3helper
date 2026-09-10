package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKubeconfig creates a stand-in kubeconfig. Nothing parses it — the
// transport hands the path to kubectl — so its contents only have to exist.
func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func writeTargets(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "targets.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write targets: %v", err)
	}
	return path
}

func TestModeFollowsKubeconfig(t *testing.T) {
	ssh := &Targets{Cluster: "a", Nodes: []Node{{Name: "s", Role: "server", Host: "h", User: "u"}}}
	if got := ssh.Mode(); got != ModeSSH {
		t.Errorf("nodes-only cluster: mode = %v, want ModeSSH", got)
	}
	kc := &Targets{Cluster: "a", Kubeconfig: "/k/c.yaml"}
	if got := kc.Mode(); got != ModeKubeconfig {
		t.Errorf("kubeconfig cluster: mode = %v, want ModeKubeconfig", got)
	}
}

func TestLoadKubeconfigCluster(t *testing.T) {
	kube := writeKubeconfig(t)
	path := writeTargets(t, "cluster: prod\nkubeconfig: "+kube+"\nkube_context: admin\n")

	got, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if got.Mode() != ModeKubeconfig {
		t.Errorf("mode = %v, want ModeKubeconfig", got.Mode())
	}
	if got.KubeContext != "admin" {
		t.Errorf("kube_context = %q, want admin", got.KubeContext)
	}
	if len(got.Nodes) != 0 {
		t.Errorf("got %d nodes, want none", len(got.Nodes))
	}
}

// Both together leave every command guessing which door to use — including
// the ones that write files on hosts.
func TestKubeconfigAndNodesTogetherRejected(t *testing.T) {
	kube := writeKubeconfig(t)
	path := writeTargets(t, "cluster: prod\nkubeconfig: "+kube+"\nnodes:\n  - name: s\n    role: server\n    host: 10.0.0.1\n    user: root\n")

	_, err := LoadTargets(path)
	if err == nil {
		t.Fatal("a file with both nodes and kubeconfig was accepted")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error %q does not explain the conflict", err)
	}
}

func TestKubeconfigMustExist(t *testing.T) {
	path := writeTargets(t, "cluster: prod\nkubeconfig: /nowhere/kube.yaml\n")
	_, err := LoadTargets(path)
	if err == nil {
		t.Fatal("a missing kubeconfig was accepted")
	}
	if !strings.Contains(err.Error(), "no kubeconfig at") {
		t.Errorf("error %q does not name the missing file", err)
	}
}

func TestKubeconfigDirectoryRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTargets(t, "cluster: prod\nkubeconfig: "+dir+"\n")
	if _, err := LoadTargets(path); err == nil {
		t.Fatal("a directory was accepted as a kubeconfig")
	}
}

// Registry configuration is written on the nodes. A kubeconfig cluster has
// none, so accepting the block would silently do nothing.
func TestRegistriesRejectedOnKubeconfigCluster(t *testing.T) {
	kube := writeKubeconfig(t)
	path := writeTargets(t, "cluster: prod\nkubeconfig: "+kube+"\nregistries:\n  - host: reg.example.net\n")
	_, err := LoadTargets(path)
	if err == nil {
		t.Fatal("registries were accepted on a cluster with no nodes to write them to")
	}
	if !strings.Contains(err.Error(), "SSH") {
		t.Errorf("error %q does not say why", err)
	}
}

// File-level registries are a default for SSH clusters. Inheriting them into a
// kubeconfig cluster would turn a valid mixed file into a validation error.
func TestFileLevelRegistriesSkipKubeconfigClusters(t *testing.T) {
	kube := writeKubeconfig(t)
	path := writeTargets(t, `registries:
  - host: reg.example.net
clusters:
  - cluster: prod
    kubeconfig: `+kube+`
  - cluster: lab
    nodes:
      - name: s
        role: server
        host: 10.0.0.1
        user: root
current: prod
`)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prod, err := f.Select("prod")
	if err != nil {
		t.Fatalf("Select prod: %v", err)
	}
	if len(prod.Registries) != 0 {
		t.Errorf("kubeconfig cluster inherited %d registries", len(prod.Registries))
	}
	lab, err := f.Select("lab")
	if err != nil {
		t.Fatalf("Select lab: %v", err)
	}
	if len(lab.Registries) != 1 {
		t.Errorf("SSH cluster got %d registries, want the file-level default", len(lab.Registries))
	}
}

// A path is handed to os.Stat and to kubectl as an argument; neither expands ~.
func TestKubeconfigPathExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory in this environment")
	}
	tg := &Targets{Cluster: "a", Kubeconfig: "~/.kube/config"}
	want := filepath.Join(home, ".kube", "config")
	if got := tg.KubeconfigPath(); got != want {
		t.Errorf("KubeconfigPath() = %q, want %q", got, want)
	}
	// A plain path is left alone.
	tg.Kubeconfig = "/etc/kube/config"
	if got := tg.KubeconfigPath(); got != "/etc/kube/config" {
		t.Errorf("absolute path rewritten to %q", got)
	}
}

// The message a caller gets when it asks a kubeconfig cluster for a machine
// has to say what is actually going on, not "no server node".
func TestServerOnKubeconfigClusterExplainsItself(t *testing.T) {
	tg := &Targets{Cluster: "prod", Kubeconfig: "/k/c.yaml"}
	_, err := tg.Server()
	if err == nil {
		t.Fatal("Server() returned a node for a cluster that has none")
	}
	if !strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("error %q reads like a malformed file rather than a kubeconfig cluster", err)
	}
}

// A nodes-only file must keep validating exactly as before.
func TestSSHClusterStillNeedsNodes(t *testing.T) {
	path := writeTargets(t, "cluster: prod\nnodes: []\n")
	_, err := LoadTargets(path)
	if err == nil {
		t.Fatal("a cluster with neither nodes nor a kubeconfig was accepted")
	}
	if !strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("error %q does not mention the other way to reach a cluster", err)
	}
}
