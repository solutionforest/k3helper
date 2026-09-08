// Package troubleshoot maps gathered evidence to known failure signatures
// and produces ranked diagnoses with remediation.
package troubleshoot

import (
	"sort"
	"strings"
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

	// livePods: namespace/pod keys of pods that currently exist.
	// Populated by parsePods; used to drop stale evidence for deleted pods.
	// Unexported: internal to the gather/parse pipeline.
	livePods map[string]bool
}

// HostMetric is a node's host-level vitals.
type HostMetric struct {
	DiskUsedPercent int
	AvailMemMB      int
}

// UnreachableNode is a targets-file node that could not be contacted.
type UnreachableNode struct {
	Name   string
	Reason string
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
		ID:    "cluster.etcd-quorum",
		Title: "Embedded etcd has lost or is about to lose quorum",
		Match: func(e Evidence) int {
			if e.Etcd == nil || e.Etcd.Desired == 0 {
				return 0 // sqlite-backed cluster: no etcd to lose
			}
			quorum := e.Etcd.Desired/2 + 1
			switch {
			case e.Etcd.Ready < quorum:
				// below quorum the API server is read-only or down entirely
				return 95
			case e.Etcd.Ready < e.Etcd.Desired:
				// still quorate, but one more failure ends the cluster
				return 70
			}
			return 0
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
				// nothing in the cluster can resolve a Service name
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
			// A Service resolves fine and still black-holes every request, so
			// this is worth surfacing even for a single occurrence.
			return min(85, 50+n*15)
		},
		Remediation: "The Service selector matches no ready pod. Compare them: `kubectl get svc <svc> -o wide` and `kubectl get pods -l <selector>`. Usual causes: a selector that does not match the pod labels, pods failing their readiness probe, or all backing pods being down — check the pod-level findings above first.",
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
			return min(95, 80+n*15)
		},
		Remediation: "Check image name/tag spelling; if private registry, verify imagePullSecrets (`kubectl create secret docker-registry`) and registry auth; test pull manually: `sudo crictl pull <image>`",
	},
	{
		ID:    "pod.crashloop",
		Title: "Container crashing on start (CrashLoopBackOff)",
		Match: func(e Evidence) int {
			n := 0
			for _, st := range e.PodStatuses {
				if st == "CrashLoopBackOff" {
					n++
				}
			}
			return min(90, n*40)
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
					if strings.Contains(ev, "Insufficient cpu") || strings.Contains(ev, "Insufficient memory") ||
						strings.Contains(ev, "node(s) had untolerated taint") || strings.Contains(ev, "0/N nodes are available") {
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
			if len(e.NodeNotReady) > 0 && n > 0 {
				n++ // correlation bonus
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
				if m.AvailMemMB < 200 {
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
			if e.KubeconfigError != "" {
				return 85
			}
			return 0
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
