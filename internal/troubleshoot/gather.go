package troubleshoot

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/solutionforest/k3helper/internal/kube"
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
		PodEvents:        map[string][]string{},
		PodStatuses:      map[string]string{},
		ContainerStates:  map[string]string{},
		NodeConditions:   map[string][]string{},
		K3sService:       map[string]string{},
		HostMetrics:      map[string]HostMetric{},
		PVCEvents:        map[string][]string{},
		ContainerRuntime: map[string]string{},
		ClockSkew:        map[string]time.Duration{},
		Unreachable:      g.Unreachable,
	}

	// --- cluster layer via kubectl ---
	nodesJSON, code, err := g.Server.Run(kubectl(`get nodes -o json`))
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
		if state, ok := runtimeState(exec); ok {
			e.ContainerRuntime[name] = state
		}
		if skew, ok := clockSkew(exec, time.Now()); ok {
			e.ClockSkew[name] = skew
		}
	}
	return e
}

// runtimeState reports the container runtime unit's state. k3s embeds
// containerd inside its own unit, so an absent containerd.service is normal
// there and reported as "" (no evidence) rather than a fault.
func runtimeState(exec ssh.Executor) (string, bool) {
	out, _, err := exec.Run(
		`for u in containerd cri-o docker; do ` +
			`if systemctl list-unit-files $u.service --no-legend 2>/dev/null | grep -q $u; then ` +
			`echo "$u=$(sudo -n systemctl is-active $u 2>/dev/null)"; break; fi; done`)
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(out)
	if s == "" {
		return "", false
	}
	return s, true
}

// clockSkew measures the node's clock against this machine's. Certificates
// and etcd leases are time-sensitive, and a badly skewed node fails TLS in
// ways that look like anything but a clock problem.
func clockSkew(exec ssh.Executor, now time.Time) (time.Duration, bool) {
	out, code, err := exec.Run(`date +%s`)
	if err != nil || code != 0 {
		return 0, false
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, false
	}
	skew := time.Unix(secs, 0).Sub(now)
	if skew < 0 {
		skew = -skew
	}
	return skew, true
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
	if pods, code, err := g.Server.Run(kubectl(`get pods -A -o json`)); err == nil && code == 0 {
		parsePods(pods, e)
	}
	// events linger ~1h after pod deletion; events for dead pods are stale
	// evidence that would fire signatures on a healthy cluster.
	if evs, code, err := g.Server.Run(kubectl(`get events -A -o json`)); err == nil && code == 0 {
		parseEvents(evs, e)
	}
	filterStalePodEvidence(e)
	if pvcs, code, err := g.Server.Run(kubectl(`get pvc -A -o json`)); err == nil && code == 0 {
		parsePVCs(pvcs, e)
		filterStalePVCEvidence(e)
	}
	// endpoints back every Service; a Service with none is a silent outage
	// that no pod-level signature reports.
	if eps, code, err := g.Server.Run(kubectl(`get endpoints -A -o json`)); err == nil && code == 0 {
		parseEndpoints(eps, e)
	}
	g.collectCoreDNS(e)
	g.collectCerts(e)
	g.collectEtcd(e)
}

// kubectl builds a server-side kubectl command that works on k3s or kubeadm.
func kubectl(args string) string {
	return kube.Cmd(args)
}

// collectCoreDNS records whether cluster DNS has ready replicas. Everything
// in the cluster resolves through it, so it is worth its own signature
// rather than being buried in a generic "pod not ready" finding.
func (g Gatherer) collectCoreDNS(e *Evidence) {
	out, code, err := g.Server.Run(kubectl(`get deployment coredns -n kube-system -o jsonpath={.status.readyReplicas}/{.spec.replicas}`))
	if err != nil || code != 0 {
		return
	}
	ready, desired, ok := parseReadyRatio(out)
	if !ok {
		return
	}
	e.CoreDNS = &ReadyRatio{Ready: ready, Desired: desired}
}

// collectCerts reads k3s certificate expiry. k3s auto-rotates on restart
// within 90 days of expiry, but a server that has not restarted in a year
// will simply stop accepting connections.
func (g Gatherer) collectCerts(e *Evidence) {
	// `k3s certificate check` prints lines like:
	//   Checking certificate CN=k3s-serving, expires 2027-01-05
	out, code, err := g.Server.Run(`sudo -n k3s certificate check 2>&1`)
	if err != nil || code != 0 {
		return
	}
	e.CertExpiryDays, e.CertSubject = parseCertExpiry(out, time.Now())
}

// collectEtcd checks etcd quorum when the cluster runs embedded etcd. On a
// single-server k3s (sqlite backend) there is no etcd and this is skipped.
func (g Gatherer) collectEtcd(e *Evidence) {
	out, code, err := g.Server.Run(kubectl(`get nodes -l node-role.kubernetes.io/etcd=true -o json`))
	if err != nil || code != 0 {
		return
	}
	nodes, err := parseNodes(out)
	if err != nil || len(nodes) == 0 {
		return // sqlite-backed cluster: no etcd members to check
	}
	total, ready := len(nodes), 0
	for _, n := range nodes {
		for _, c := range n.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready++
			}
		}
	}
	e.Etcd = &ReadyRatio{Ready: ready, Desired: total}
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
	// Image count feeds the image-GC signature: disk filling up with a large
	// image cache has a different fix from disk filling up with data.
	outImg, codeImg, errImg := exec.Run(
		`(sudo -n k3s crictl images -q 2>/dev/null || sudo -n crictl images -q 2>/dev/null) | wc -l`)
	if errImg == nil && codeImg == 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(outImg)); err == nil {
			m.Images = n
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

// filterStalePVCEvidence drops events for PVCs that have been deleted.
// Events outlive their object by about an hour, so a PVC removed minutes ago
// keeps reporting "stuck Pending" long after the problem is gone.
func filterStalePVCEvidence(e *Evidence) {
	if e.livePVCs == nil {
		return // PVC list unavailable; can't judge staleness
	}
	for key := range e.PVCEvents {
		if !e.livePVCs[key] {
			delete(e.PVCEvents, key)
		}
	}
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
