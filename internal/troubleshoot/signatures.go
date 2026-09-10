// Package troubleshoot maps gathered evidence to known failure signatures
// and produces ranked diagnoses with remediation.
package troubleshoot

import (
	"sort"
	"strings"
	"time"
)

// Signature is a known failure pattern.
type Signature struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Match returns confidence 0-100 given the evidence bundle.
	Match       func(e Evidence) int
	Remediation string   `json:"remediation"`
	References  []string `json:"references,omitempty"`
}

// Evidence is everything gathered about a misbehaving system.
type Evidence struct {
	// PodEvents: namespace/pod → event messages
	PodEvents map[string][]string
	// PodStatuses: namespace/pod → status (e.g. "CrashLoopBackOff")
	PodStatuses map[string]string
	// ContainerStates: namespace/pod/container → state reason (e.g. "OOMKilled")
	ContainerStates map[string]string
	// NodeConditions: node → list of active condition types (DiskPressure etc.)
	NodeConditions map[string][]string
	// NodeNotReady: nodes reporting NotReady
	NodeNotReady []string
	// NotReadyPods: namespace/pod of pods that are Running but have containers
	// that never became ready.
	NotReadyPods []string
	// PodRestarts: namespace/pod → total container restarts, for pods that are
	// Running and not ready.
	//
	// A crashlooping container only reads as "CrashLoopBackOff" while it is
	// waiting between attempts; the moment it starts again the status is
	// Running, and a diagnosis taken then sees an unready pod and no reason.
	// The restart count is the durable evidence, so it is kept.
	PodRestarts map[string]int
	// K3sService: node → systemd state ("active", "failed", "inactive", ""=not found)
	K3sService map[string]string
	// HostMetrics: node → {disk used %, availMemMB}
	HostMetrics map[string]HostMetric
	// PVCEvents: pvc → event messages
	PVCEvents map[string][]string
	// KubeconfigError: error text if kubeconfig/auth is broken
	KubeconfigError string
	// Unreachable: nodes that could not be contacted at all. Absence of
	// evidence from a node is itself a finding, never a reason to stay silent.
	Unreachable []UnreachableNode
	// CoreDNS: ready/desired replicas of the cluster DNS deployment (nil when
	// the deployment could not be read).
	CoreDNS *ReadyRatio
	// Etcd: ready/total embedded-etcd member nodes (nil on sqlite-backed k3s).
	Etcd *ReadyRatio
	// CertExpiryDays: days until the soonest k3s certificate expires, with its
	// subject. Zero days with an empty subject means "no evidence gathered".
	CertExpiryDays int
	CertSubject    string
	// EmptyEndpoints: namespace/name of Services whose endpoints have no
	// ready addresses — reachable in DNS, but nothing behind them.
	EmptyEndpoints []string
	// ContainerRuntime: node → "unit=state" for the container runtime, e.g.
	// "containerd=active". Absent when the distribution embeds its runtime.
	ContainerRuntime map[string]string
	// ClockSkew: node → absolute clock offset from the machine running
	// k3helper.
	ClockSkew map[string]time.Duration
	// ServerNodes: targets-file names of the control-plane nodes.
	ServerNodes []string
	// NodeAlias maps a Kubernetes node name to the targets-file node name for
	// the same machine. The two differ in practice ("sandbox-agent2" vs
	// "agent2"), and matching cluster evidence to host evidence needs it.
	NodeAlias map[string]string
	// ProbeErrors: what could not be collected, and why. An absent finding is
	// only good news if we actually looked, so these are reported rather than
	// silently narrowing the diagnosis.
	ProbeErrors map[string]string
	// HostLayerUnavailable: this cluster is reached through a kubeconfig, so
	// there is no host layer to gather — no disks, no systemd units, no
	// container runtime state.
	//
	// Deliberately not a ProbeError and deliberately not an Unreachable node.
	// Both of those mean "we tried and failed", which is a fault. This means
	// "there was never anything there to try", which is how managed clusters
	// work and is not a fault. Conflating them would report every healthy EKS
	// cluster as degraded.
	HostLayerUnavailable bool

	// livePVCs: namespace/pvc keys of PVCs that currently exist.
	livePVCs map[string]bool
	// settledPods: pods that are currently running-and-ready or completed.
	// Their older events describe conditions they have since recovered from —
	// a scheduling complaint from cluster startup is history, not a fault.
	settledPods map[string]bool
	// livePods: namespace/pod keys of pods that currently exist.
	// Populated by parsePods; used to drop stale evidence for deleted pods.
	// Unexported: internal to the gather/parse pipeline.
	livePods map[string]bool
}

// restartsMeaningCrashLoop is how many restarts an unready pod needs before it
// is called a crash loop rather than a pod that had a bad start. kubelet's
// backoff reaches 40s by the third restart, so a pod at this count has been
// failing for the best part of a minute.
const restartsMeaningCrashLoop = 3

// HostMetric is a node's host-level vitals.
type HostMetric struct {
	DiskUsedPercent int
	AvailMemMB      int
	// Images is the number of container images cached on the node.
	Images int
}

// UnreachableNode is a targets-file node that could not be contacted.
type UnreachableNode struct {
	Name   string
	Reason string
}

// serviceStateFor resolves the Kubernetes service state for a node named as
// the cluster names it, translating through NodeAlias when the targets file
// calls the same machine something else.
func (e Evidence) serviceStateFor(k8sNode string) (string, bool) {
	if state, ok := e.K3sService[k8sNode]; ok {
		return state, true
	}
	if alias, ok := e.NodeAlias[k8sNode]; ok {
		state, ok := e.K3sService[alias]
		return state, ok
	}
	return "", false
}

// probeFailed records that a piece of evidence could not be gathered.
func (e *Evidence) probeFailed(name, reason string) {
	if e.ProbeErrors == nil {
		e.ProbeErrors = map[string]string{}
	}
	e.ProbeErrors[name] = reason
}

// ReadyRatio is a ready-out-of-desired count for a replicated component.
type ReadyRatio struct {
	Ready   int
	Desired int
}

// Diagnosis is a ranked possible root cause.
type Diagnosis struct {
	SignatureID string `json:"signature_id"`
	Title       string `json:"title"`
	Confidence  int    `json:"confidence"`
	Remediation string `json:"remediation"`
	Evidence    string `json:"evidence"`
}

// Informational reports whether this finding describes the scope of the
// diagnosis rather than a fault in the cluster.
//
// These must not set doctor's exit code. An RBAC-scoped kubeconfig or a
// managed cluster with no host layer would otherwise fail every CI run while
// being perfectly healthy.
func (d Diagnosis) Informational() bool {
	switch d.SignatureID {
	case "cluster.partial-evidence", "cluster.host-layer-unavailable":
		return true
	}
	return false
}

// OnlyInformational reports whether nothing but scope notes were found.
func OnlyInformational(ds []Diagnosis) bool {
	for _, d := range ds {
		if !d.Informational() {
			return false
		}
	}
	return true
}

// pullFailures counts image-pull failures by what the registry actually said.
type pullFailures struct {
	auth        int
	cert        int
	unreachable int
}

// classifyPullFailures reads the reason out of image-pull events.
//
// "ImagePullBackOff" is a symptom shared by a wrong tag, a missing credential,
// an untrusted CA and a registry nobody can reach — four different fixes. The
// kubelet records the underlying error in the event text, so the distinction
// is available and worth making: a single generic finding sends people to
// check credentials when the name simply did not resolve.
func classifyPullFailures(e Evidence) pullFailures {
	var f pullFailures
	for _, evs := range e.PodEvents {
		for _, ev := range evs {
			if !strings.Contains(ev, "Failed to pull image") &&
				!strings.Contains(ev, "ErrImagePull") &&
				!strings.Contains(ev, "ImagePullBackOff") {
				continue
			}
			low := strings.ToLower(ev)
			switch {
			// Order matters: an auth failure over a bad certificate is
			// reported as a certificate error, and fixing the credential
			// would not help.
			case containsAny(low, "x509", "certificate signed by unknown authority",
				"certificate has expired", "tls: failed to verify",
				"failed to verify certificate", "server gave http response to https client",
				// containerd's own message when registries.yaml names a
				// ca_file that is not on the node — seen against a real
				// cluster, and not something any of the TLS phrases above
				// would have matched.
				"unable to read ca cert", "failed to load ca"):
				f.cert++
			case containsAny(low, "401 unauthorized", "unauthorized", "authentication required",
				"pull access denied", "denied: requested access to the resource is denied",
				"403 forbidden"):
				f.auth++
			case containsAny(low, "no such host", "dial tcp", "connection refused",
				"i/o timeout", "network is unreachable", "temporary failure in name resolution",
				"context deadline exceeded"):
				f.unreachable++
			}
		}
	}
	return f
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Diagnose runs all signatures against the evidence and returns
// ranked diagnoses (highest confidence first, 0-confidence dropped).
func Diagnose(e Evidence) []Diagnosis {
	var out []Diagnosis
	for _, sig := range registry {
		conf := sig.Match(e)
		if conf > 0 {
			out = append(out, Diagnosis{
				SignatureID: sig.ID,
				Title:       sig.Title,
				Confidence:  conf,
				Remediation: sig.Remediation,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Confidence > out[j].Confidence })
	return out
}

var registry = []Signature{
	{
		ID:    "cluster.partial-evidence",
		Title: "Some evidence could not be gathered — this diagnosis is incomplete",
		Match: func(e Evidence) int {
			if len(e.ProbeErrors) == 0 {
				return 0
			}
			// Deliberately low: this does not compete with a real finding, it
			// exists so "nothing found" is never mistaken for "nothing wrong".
			return 30
		},
		Remediation: "One or more probes failed, so faults they would have caught cannot be ruled out. Check that the credentials in use have list access to pods, events, PVCs, endpoints and services, and that the API server is responsive.",
	},
	{
		ID:    "cluster.host-layer-unavailable",
		Title: "Host layer not visible — findings cover the cluster only",
		Match: func(e Evidence) int {
			if !e.HostLayerUnavailable {
				return 0
			}
			// Low, like cluster.partial-evidence: this is a note on the scope
			// of the diagnosis, not a fault competing with real findings.
			return 20
		},
		Remediation: "This cluster is reached through a kubeconfig, so k3helper can see the API server but not the machines behind it. Disk pressure, swap, cgroups, systemd units, the container runtime and certificate expiry are not checked. For a self-managed cluster, describe its nodes in the targets file to get the host layer as well; for a managed cluster, that layer belongs to the provider.",
	},
	{
		ID:    "node.unreachable",
		Title: "Node unreachable over SSH (no evidence could be gathered)",
		Match: func(e Evidence) int {
			if len(e.Unreachable) == 0 {
				return 0
			}
			// A node we cannot reach may hide any other fault, so this
			// outranks the signatures that depend on host evidence.
			return min(95, 75+len(e.Unreachable)*10)
		},
		Remediation: "Confirm the node is powered on and reachable (`ping`), that sshd is running, and that the host/port/user/key in the targets file are correct. Until it responds, no host-level diagnosis is possible for that node.",
	},
	{
		ID:    "node.notready",
		Title: "Node is NotReady while its Kubernetes service is running",
		Match: func(e Evidence) int {
			// Only the nodes whose service is up: a stopped service is
			// node.notready-k3s-down's job, and reporting both would send the
			// user to two places for one fault.
			n := 0
			for _, node := range e.NodeNotReady {
				state, seen := e.serviceStateFor(node)
				if !seen || state == "active" {
					n++
				}
			}
			if n == 0 {
				return 0
			}
			// A NotReady node stops taking work and gets its pods evicted; it
			// is the most common serious fault in a cluster and was previously
			// gathered but never diagnosed.
			return min(90, 70+n*10)
		},
		Remediation: "The service is up but the node is not Ready, so the kubelet is unhealthy rather than absent. Check `kubectl describe node <name>` for the failing condition, then on the node: `sudo journalctl -u k3s-agent -n 100 --no-pager` (or `-u kubelet`). Common causes: the container runtime is wedged, the CNI is not configured, disk or memory pressure, or the node cannot reach the API server.",
	},
	{
		ID:    "pod.not-ready",
		Title: "Pods running but never becoming ready",
		Match: func(e Evidence) int {
			n := len(e.NotReadyPods)
			if n == 0 {
				return 0
			}
			// A pod that runs but never passes readiness serves no traffic,
			// yet has no waiting or terminated reason to report.
			return min(80, 45+n*10)
		},
		Remediation: "The container is running but its readiness probe never passes, so it receives no traffic. Check the probe and the app: `kubectl describe pod <pod>` for the probe failure, then `kubectl logs <pod>`. Common causes: the probe path or port is wrong, the app takes longer to start than initialDelaySeconds allows, or it is waiting on a dependency.",
	},
	{
		ID:    "node.runtime-down",
		Title: "Container runtime is not running",
		Match: func(e Evidence) int {
			n := 0
			for node, state := range e.ContainerRuntime {
				_, status, ok := strings.Cut(state, "=")
				if !ok || status == "" || status == "active" {
					continue
				}
				// k3s embeds its own containerd and never starts the unit, so
				// a leftover stopped containerd.service from a previous
				// kubeadm install is not a fault. Only count a dead runtime on
				// a node whose Kubernetes service is itself unhappy.
				if k3s, seen := e.K3sService[node]; seen && k3s == "active" {
					continue
				}
				n++
			}
			if n == 0 {
				return 0
			}
			// Without a runtime the kubelet cannot start a single container,
			// so this explains almost any workload symptom on that node.
			return min(95, 80+n*10)
		},
		Remediation: "Restart the runtime on the affected node (`sudo systemctl restart containerd`), then check `sudo journalctl -u containerd -n 100 --no-pager`. A runtime that will not start is often out of disk or has a corrupt state directory under /var/lib/containerd.",
	},
	{
		ID:    "node.clock-skew",
		Title: "Node clock is out of sync",
		Match: func(e Evidence) int {
			worst := time.Duration(0)
			for _, skew := range e.ClockSkew {
				if skew > worst {
					worst = skew
				}
			}
			switch {
			case worst >= 5*time.Minute:
				// Beyond this, TLS handshakes and token validation fail
				// outright and the symptoms look like anything but a clock.
				return 90
			case worst >= 60*time.Second:
				return 55
			}
			return 0
		},
		Remediation: "Enable time sync on the node: `sudo timedatectl set-ntp true` (or install chrony/systemd-timesyncd), then confirm with `timedatectl status`. Certificates, service-account tokens and etcd leases are all time-sensitive, so a skewed clock surfaces as TLS and auth failures.",
	},
	{
		ID:    "node.image-bloat",
		Title: "Disk filling up with cached container images",
		Match: func(e Evidence) int {
			n := 0
			for _, m := range e.HostMetrics {
				// Only meaningful once the disk is actually under pressure:
				// a large image cache on a half-empty disk is not a problem.
				if m.DiskUsedPercent >= 85 && m.Images >= 40 {
					n++
				}
			}
			if n == 0 {
				return 0
			}
			return min(75, 45+n*15)
		},
		Remediation: "Prune unused images on the node: `sudo k3s crictl rmi --prune` (or `sudo crictl rmi --prune`). If it refills, lower the kubelet image GC thresholds (--image-gc-high-threshold/--image-gc-low-threshold) or give the node a bigger disk.",
	},
	{
		ID:    "cluster.etcd-quorum",
		Title: "Embedded etcd has lost or is about to lose quorum",
		Match: func(e Evidence) int {
			// Preferred source: the API server's own view of the members.
			if e.Etcd != nil && e.Etcd.Desired > 0 {
				quorum := e.Etcd.Desired/2 + 1
				switch {
				case e.Etcd.Ready < quorum:
					return 95 // below quorum: read-only, or down entirely
				case e.Etcd.Ready < e.Etcd.Desired:
					return 70 // quorate, but one more failure ends the cluster
				}
				return 0
			}
			// Losing quorum is exactly what stops the API server answering, so
			// the preferred source is unavailable in the case that matters
			// most. Fall back to the host layer: how many control-plane nodes
			// have a running service.
			if len(e.ServerNodes) < 3 {
				return 0 // single server, or too few to have had quorum
			}
			down := 0
			for _, n := range e.ServerNodes {
				if state, seen := e.K3sService[n]; seen && state != "active" && state != "" {
					down++
					continue
				}
				for _, u := range e.Unreachable {
					if u.Name == n {
						down++
					}
				}
			}
			if down == 0 {
				return 0
			}
			quorum := len(e.ServerNodes)/2 + 1
			if len(e.ServerNodes)-down < quorum {
				return 95
			}
			return 70
		},
		Remediation: "Check each etcd server node: `sudo systemctl status k3s` and `sudo k3s etcd-snapshot ls`. Restore a failed member by restarting k3s on it; if a member is permanently gone, remove it with `k3s server --cluster-reset` on a surviving server (snapshot first). An even number of servers gains no quorum — run 3 or 5.",
	},
	{
		ID:    "cluster.cert-expiry",
		Title: "k3s TLS certificates expiring soon",
		Match: func(e Evidence) int {
			if e.CertSubject == "" {
				return 0 // no cert evidence gathered
			}
			switch {
			case e.CertExpiryDays < 0:
				return 95 // already expired: the API server is rejecting clients
			case e.CertExpiryDays <= 7:
				return 90
			case e.CertExpiryDays <= 30:
				return 60
			}
			return 0
		},
		Remediation: "k3s rotates its certificates on restart when they are within 90 days of expiry: `sudo systemctl restart k3s` on each server, then restart agents. Verify with `sudo k3s certificate check`. If they already expired, the same restart still rotates them, but client kubeconfigs must be re-fetched afterwards.",
	},
	{
		ID:    "network.coredns",
		Title: "CoreDNS has no ready replicas (cluster DNS is down)",
		Match: func(e Evidence) int {
			// nil means the deployment could not be read at all (no evidence).
			// A deployment that exists with zero desired replicas is a real
			// outage — someone scaled DNS to nothing — not a reason to skip.
			if e.CoreDNS == nil {
				return 0
			}
			if e.CoreDNS.Ready == 0 {
				// Nothing in the cluster can resolve a Service name. But when
				// a host-level fault is visible, that fault is why CoreDNS is
				// down — rank under it so the user fixes the cause.
				// 40 keeps it visible but below the causes it follows from
				// (a stopped service scores 45 for a single node).
				const consequence = 40
				for _, state := range e.K3sService {
					if state != "active" && state != "" {
						return consequence
					}
				}
				for _, conds := range e.NodeConditions {
					if contains(conds, "DiskPressure") || contains(conds, "MemoryPressure") {
						return consequence
					}
				}
				if len(e.NodeNotReady) > 0 || len(e.Unreachable) > 0 {
					return consequence
				}
				return 95
			}
			if e.CoreDNS.Ready < e.CoreDNS.Desired {
				return 55
			}
			return 0
		},
		Remediation: "Inspect the pods: `kubectl -n kube-system get pods -l k8s-app=kube-dns` and `kubectl -n kube-system logs -l k8s-app=kube-dns`. Common causes: the node hosting CoreDNS is NotReady, insufficient memory, or a broken /etc/resolv.conf on the host causing a forwarding loop. On k3s, CoreDNS is redeployed from the bundled manifest if you delete the deployment.",
	},
	{
		ID:    "network.empty-endpoints",
		Title: "Services with no ready endpoints (nothing is serving them)",
		Match: func(e Evidence) int {
			n := len(e.EmptyEndpoints)
			if n == 0 {
				return 0
			}
			// Empty endpoints are usually a *consequence*: the backing pods
			// are crashlooping or unschedulable. Rank below those causes so
			// the user is sent to the pod, not to selector debugging.
			base := min(60, 25+n*10)
			if len(e.NotReadyPods) > 0 {
				return base // pods exist but are not ready; that is the cause
			}
			for _, st := range e.PodStatuses {
				if st != "" {
					return base // a pod-level cause is visible; stay under it
				}
			}
			// Nothing else explains it: the selector really may be wrong.
			return min(75, 40+n*10)
		},
		Remediation: "The Service selector matches no ready pod. Compare them: `kubectl get svc <svc> -o wide` and `kubectl get pods -l <selector>`. Usual causes: a selector that does not match the pod labels, pods failing their readiness probe, or all backing pods being down — check the pod-level findings above first.",
	},
	{
		ID:    "registry.auth",
		Title: "Registry refused the credentials (image pull unauthorized)",
		Match: func(e Evidence) int {
			n := classifyPullFailures(e).auth
			if n == 0 {
				return 0
			}
			return min(92, 82+n*5)
		},
		Remediation: "The registry answered, and rejected who we are. Either the node has no credentials for it or they are wrong. " +
			"Cluster-wide: put the registry in the targets file under `registries:` with a username and `password_env`, then " +
			"`k3helper registry apply -t targets.yaml` (it restarts k3s, which does not re-read the file on its own). " +
			"Per workload: `k3helper gen secret <name> --docker-registry <host> --registry-user <u> --registry-password <p>`, apply it, " +
			"and reference it from the pod's imagePullSecrets. Verify on the node with `sudo crictl pull <image>`.",
	},
	{
		ID:    "registry.cert",
		Title: "Registry TLS certificate not trusted by the node",
		Match: func(e Evidence) int {
			n := classifyPullFailures(e).cert
			if n == 0 {
				return 0
			}
			return min(90, 80+n*5)
		},
		Remediation: "Either the certificate is not trusted — an internal CA, a self-signed certificate, or a registry serving plain " +
			"HTTP on an https endpoint — or the `ca_file` in registries.yaml names a file that is not on this node. " +
			"`k3helper check` reports the second case directly. Right fix: copy the CA to every node and set `ca_file:` on the " +
			"registry in the targets file, then `k3helper registry apply`. For a throwaway registry only, `insecure_skip_verify: true`. " +
			"If the registry speaks HTTP, set `endpoint: http://<host>` instead of turning verification off.",
	},
	{
		ID:    "registry.unreachable",
		Title: "Registry could not be reached from the node (DNS or connection failure)",
		Match: func(e Evidence) int {
			n := classifyPullFailures(e).unreachable
			if n == 0 {
				return 0
			}
			return min(88, 78+n*5)
		},
		Remediation: "The pull never got as far as an answer: the name did not resolve, or nothing accepted the connection. " +
			"Check it from the node itself, not from your machine — `getent hosts <registry>` and " +
			"`curl -sSv https://<registry>/v2/ -o /dev/null`. Usual causes: the registry is reachable from your network and not " +
			"from the nodes', a firewall between them, or a mirror endpoint pointing somewhere that no longer exists.",
	},
	{
		ID:    "pod.imagepull",
		Title: "Pods failing to pull images (ImagePullBackOff / ErrImagePull)",
		Match: func(e Evidence) int {
			n := 0
			for _, evs := range e.PodEvents {
				for _, ev := range evs {
					if strings.Contains(ev, "Failed to pull image") || strings.Contains(ev, "ImagePullBackOff") || strings.Contains(ev, "ErrImagePull") {
						n++
					}
				}
			}
			for _, st := range e.PodStatuses {
				if st == "ImagePullBackOff" || st == "ErrImagePull" {
					n++
				}
			}
			if n == 0 {
				return 0
			}
			// When the registry said *why*, that finding is the cause and this
			// one is the symptom. Ranking the symptom above it is the mistake
			// that sent operators to check a kubeconfig that was fine.
			if f := classifyPullFailures(e); f.auth+f.cert+f.unreachable > 0 {
				return 55
			}
			return min(95, 80+n*15)
		},
		Remediation: "Check the image name and tag first — a typo and a missing tag look identical to a permission problem from here. " +
			"Then, for a private registry, check credentials (`k3helper registry apply`, or imagePullSecrets on the workload). " +
			"Test the pull on the node: `sudo crictl pull <image>`.",
	},
	{
		ID:    "pod.crashloop",
		Title: "Container crashing on start (CrashLoopBackOff)",
		Match: func(e Evidence) int {
			crashing := map[string]bool{}
			for pod, st := range e.PodStatuses {
				if st == "CrashLoopBackOff" {
					crashing[pod] = true
				}
			}
			// A container in backoff is only reported as CrashLoopBackOff while
			// it is waiting; between attempts it is Running with no reason
			// attached, and a diagnosis taken in that window used to see
			// nothing but an unready pod. Restarts are what does not go away:
			// a pod that is not ready and has restarted repeatedly is
			// crashlooping whichever half of the cycle we happened to catch.
			//
			// The threshold is deliberately not 1. A single restart is a pod
			// that fell over once and came back, which is not this finding.
			for _, pod := range e.NotReadyPods {
				if e.PodRestarts[pod] >= restartsMeaningCrashLoop {
					crashing[pod] = true
				}
			}
			return min(90, len(crashing)*40)
		},
		Remediation: "Read the container logs: `kubectl logs <pod> --previous`. Typical causes: bad command/args, missing config/env, app failing at startup. Fix the cause — do NOT use restart policies to mask it.",
	},
	{
		ID:    "pod.oom",
		Title: "Containers being OOMKilled (memory limit too low)",
		Match: func(e Evidence) int {
			n := 0
			for _, st := range e.ContainerStates {
				if st == "OOMKilled" {
					n++
				}
			}
			if n == 0 {
				return 0
			}
			// OOMKilled is an explicit kubelet verdict — high confidence on
			// first occurrence
			return min(95, 80+n*15)
		},
		Remediation: "Raise memory limits in the pod spec (resources.limits.memory), or find a memory leak in the app. Note: node-level OOM may also mean the node itself is out of memory.",
	},
	{
		ID:    "pod.pending-sched",
		Title: "Pods unschedulable (insufficient resources / taints / cordoned nodes)",
		Match: func(e Evidence) int {
			n := 0
			for _, evs := range e.PodEvents {
				for _, ev := range evs {
					if matchesAny(ev, unschedulableReasons) {
						n++
					}
				}
			}
			return min(90, n*40)
		},
		Remediation: "Lower resource requests, add node capacity, or add tolerations/nodeSelector. If nodes were cordoned: `kubectl uncordon <node>`.",
	},
	{
		ID:    "node.diskpressure",
		Title: "Node under DiskPressure (disk nearly full)",
		Match: func(e Evidence) int {
			n := 0
			for _, conds := range e.NodeConditions {
				if contains(conds, "DiskPressure") {
					n++
				}
			}
			for _, m := range e.HostMetrics {
				if m.DiskUsedPercent >= 95 {
					n++
				}
			}
			return min(95, n*45)
		},
		Remediation: "Free node disk: prune unused images (`sudo crictl rmi --prune`), clean /var/log, enlarge the volume. k3s evicts pods when DiskPressure is active.",
	},
	{
		ID:    "node.notready-k3s-down",
		Title: "Node NotReady because k3s service is down",
		Match: func(e Evidence) int {
			n := 0
			for _, state := range e.K3sService {
				if state != "active" && state != "" {
					n++
				}
			}
			if n == 0 {
				return 0
			}
			if len(e.NodeNotReady) > 0 {
				n++ // correlation bonus
			}
			// When the API server is also unreachable we cannot see NotReady
			// at all, so the correlation bonus can never arrive — yet a
			// stopped service is precisely the likely cause. Score it as one.
			if e.KubeconfigError != "" {
				return 90
			}
			return min(95, n*45)
		},
		Remediation: "On the affected node: `sudo systemctl restart k3s` (or k3s-agent), then check `sudo journalctl -u k3s -n 100 --no-pager` for the underlying cause.",
	},
	{
		ID:    "node.memorypressure",
		Title: "Node under MemoryPressure",
		Match: func(e Evidence) int {
			n := 0
			for _, conds := range e.NodeConditions {
				if contains(conds, "MemoryPressure") {
					n++
				}
			}
			for _, m := range e.HostMetrics {
				// -1 means the reading failed; only a real measurement counts.
				if m.AvailMemMB >= 0 && m.AvailMemMB < 200 {
					n++
				}
			}
			return min(90, n*45)
		},
		Remediation: "Reduce workloads or add memory. Look for pods without memory limits that may be hogging RAM: `kubectl top pods --all-namespaces`.",
	},
	{
		ID:    "storage.pvc-pending",
		Title: "PVC stuck Pending (no storage class / CSI driver)",
		Match: func(e Evidence) int {
			n := 0
			for _, evs := range e.PVCEvents {
				for _, ev := range evs {
					if strings.Contains(ev, "no persistent volumes available") || strings.Contains(ev, "waiting for a volume") || strings.Contains(ev, "storageclass") {
						n++
					}
				}
			}
			return min(90, n*45)
		},
		Remediation: "Check the storageClass exists: `kubectl get sc`. On k3s install the local-path provider (bundled by default — if disabled, re-enable) or deploy a CSI driver.",
	},
	{
		ID:    "cluster.kubeconfig",
		Title: "kubeconfig invalid or expired credentials",
		Match: func(e Evidence) int {
			if e.KubeconfigError == "" {
				return 0
			}
			// An unreachable API server is a symptom with many causes. When a
			// host-level cause is visible — k3s stopped, certs expired, etcd
			// without quorum — that cause must rank above this, or the user is
			// sent to check a kubeconfig that is perfectly fine.
			for _, state := range e.K3sService {
				if state != "active" && state != "" {
					return 40
				}
			}
			if e.CertSubject != "" && e.CertExpiryDays <= 0 {
				return 40
			}
			if e.Etcd != nil && e.Etcd.Desired > 0 && e.Etcd.Ready < e.Etcd.Desired/2+1 {
				return 40
			}
			return 85
		},
		Remediation: "Verify KUBECONFIG path and token validity. On k3s: copy /etc/rancher/k3s/k3s.yaml from the server, or run `k3helper vm setup --kubeconfig` to re-fetch.",
	},
	{
		ID:    "pod.evicted",
		Title: "Pods evicted (resource pressure on node)",
		Match: func(e Evidence) int {
			n := 0
			for _, st := range e.PodStatuses {
				if st == "Evicted" {
					n++
				}
			}
			return min(85, n*40)
		},
		Remediation: "Evictions come from node pressure (disk/memory). Fix the node pressure first, then delete evicted pods: `kubectl delete pod <pod>` so the controller recreates them.",
	},
}

// unschedulableReasons are the scheduler's own phrases for "this pod cannot
// be placed". They are matched as substrings of the FailedScheduling event.
//
// Note "were unschedulable" (a cordoned node) and "nodes are available" — the
// latter is preceded by a live count like "0/3", so matching the literal
// "0/N" as this once did never fired at all.
var unschedulableReasons = []string{
	"Insufficient cpu",
	"Insufficient memory",
	"Insufficient ephemeral-storage",
	"had untolerated taint",
	"were unschedulable",
	"nodes are available",
	"didn't match Pod's node affinity",
	"didn't match node selector",
	"had volume node affinity conflict",
}

func matchesAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
