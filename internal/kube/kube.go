// Package kube reads cluster resources through kubectl on a server node.
// It is deliberately transport-agnostic: everything takes an Executor, so the
// TUI can drive it over SSH and tests can drive it with canned output.
package kube

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Executor runs a command somewhere kubectl exists (satisfied by *ssh.Client).
type Executor interface {
	Run(cmd string) (string, int, error)
}

// The kubectl invocations for each supported distribution. Both name an
// explicit --kubeconfig: a bare `kubectl` would run against whatever context
// the SSH user's own ~/.kube/config points at, so a node whose k3s is briefly
// down could report another cluster's state under this cluster's name.
const (
	k3sBase     = `sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml`
	kubeadmBase = `sudo -n kubectl --kubeconfig /etc/kubernetes/admin.conf`
)

// DetectBase picks the kubectl invocation this node can actually use.
//
// It probes once rather than chaining the candidates with || on every call.
// A chain cannot tell "this distribution is not installed" from "kubectl ran
// and rejected your manifest": the first arm's failure would fall through and
// the caller would be shown the *second* arm's "no such file" instead of the
// validation error it actually needed.
func DetectBase(exec Executor) string {
	if _, code, err := exec.Run(`test -x /usr/local/bin/k3s || command -v k3s >/dev/null 2>&1`); err == nil && code == 0 {
		return k3sBase
	}
	// `test -e`, not `sudo -n test -r`: a host may grant passwordless sudo for
	// kubectl specifically and not for a generic `test`, and existence only
	// needs traverse permission on /etc/kubernetes, which is world-executable.
	if _, code, err := exec.Run(`test -e /etc/kubernetes/admin.conf`); err == nil && code == 0 {
		return kubeadmBase
	}
	return k3sBase // nothing detected: k3s is the supported default, and its
	// error message will say so plainly
}

// Cmd builds a kubectl command for the k3s layout only.
//
// Prefer Builder, which detects the node's distribution. This exists for
// callers that have no executor to probe with, and for tests.
func Cmd(args string) string {
	return k3sBase + " " + args
}

// Builder returns a function that renders kubectl commands for this node,
// having detected the distribution once.
func Builder(exec Executor) func(args string) string {
	base := DetectBase(exec)
	return func(args string) string { return base + " " + args }
}

// nsFlag renders the namespace selector: empty means all namespaces.
func nsFlag(ns string) string {
	if ns == "" {
		return "-A"
	}
	return "-n " + shellQuote(ns)
}

// shellQuote wraps a value in single quotes for the remote shell, rejecting
// nothing — callers pass namespace/pod names that came from the API server.
// An embedded quote is escaped rather than allowed to terminate the word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Pod is one row of the pod browser.
type Pod struct {
	Namespace string
	Name      string
	Ready     string // "1/2"
	Status    string
	Restarts  int
	Node      string
	Age       time.Duration
}

// Node is one row of the node browser.
type Node struct {
	Name    string
	Status  string
	Roles   string
	Version string
	Age     time.Duration
}

// Event is one row of the event browser.
type Event struct {
	Namespace string
	Type      string
	Reason    string
	Object    string
	Message   string
	Age       time.Duration
	Count     int
}

// ListPods returns pods in ns ("" = all namespaces), newest problems first is
// left to the caller; ordering here is namespace/name for stability.
func ListPods(exec Executor, ns string) ([]Pod, error) {
	out, code, err := exec.Run(Builder(exec)("get pods "+nsFlag(ns)+" -o json") + " 2>/dev/null")
	if err != nil {
		return nil, err
	}
	if code != 0 || strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("kubectl get pods failed (exit %d)", code)
	}
	var raw struct {
		Items []struct {
			Metadata struct {
				Namespace         string    `json:"namespace"`
				Name              string    `json:"name"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				Phase             string `json:"phase"`
				Reason            string `json:"reason"`
				ContainerStatuses []struct {
					Ready        bool `json:"ready"`
					RestartCount int  `json:"restartCount"`
					State        struct {
						Waiting *struct {
							Reason string `json:"reason"`
						} `json:"waiting"`
						Terminated *struct {
							Reason string `json:"reason"`
						} `json:"terminated"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse pods: %w", err)
	}
	now := time.Now()
	pods := make([]Pod, 0, len(raw.Items))
	for _, it := range raw.Items {
		p := Pod{
			Namespace: it.Metadata.Namespace,
			Name:      it.Metadata.Name,
			Node:      it.Spec.NodeName,
			Status:    it.Status.Phase,
			Age:       now.Sub(it.Metadata.CreationTimestamp),
		}
		if it.Status.Reason != "" {
			p.Status = it.Status.Reason
		}
		ready := 0
		for _, cs := range it.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			p.Restarts += cs.RestartCount
			// A waiting/terminated reason is what the operator needs to see,
			// not the generic "Pending"/"Running" phase.
			if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
				p.Status = cs.State.Waiting.Reason
			} else if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" &&
				cs.State.Terminated.Reason != "Completed" {
				p.Status = cs.State.Terminated.Reason
			}
		}
		p.Ready = fmt.Sprintf("%d/%d", ready, len(it.Status.ContainerStatuses))
		pods = append(pods, p)
	}
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Name < pods[j].Name
	})
	return pods, nil
}

// Healthy reports whether the pod is in a state an operator can ignore.
func (p Pod) Healthy() bool {
	switch p.Status {
	case "Succeeded", "Completed":
		// A finished Job pod has no running containers by design.
		return true
	case "Running":
		// "Running" with nothing ready is a pod failing its readiness probe,
		// which is an outage however healthy the phase reads.
		return !strings.HasPrefix(p.Ready, "0/")
	}
	return false
}

// ListNodes returns the cluster's nodes.
func ListNodes(exec Executor) ([]Node, error) {
	out, code, err := exec.Run(Builder(exec)("get nodes -o json") + " 2>/dev/null")
	if err != nil {
		return nil, err
	}
	if code != 0 || strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("kubectl get nodes failed (exit %d)", code)
	}
	var raw struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				Labels            map[string]string `json:"labels"`
				CreationTimestamp time.Time         `json:"creationTimestamp"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				NodeInfo struct {
					KubeletVersion string `json:"kubeletVersion"`
				} `json:"nodeInfo"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse nodes: %w", err)
	}
	now := time.Now()
	nodes := make([]Node, 0, len(raw.Items))
	for _, it := range raw.Items {
		n := Node{
			Name:    it.Metadata.Name,
			Status:  "NotReady",
			Version: it.Status.NodeInfo.KubeletVersion,
			Age:     now.Sub(it.Metadata.CreationTimestamp),
		}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				n.Status = "Ready"
			}
		}
		var roles []string
		for label := range it.Metadata.Labels {
			if r, ok := strings.CutPrefix(label, "node-role.kubernetes.io/"); ok && r != "" {
				roles = append(roles, r)
			}
		}
		sort.Strings(roles)
		n.Roles = strings.Join(roles, ",")
		if n.Roles == "" {
			n.Roles = "<none>"
		}
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, nil
}

// ListEvents returns recent events, most recent first, warnings included.
func ListEvents(exec Executor, ns string) ([]Event, error) {
	out, code, err := exec.Run(Builder(exec)("get events "+nsFlag(ns)+" -o json") + " 2>/dev/null")
	if err != nil {
		return nil, err
	}
	if code != 0 || strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("kubectl get events failed (exit %d)", code)
	}
	var raw struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			InvolvedObject struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"involvedObject"`
			Reason        string    `json:"reason"`
			Message       string    `json:"message"`
			Type          string    `json:"type"`
			Count         int       `json:"count"`
			LastTimestamp time.Time `json:"lastTimestamp"`
			EventTime     time.Time `json:"eventTime"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse events: %w", err)
	}
	now := time.Now()
	events := make([]Event, 0, len(raw.Items))
	for _, it := range raw.Items {
		// Events carry either lastTimestamp (v1) or eventTime (events.k8s.io).
		ts := it.LastTimestamp
		if ts.IsZero() {
			ts = it.EventTime
		}
		events = append(events, Event{
			Namespace: it.Metadata.Namespace,
			Type:      it.Type,
			Reason:    it.Reason,
			Object:    it.InvolvedObject.Kind + "/" + it.InvolvedObject.Name,
			Message:   strings.ReplaceAll(it.Message, "\n", " "),
			Count:     it.Count,
			Age:       now.Sub(ts),
		})
	}
	// Most recent first: that is what an operator scrolls for.
	sort.Slice(events, func(i, j int) bool { return events[i].Age < events[j].Age })
	return events, nil
}

// Logs returns the last tail lines from a pod. previous reads the log of the
// prior container instance, which is where a CrashLoopBackOff cause lives.
func Logs(exec Executor, ns, pod string, tail int, previous bool) (string, error) {
	if tail <= 0 {
		tail = 200
	}
	args := fmt.Sprintf("logs %s -n %s --tail=%d --all-containers=true --timestamps=false",
		shellQuote(pod), shellQuote(ns), tail)
	if previous {
		args += " --previous"
	}
	out, code, err := exec.Run(Builder(exec)(args))
	if err != nil {
		return "", err
	}
	if code != 0 {
		if previous {
			return "", fmt.Errorf("no previous container instance for %s/%s", ns, pod)
		}
		return "", fmt.Errorf("kubectl logs failed (exit %d): %s", code, strings.TrimSpace(out))
	}
	return out, nil
}

// Describe returns `kubectl describe` output for one object.
func Describe(exec Executor, kind, ns, name string) (string, error) {
	args := fmt.Sprintf("describe %s %s", shellQuote(kind), shellQuote(name))
	if ns != "" {
		args += " -n " + shellQuote(ns)
	}
	out, code, err := exec.Run(Builder(exec)(args))
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("kubectl describe failed (exit %d): %s", code, strings.TrimSpace(out))
	}
	return out, nil
}

// ShortAge renders a duration the way kubectl does: 3d, 4h, 12m, 30s.
func ShortAge(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
