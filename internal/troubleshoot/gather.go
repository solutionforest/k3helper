package troubleshoot

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/solutionforest/k3helper/internal/check"
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
	// ServerNodes: targets-file names of the nodes with role "server". Needed
	// to assess etcd quorum from host evidence when the API server — the thing
	// quorum loss takes down — cannot be reached to ask.
	ServerNodes []string
	// NoHostLayer says there is no host layer to gather, as opposed to one
	// that was not reachable. Set for kubeconfig clusters.
	NoHostLayer bool
}

// Collect assembles an Evidence bundle. Never fails: collection problems
// are recorded as evidence (e.g. broken kubeconfig is itself a diagnosis input).
func (g Gatherer) Collect() Evidence {
	// Detect the node's kubectl once: chaining candidates per call would hide
	// the real error behind the fallback arm's.
	base := kube.Builder(g.Server)
	// JSON queries must not carry stderr: kubectl prints deprecation warnings
	// there ("v1 Endpoints is deprecated in v1.33+"), and ssh merges stderr
	// into stdout, so the warning would prefix the JSON and every parse would
	// fail on a perfectly healthy cluster.
	kubectl := func(args string) string { return base(args) + " 2>/dev/null" }

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
		NodeAlias:        map[string]string{},
		ServerNodes:      g.ServerNodes,
		Unreachable:      g.Unreachable,

		HostLayerUnavailable: g.NoHostLayer,
	}

	// --- cluster layer via kubectl ---
	nodesJSON, code, err := g.Server.Run(kubectl(`get nodes -o json`))
	if err != nil || code != 0 || !looksLikeJSON(nodesJSON) {
		// Re-run with stderr attached purely to report *why*; the first call
		// keeps stderr off so a warning cannot masquerade as a failure.
		detail, _, _ := g.Server.Run(base(`get nodes -o json`) + " 2>&1")
		detail = firstLine(strings.TrimSpace(detail))
		if detail == "" || looksLikeJSON(detail) {
			detail = "no output"
		}
		e.KubeconfigError = fmt.Sprintf("kubectl get nodes failed (exit %d): %s", code, detail)
	} else {
		g.collectCluster(&e, kubectl, nodesJSON)
	}

	// These do not depend on `get nodes` having worked. Certificate expiry and
	// lost etcd quorum are among the reasons the API server stops answering,
	// so gating them on a healthy API server would make them unreachable in
	// exactly the situations they exist to diagnose.
	g.collectCerts(&e)
	g.collectEtcd(&e, kubectl)
	g.collectCoreDNS(&e, kubectl)

	// --- host layer via SSH ---
	for name, exec := range g.Hosts {
		// probe the correct unit for the node's role: agents run "k3s-agent",
		// servers run "k3s". Pick the unit that actually exists on this node.
		// `systemctl is-active` prints "inactive" for a unit that was never
		// installed, so a not-installed node used to be diagnosed as "restart
		// k3s". Ask which unit exists first, and report "" when none does.
		//
		// Presence is read from the filesystem and the state is asked with and
		// without sudo, because neither query works everywhere: a host without
		// passwordless sudo answers only the unprivileged one, and a login
		// that cannot reach the systemd bus answers only the privileged one.
		// Getting this wrong is not cosmetic — it reported "k3s is not
		// installed" for a node whose k3s was up, and hid a stopped agent.
		unit := ""
		for _, candidate := range []string{"k3s-agent", "k3s"} {
			if check.UnitPresent(exec, candidate) {
				unit = candidate
				break
			}
		}
		if unit == "" {
			e.K3sService[name] = "" // not installed
		} else if s, problem := check.ServiceState(exec, unit); s == "" {
			// Could not be determined: record nothing rather than inventing a
			// fault, but say that a probe failed so the diagnosis is not
			// mistaken for a clean bill of health.
			e.probeFailed("k3s-service:"+name, problem)
			e.K3sService[name] = ""
		} else {
			e.K3sService[name] = s
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
		// The Kubernetes node name is the host's hostname, which is rarely the
		// name the targets file uses ("sandbox-agent2" vs "agent2"). Record the
		// mapping so cluster-layer evidence can be matched to host-layer
		// evidence about the same machine.
		if hn, code, err := exec.Run(`hostname`); err == nil && code == 0 {
			if h := strings.TrimSpace(hn); h != "" {
				e.NodeAlias[h] = name
			}
		}
	}
	return e
}

// isServiceState reports whether out is something `systemctl is-active`
// actually prints, as opposed to an error from the shell.
func isServiceState(out string) bool { return check.IsServiceState(out) }

// runtimeState reports the container runtime unit's state. k3s embeds
// containerd inside its own unit, so an absent containerd.service is normal
// there and reported as "" (no evidence) rather than a fault.
func runtimeState(exec ssh.Executor) (string, bool) {
	for _, unit := range []string{"containerd", "cri-o", "docker"} {
		if !check.UnitPresent(exec, unit) {
			continue
		}
		state, _ := check.ServiceState(exec, unit)
		if state == "" {
			return "", false
		}
		return unit + "=" + state, true
	}
	return "", false
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

func (g Gatherer) collectCluster(e *Evidence, kubectl func(string) string, nodesJSON string) {
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
		if !parsePods(pods, e) {
			e.probeFailed("pods", "output could not be parsed")
		}
	} else {
		e.probeFailed("pods", fmt.Sprintf("kubectl get pods failed (exit %d)", code))
	}
	// events linger ~1h after pod deletion; events for dead pods are stale
	// evidence that would fire signatures on a healthy cluster.
	if evs, code, err := g.Server.Run(kubectl(`get events -A -o json`)); err == nil && code == 0 {
		if !parseEvents(evs, e) {
			e.probeFailed("events", "output could not be parsed")
		}
	} else {
		e.probeFailed("events", fmt.Sprintf("kubectl get events failed (exit %d)", code))
	}
	filterStalePodEvidence(e)
	if pvcs, code, err := g.Server.Run(kubectl(`get pvc -A -o json`)); err == nil && code == 0 {
		if parsePVCs(pvcs, e) {
			filterStalePVCEvidence(e)
		} else {
			e.probeFailed("pvcs", "output could not be parsed")
		}
	} else {
		e.probeFailed("pvcs", fmt.Sprintf("kubectl get pvc failed (exit %d)", code))
	}
	// endpoints back every Service; a Service with none is a silent outage
	// that no pod-level signature reports.
	if eps, code, err := g.Server.Run(kubectl(`get endpoints -A -o json`)); err == nil && code == 0 {
		if !parseEndpoints(eps, e, g.serviceSelectors(e, kubectl)) && len(e.ProbeErrors["services"]) == 0 {
			e.probeFailed("endpoints", "output could not be parsed")
		}
	} else {
		e.probeFailed("endpoints", fmt.Sprintf("kubectl get endpoints failed (exit %d)", code))
	}
}

// collectCoreDNS records whether cluster DNS has ready replicas. Everything
// in the cluster resolves through it, so it is worth its own signature
// rather than being buried in a generic "pod not ready" finding.
func (g Gatherer) collectCoreDNS(e *Evidence, kubectl func(string) string) {
	out, code, err := g.Server.Run(kubectl(`get deployment coredns -n kube-system -o jsonpath={.status.readyReplicas}/{.spec.replicas}`))
	if err != nil || code != 0 {
		e.probeFailed("coredns", fmt.Sprintf("kubectl get deployment coredns failed (exit %d)", code))
		return
	}
	ready, desired, ok := parseReadyRatio(out)
	if !ok {
		e.probeFailed("coredns", "unparseable ready/desired ratio")
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
	// Only k3s ships this command. Probing for it on a kubeadm node would
	// record a permanent "could not gather" on an otherwise healthy cluster.
	if _, code, err := g.Server.Run(`command -v k3s >/dev/null 2>&1`); err != nil || code != 0 {
		return
	}
	out, code, err := g.Server.Run(`sudo -n k3s certificate check 2>&1`)
	if err != nil || code != 0 {
		// k3s is present but the check did not run — usually no passwordless
		// sudo. Record it so "no certificate finding" is not mistaken for
		// "certificates fine".
		e.probeFailed("certificates", "`k3s certificate check` did not run on this node")
		return
	}
	e.CertExpiryDays, e.CertSubject = parseCertExpiry(out, time.Now())
	if e.CertSubject == "" {
		e.probeFailed("certificates", "no expiry dates found in `k3s certificate check` output")
	}
}

// collectEtcd checks etcd quorum when the cluster runs embedded etcd. On a
// single-server k3s (sqlite backend) there is no etcd and this is skipped.
func (g Gatherer) collectEtcd(e *Evidence, kubectl func(string) string) {
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
	// -1 means "not measured". Leaving the zero value here made an unreadable
	// `free` look like a node with no memory left, firing MemoryPressure on a
	// healthy host.
	m.AvailMemMB = -1
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

// looksLikeJSON guards against a kubectl arm that printed an error to stdout:
// parsing that as a node list would report an empty, healthy-looking cluster.
func looksLikeJSON(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")
}

// serviceSelectors returns namespace/name of Services that actually select
// pods. A headless or selector-less Service legitimately has no endpoints and
// must not be reported as broken.
func (g Gatherer) serviceSelectors(e *Evidence, kubectl func(string) string) map[string]bool {
	out := map[string]bool{}
	svcs, code, err := g.Server.Run(kubectl(`get services -A -o json`))
	if err != nil || code != 0 {
		// Unknown: parseEndpoints stays silent rather than guessing — but say
		// so, or a Service with no backends goes unreported and unexplained.
		e.probeFailed("services", fmt.Sprintf("kubectl get services failed (exit %d)", code))
		return nil
	}
	for _, key := range parseSelectingServices(svcs) {
		out[key] = true
	}
	return out
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

// filterStalePodEvidence removes event evidence that no longer describes the
// cluster: events for pods that have been deleted, and events for pods that
// have since become healthy.
//
// Events outlive the condition they describe by about an hour. Without this, a
// pod that was briefly unschedulable during cluster startup keeps producing a
// "pods unschedulable" finding long after it is running normally.
// (events linger ~1h after deletion). LivePods is populated by parsePods.
func filterStalePodEvidence(e *Evidence) {
	if e.livePods == nil {
		return // pod list unavailable; can't judge staleness
	}
	for key := range e.PodEvents {
		if !e.livePods[key] || e.settledPods[key] {
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
