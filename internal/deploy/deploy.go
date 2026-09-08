// Package deploy applies manifests to the cluster with a verify-first,
// wait-after-apply flow.
package deploy

import (
	"fmt"
	"strings"
	"time"

	"github.com/solutionforest/k3helper/internal/ssh"
)

// Executor runs kubectl-bearing commands (server SSH client or local kubectl).
type Executor interface {
	Run(cmd string) (string, int, error)
}

// Result summarizes a deploy.
type Result struct {
	Applied   []string // kind/name of applied resources
	RolledOut []string // resources that reached desired state
	Errors    []string
}

// Options controls the deploy flow.
type Options struct {
	Namespace   string        // "" = whatever the manifest says
	WaitTimeout time.Duration // per-resource rollout wait
	DryRun      bool          // server dry-run only, no changes
}

// kubectlBase returns the kubectl invocation prefix for the executor type.
func kubectlBase() string {
	return `sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml`
}

// Deploy validates then applies a manifest file on the cluster.
func Deploy(exec Executor, manifest string, opts Options) (*Result, error) {
	res := &Result{}
	kb := kubectlBase()

	// 1. server-side dry-run validation
	dry := fmt.Sprintf(`%s apply --dry-run=server -f %s 2>&1`, kb, manifest)
	out, code, err := exec.Run(dry)
	if err != nil {
		return res, fmt.Errorf("kubectl failed: %w", err)
	}
	if code != 0 {
		return res, fmt.Errorf("dry-run validation failed:\n%s", out)
	}
	res.Applied = parseApplied(out)
	if opts.DryRun {
		return res, nil
	}

	// 2. real apply
	apply := fmt.Sprintf(`%s apply -f %s 2>&1`, kb, manifest)
	out, code, err = exec.Run(apply)
	if err != nil {
		return res, fmt.Errorf("kubectl failed: %w", err)
	}
	if code != 0 {
		return res, fmt.Errorf("apply failed:\n%s", out)
	}
	res.Applied = parseApplied(out)

	// 3. wait for rollout
	timeout := opts.WaitTimeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	for _, name := range res.Applied {
		if err := waitRollout(exec, kb, name, timeout); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", name, err))
		} else {
			res.RolledOut = append(res.RolledOut, name)
		}
	}
	return res, nil
}

// parseApplied extracts "deployment.apps/web created" style lines
// (including "(server dry run)" suffixed dry-run output).
func parseApplied(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		last := fields[len(fields)-1]
		// strip "(server dry run)" / "(dry run)" suffix: cut at the first "("
		if idx := strings.Index(line, " ("); idx > 0 {
			fields = strings.Fields(strings.TrimSpace(line[:idx]))
			if len(fields) < 2 {
				continue
			}
			last = fields[len(fields)-1]
		}
		switch last {
		case "created", "configured", "unchanged":
			names = append(names, fields[0])
		}
	}
	return names
}

// waitRollout waits for a workload or generic resource to be ready.
func waitRollout(exec Executor, kb, name string, timeout time.Duration) error {
	// deployment/statefulset/daemonset support rollout status; others just exist
	if strings.HasPrefix(name, "deployment.") || strings.HasPrefix(name, "statefulset.") {
		cmd := fmt.Sprintf(`%s rollout status %s --timeout=%s 2>&1`, kb, name, formatDuration(timeout))
		out, code, err := exec.Run(cmd)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("rollout failed: %s", firstLine(out))
		}
		return nil
	}
	// generic: verify resource exists
	cmd := fmt.Sprintf(`%s get %s -o name 2>&1`, kb, name)
	_, code, err := exec.Run(cmd)
	if err != nil || code != 0 {
		return fmt.Errorf("resource missing after apply")
	}
	return nil
}

func formatDuration(d time.Duration) string {
	secs := int(d.Seconds())
	return fmt.Sprintf("%ds", secs)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Ensure ssh.Client satisfies Executor (compile-time).
var _ Executor = (*ssh.Client)(nil)
