// Package sandbox locates the test sandbox described by
// test/sandbox/targets.sandbox.yaml. Integration tests must never hardcode
// host/port: the sandbox is a set of OrbStack VMs whose IPs change every time
// they are recreated.
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/ssh"
)

// TargetsPath returns the absolute path of the sandbox targets file, honouring
// K3HELPER_TARGETS for callers that point at a different cluster.
func TargetsPath() (string, error) {
	if p := os.Getenv("K3HELPER_TARGETS"); p != "" {
		return filepath.Abs(p)
	}
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "test", "sandbox", "targets.sandbox.yaml"), nil
}

// repoRoot walks up from the working directory to the directory holding go.mod,
// so tests resolve the same paths regardless of which package they run in.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// Targets loads the sandbox targets file with node key paths resolved to
// absolute paths (they are stored relative to the repo root).
func Targets() (*config.Targets, error) {
	path, err := TargetsPath()
	if err != nil {
		return nil, err
	}
	targets, err := config.LoadTargets(path)
	if err != nil {
		return nil, err
	}
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	for i := range targets.Nodes {
		if k := targets.Nodes[i].Key; k != "" && !filepath.IsAbs(k) {
			targets.Nodes[i].Key = filepath.Join(root, k)
		}
	}
	return targets, nil
}

// Node returns the named node from the sandbox targets file.
func Node(name string) (ssh.Node, error) {
	targets, err := Targets()
	if err != nil {
		return ssh.Node{}, err
	}
	for _, n := range targets.Nodes {
		if n.Name == name {
			return n.SSH(), nil
		}
	}
	return ssh.Node{}, fmt.Errorf("no node %q in sandbox targets", name)
}

// Agents returns every agent node.
func Agents() ([]ssh.Node, error) {
	targets, err := Targets()
	if err != nil {
		return nil, err
	}
	var out []ssh.Node
	for _, n := range targets.Agents() {
		out = append(out, n.SSH())
	}
	return out, nil
}

// Dial connects to the named sandbox node, skipping the test when the sandbox
// is not running so `go test -tags=integration ./...` degrades to a skip
// instead of a wall of connection failures.
func Dial(t *testing.T, name string) *ssh.Client {
	t.Helper()
	node, err := Node(name)
	if err != nil {
		t.Skipf("sandbox targets unavailable: %v (run `make sandbox-up`)", err)
	}
	c, err := ssh.Dial(node)
	if err != nil {
		t.Skipf("sandbox node %q unreachable at %s:%d: %v (run `make sandbox-up`)", name, node.Host, node.Port, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
