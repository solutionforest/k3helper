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
