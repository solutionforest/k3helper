package troubleshoot

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/solutionforest/k3helper/internal/ssh"
)

// Gatherer collects Evidence from the cluster (via kubectl on the server) and hosts (via SSH).
type Gatherer struct {
	// Server runs kubectl commands (k3s kubectl on the server node).
	Server ssh.Executor
	// Hosts: node name → executor for host-level probes (nil skips host layer).
	Hosts map[string]ssh.Executor
	// Unreachable: nodes in the targets file we could not connect to.
	Unreachable []UnreachableNode
}

// Collect assembles an Evidence bundle. Never fails: collection problems
// are recorded as evidence (e.g. broken kubeconfig is itself a diagnosis input).
func (g Gatherer) Collect() Evidence {
	e := Evidence{
		PodEvents:       map[string][]string{},
		PodStatuses:     map[string]string{},
		ContainerStates: map[string]string{},
		NodeConditions:  map[string][]string{},
		K3sService:      map[string]string{},
		HostMetrics:     map[string]HostMetric{},
		PVCEvents:       map[string][]string{},
		Unreachable:     g.Unreachable,
	}

	// --- cluster layer via kubectl ---
	nodesJSON, code, err := g.Server.Run(`sudo -n k3s kubectl get nodes -o json --kubeconfig /etc/rancher/k3s/k3s.yaml 2>/dev/null || kubectl get nodes -o json 2>/dev/null`)
	if err != nil || code != 0 || strings.TrimSpace(nodesJSON) == "" {
		e.KubeconfigError = fmt.Sprintf("kubectl get nodes failed (exit %d): %s", code, firstLine(nodesJSON))
	} else {
		g.collectCluster(&e, nodesJSON)
	}

	// --- host layer via SSH ---
	for name, exec := range g.Hosts {
		// probe the correct unit for the node's role: agents run "k3s-agent",
		// servers run "k3s". Pick the unit that actually exists on this node
		// (piping is-active would mask the exit code, so branch on unit presence).
		out, _, err := exec.Run(`if systemctl list-unit-files k3s-agent.service 2>/dev/null | grep -q k3s-agent; then sudo -n systemctl is-active k3s-agent; else sudo -n systemctl is-active k3s; fi`)
		if err == nil && strings.TrimSpace(out) != "" {
			// "active" (exit 0) or "inactive"/"failed"/"activating" (exit 3)
			// are all valid evidence; empty output = unit truly absent
			e.K3sService[name] = strings.TrimSpace(out)
		} else {
			e.K3sService[name] = "" // not installed
		}
		if m, ok := hostMetric(exec); ok {
			e.HostMetrics[name] = m
		}
	}
	return e
}

func (g Gatherer) collectCluster(e *Evidence, nodesJSON string) {
	nodes, err := parseNodes(nodesJSON)
	if err != nil {
		e.KubeconfigError = "unparseable node list: " + err.Error()
		return
	}
	for _, n := range nodes {
		for _, c := range n.Conditions {
			// NotReady = Ready!=True (False or Unknown — Unknown means
			// node lost contact, e.g. kubelet/service down)
			if c.Type == "Ready" && c.Status != "True" {
				e.NodeNotReady = append(e.NodeNotReady, n.Name)
			}
			// only count pressure conditions that are actively True
			if c.Type != "Ready" && c.Status == "True" {
				e.NodeConditions[n.Name] = append(e.NodeConditions[n.Name], c.Type)
			}
		}
	}

	// pod statuses + events
	if pods, code, err := g.Server.Run(`sudo -n k3s kubectl get pods -A -o json --kubeconfig /etc/rancher/k3s/k3s.yaml 2>/dev/null || kubectl get pods -A -o json 2>/dev/null`); err == nil && code == 0 {
		parsePods(pods, e)
	}
	// events linger ~1h after pod deletion; events for dead pods are stale
	// evidence that would fire signatures on a healthy cluster.
	if evs, code, err := g.Server.Run(`sudo -n k3s kubectl get events -A -o json --kubeconfig /etc/rancher/k3s/k3s.yaml 2>/dev/null || kubectl get events -A -o json 2>/dev/null`); err == nil && code == 0 {
		parseEvents(evs, e)
	}
	filterStalePodEvidence(e)
	if pvcs, code, err := g.Server.Run(`sudo -n k3s kubectl get pvc -A -o json --kubeconfig /etc/rancher/k3s/k3s.yaml 2>/dev/null || kubectl get pvc -A -o json 2>/dev/null`); err == nil && code == 0 {
		parsePVCs(pvcs, e)
	}
}

func hostMetric(exec ssh.Executor) (HostMetric, bool) {
	m := HostMetric{}
	out, code, err := exec.Run(`df -P / | tail -1 | awk '{print $5}'`)
	if err != nil || code != 0 {
		return m, false
	}
	pct, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(out), "%"))
	if err != nil {
		return m, false
	}
	m.DiskUsedPercent = pct
	outMem, codeMem, errMem := exec.Run(`free -m | awk '/^Mem:/{print $7}'`)
	if errMem == nil && codeMem == 0 {
		if mb, err := strconv.Atoi(strings.TrimSpace(outMem)); err == nil {
			m.AvailMemMB = mb
		}
	}
	return m, true
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// filterStalePodEvidence removes event evidence for pods that no longer exist
// (events linger ~1h after deletion). LivePods is populated by parsePods.
func filterStalePodEvidence(e *Evidence) {
	if e.livePods == nil {
		return // pod list unavailable; can't judge staleness
	}
	for key := range e.PodEvents {
		if !e.livePods[key] {
			delete(e.PodEvents, key)
		}
	}
	for key := range e.PodStatuses {
		if !e.livePods[key] {
			delete(e.PodStatuses, key)
		}
	}
	for key := range e.ContainerStates {
		if !e.livePods[key] {
			delete(e.ContainerStates, key)
		}
	}
}
