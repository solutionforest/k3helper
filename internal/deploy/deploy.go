// Package deploy applies manifests to the cluster with a verify-first,
// wait-after-apply flow.
package deploy

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/solutionforest/k3helper/internal/kube"
	"github.com/solutionforest/k3helper/internal/kyaml"
	"github.com/solutionforest/k3helper/internal/ssh"
)

// Executor is the machine kubectl runs on: it executes commands and can
// receive files. Deploy needs both, because the manifest the user names is a
// path on *their* machine and kubectl reads it on the *cluster* machine.
// Satisfied by *ssh.Client.
type Executor interface {
	Run(cmd string) (string, int, error)
	WriteFile(remotePath string, data []byte, mode os.FileMode) error
	RemoveFile(remotePath string) error
}

// Result summarizes a deploy.
type Result struct {
	Applied   []string // kind/name of applied resources
	RolledOut []string // resources that reached desired state
	Errors    []string
	// Diff is the unified diff against live cluster state, populated when
	// Options.Diff is set. Empty means the manifest changes nothing.
	Diff string
}

// Options controls the deploy flow.
type Options struct {
	Namespace   string        // "" = whatever the manifest says
	WaitTimeout time.Duration // per-resource rollout wait
	DryRun      bool          // server dry-run only, no changes
	Diff        bool          // capture a diff against live state before applying
}

// kubectlFor detects the node's kubectl invocation once per deploy, so a
// failure from the real command is reported rather than a fallback arm's.
func kubectlFor(exec Executor) func(string) string {
	return kube.Builder(exec)
}

// namespaceArg renders the -n flag for the kubectl invocations. Empty when no
// override was requested, in which case each document lands wherever its own
// metadata.namespace says (or the kubeconfig default).
func namespaceArg(ns string) string {
	if ns == "" {
		return ""
	}
	return fmt.Sprintf(" -n '%s'", ns)
}

// validNamespace guards the value before it reaches a shell command. RFC 1123
// label rules are what the API server enforces anyway, so rejecting here gives
// a better message than a quoting failure would.
func validNamespace(ns string) error {
	if ns == "" {
		return nil
	}
	if len(ns) > 63 {
		return fmt.Errorf("namespace %q is longer than 63 characters", ns)
	}
	for i, r := range ns {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(ns)-1:
		default:
			return fmt.Errorf("invalid namespace %q: must be lowercase alphanumeric or '-', starting and ending alphanumeric", ns)
		}
	}
	return nil
}

// manifestNamespaces maps each resource the manifest declares to the namespace
// it declares, keyed by lowercase kind and name ("deployment/web").
//
// The rollout wait needs this per resource, not one namespace for the whole
// file: a manifest may place a Deployment in team-a and another in team-b, and
// waiting for both in a single namespace reports a failure for a deploy that
// actually succeeded.
func manifestNamespaces(data []byte) map[string]string {
	out := map[string]string{}
	for _, d := range kyaml.Verify(data).Documents {
		if d.Namespace == "" || d.Kind == "" || d.Name == "" {
			continue
		}
		out[strings.ToLower(d.Kind)+"/"+d.Name] = d.Namespace
	}
	return out
}

// namespaceFor picks the namespace to wait in for one applied resource.
// An explicit --namespace always wins; otherwise the manifest's own namespace
// for that resource is used, and failing that kubectl's default.
//
// applied names look like "deployment.apps/web" or "service/web-svc".
func namespaceFor(applied, override string, declared map[string]string) string {
	if override != "" {
		return override
	}
	kind, name, ok := strings.Cut(applied, "/")
	if !ok {
		return ""
	}
	if group := strings.IndexByte(kind, '.'); group >= 0 {
		kind = kind[:group] // "deployment.apps" -> "deployment"
	}
	ns := declared[strings.ToLower(kind)+"/"+name]
	// This value came from the manifest, not from a validated flag. The API
	// server would reject anything that is not an RFC-1123 label, so reaching
	// a shell with one is not possible today — but validate rather than rely
	// on that.
	if validNamespace(ns) != nil {
		return ""
	}
	return ns
}

// conflictingNamespaces returns documents whose explicit metadata.namespace
// disagrees with the requested override. Silently redirecting those is how
// resources end up in the wrong namespace, so Deploy refuses instead.
func conflictingNamespaces(data []byte, ns string) []string {
	if ns == "" {
		return nil
	}
	var conflicts []string
	for _, d := range kyaml.Verify(data).Documents {
		if d.Namespace != "" && d.Namespace != ns {
			name := d.Kind + "/" + d.Name
			conflicts = append(conflicts, fmt.Sprintf("%s declares namespace %q", name, d.Namespace))
		}
	}
	return conflicts
}

// Deploy validates then applies a local manifest file on the cluster.
//
// manifest is a path on the caller's machine. It is uploaded to a temporary
// file on the target before kubectl runs, and removed afterwards.
func Deploy(exec Executor, manifest string, opts Options) (*Result, error) {
	res := &Result{}
	kubectlCmd := kubectlFor(exec)

	if err := validNamespace(opts.Namespace); err != nil {
		return res, err
	}
	ns := namespaceArg(opts.Namespace)

	data, err := os.ReadFile(manifest)
	if err != nil {
		return res, fmt.Errorf("read manifest: %w", err)
	}
	if conflicts := conflictingNamespaces(data, opts.Namespace); len(conflicts) > 0 {
		return res, fmt.Errorf("--namespace %s conflicts with the manifest: %s; remove metadata.namespace or drop the flag",
			opts.Namespace, strings.Join(conflicts, "; "))
	}
	remote, err := kyaml.RemotePath(manifest)
	if err != nil {
		return res, err
	}
	if err := exec.WriteFile(remote, data, 0o600); err != nil {
		return res, fmt.Errorf("upload manifest to target: %w", err)
	}
	defer exec.RemoveFile(remote)

	// 1. server-side dry-run validation
	dry := kubectlCmd(fmt.Sprintf(`apply --dry-run=server%s -f '%s'`, ns, remote)) + " 2>&1"
	out, code, err := exec.Run(dry)
	if err != nil {
		return res, fmt.Errorf("kubectl failed: %w", err)
	}
	if code != 0 {
		return res, fmt.Errorf("dry-run validation failed:\n%s", out)
	}
	res.Applied = parseApplied(out)

	// 2. optional diff against live state, before anything is changed
	if opts.Diff {
		diff, err := diffAgainstLive(exec, kubectlCmd, ns, remote)
		if err != nil {
			return res, err
		}
		res.Diff = diff
	}

	if opts.DryRun {
		return res, nil
	}

	// 3. real apply
	apply := kubectlCmd(fmt.Sprintf(`apply%s -f '%s'`, ns, remote)) + " 2>&1"
	out, code, err = exec.Run(apply)
	if err != nil {
		return res, fmt.Errorf("kubectl failed: %w", err)
	}
	if code != 0 {
		return res, fmt.Errorf("apply failed:\n%s", out)
	}
	res.Applied = parseApplied(out)

	// 4. wait for rollout
	timeout := opts.WaitTimeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	// A manifest may name its own namespace while --namespace was not given,
	// and different documents may name different ones. Resolve per resource:
	// waiting in the wrong namespace reports a failure for a deploy that
	// actually succeeded.
	declared := manifestNamespaces(data)
	for _, name := range res.Applied {
		waitNS := namespaceArg(namespaceFor(name, opts.Namespace, declared))
		if err := waitRollout(exec, kubectlCmd, waitNS, name, timeout); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", name, err))
		} else {
			res.RolledOut = append(res.RolledOut, name)
		}
	}
	return res, nil
}

// diffAgainstLive returns `kubectl diff` output for the manifest.
//
// kubectl diff uses exit codes as data: 0 means no differences, 1 means there
// are differences, and anything above that is a real failure. Treating 1 as an
// error would make every changed manifest look broken.
func diffAgainstLive(exec Executor, kubectlCmd func(string) string, ns, remote string) (string, error) {
	cmd := kubectlCmd(fmt.Sprintf(`diff%s -f '%s'`, ns, remote)) + " 2>&1"
	out, code, err := exec.Run(cmd)
	if err != nil {
		return "", fmt.Errorf("kubectl diff: %w", err)
	}
	switch code {
	case 0:
		return "", nil // identical to live state
	case 1:
		return out, nil
	default:
		return "", fmt.Errorf("kubectl diff failed (exit %d):\n%s", code, strings.TrimSpace(out))
	}
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
func waitRollout(exec Executor, kubectlCmd func(string) string, ns, name string, timeout time.Duration) error {
	// These three support `kubectl rollout status`; anything else only has to
	// exist. DaemonSet was named in this comment but missing from the
	// condition, so a DaemonSet whose pods never started was reported as
	// rolled out purely because the object had been created.
	if strings.HasPrefix(name, "deployment.") || strings.HasPrefix(name, "statefulset.") ||
		strings.HasPrefix(name, "daemonset.") {
		cmd := kubectlCmd(fmt.Sprintf(`rollout status%s '%s' --timeout=%s`, ns, name, formatDuration(timeout))) + " 2>&1"
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
	cmd := kubectlCmd(fmt.Sprintf(`get%s '%s' -o name`, ns, name)) + " 2>&1"
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
