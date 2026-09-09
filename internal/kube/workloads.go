package kube

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Workload is one row of the deployment / statefulset / daemonset browsers.
//
// The three kinds share a row shape (ready / up-to-date / available) even
// though the API spells the fields differently for each, so one type carries
// all three and the parser normalises.
type Workload struct {
	Namespace string
	Name      string
	Kind      string
	Ready     string // "2/3"
	UpToDate  int
	Available int
	Age       time.Duration
	// Selector is the workload's label selector in kubectl `-l` syntax, so a
	// drill-down can list exactly the pods this workload owns.
	Selector string
	// Images is what the pod template runs, shown in wide mode.
	Images string
}

// Healthy reports whether every desired replica is ready.
func (w Workload) Healthy() bool {
	ready, desired, ok := splitRatio(w.Ready)
	if !ok {
		return false
	}
	// A workload scaled to zero is not broken — it is switched off.
	return ready == desired
}

func splitRatio(s string) (int, int, bool) {
	a, b, found := strings.Cut(s, "/")
	if !found {
		return 0, 0, false
	}
	var ready, desired int
	if _, err := fmt.Sscanf(a, "%d", &ready); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(b, "%d", &desired); err != nil {
		return 0, 0, false
	}
	return ready, desired, true
}

// workloadKinds maps the browser's view names to the resource kubectl wants.
var workloadKinds = map[string]string{
	"deployments":  "deployments",
	"statefulsets": "statefulsets",
	"daemonsets":   "daemonsets",
}

// ListWorkloads returns deployments, statefulsets or daemonsets in ns.
func ListWorkloads(exec Executor, kind, ns string) ([]Workload, error) {
	resource, ok := workloadKinds[kind]
	if !ok {
		return nil, fmt.Errorf("unsupported workload kind %q", kind)
	}
	out, code, err := exec.Run(Builder(exec)("get "+resource+" "+nsFlag(ns)+" -o json") + " 2>/dev/null")
	if err != nil {
		return nil, err
	}
	if code != 0 || strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("kubectl get %s failed (exit %d)", resource, code)
	}
	var raw struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Namespace         string    `json:"namespace"`
				Name              string    `json:"name"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int `json:"replicas"`
				Selector struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"selector"`
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				// Deployment / StatefulSet
				ReadyReplicas     int `json:"readyReplicas"`
				UpdatedReplicas   int `json:"updatedReplicas"`
				AvailableReplicas int `json:"availableReplicas"`
				Replicas          int `json:"replicas"`
				// DaemonSet spells every one of these differently.
				DesiredNumberScheduled int `json:"desiredNumberScheduled"`
				NumberReady            int `json:"numberReady"`
				UpdatedNumberScheduled int `json:"updatedNumberScheduled"`
				NumberAvailable        int `json:"numberAvailable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", resource, err)
	}
	now := time.Now()
	items := make([]Workload, 0, len(raw.Items))
	for _, it := range raw.Items {
		w := Workload{
			Namespace: it.Metadata.Namespace,
			Name:      it.Metadata.Name,
			Kind:      strings.TrimSuffix(resource, "s"),
			Age:       now.Sub(it.Metadata.CreationTimestamp),
			Selector:  renderSelector(it.Spec.Selector.MatchLabels),
		}
		var images []string
		for _, c := range it.Spec.Template.Spec.Containers {
			images = append(images, c.Image)
		}
		w.Images = strings.Join(images, ",")

		if resource == "daemonsets" {
			w.Ready = fmt.Sprintf("%d/%d", it.Status.NumberReady, it.Status.DesiredNumberScheduled)
			w.UpToDate, w.Available = it.Status.UpdatedNumberScheduled, it.Status.NumberAvailable
		} else {
			// spec.replicas is a pointer because it is optional and defaults to
			// 1: treating a missing value as 0 would report a healthy single
			// replica as "1/0".
			desired := 1
			if it.Spec.Replicas != nil {
				desired = *it.Spec.Replicas
			}
			w.Ready = fmt.Sprintf("%d/%d", it.Status.ReadyReplicas, desired)
			w.UpToDate, w.Available = it.Status.UpdatedReplicas, it.Status.AvailableReplicas
		}
		items = append(items, w)
	}
	sortByNamespaceName(items, func(w Workload) (string, string) { return w.Namespace, w.Name })
	return items, nil
}

// Service is one row of the service browser.
type Service struct {
	Namespace  string
	Name       string
	Type       string
	ClusterIP  string
	ExternalIP string
	Ports      string
	Selector   string
	Age        time.Duration
}

// ListServices returns services in ns.
func ListServices(exec Executor, ns string) ([]Service, error) {
	out, code, err := exec.Run(Builder(exec)("get services "+nsFlag(ns)+" -o json") + " 2>/dev/null")
	if err != nil {
		return nil, err
	}
	if code != 0 || strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("kubectl get services failed (exit %d)", code)
	}
	var raw struct {
		Items []struct {
			Metadata struct {
				Namespace         string    `json:"namespace"`
				Name              string    `json:"name"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
			Spec struct {
				Type        string            `json:"type"`
				ClusterIP   string            `json:"clusterIP"`
				ExternalIPs []string          `json:"externalIPs"`
				Selector    map[string]string `json:"selector"`
				Ports       []struct {
					Port     int    `json:"port"`
					NodePort int    `json:"nodePort"`
					Protocol string `json:"protocol"`
				} `json:"ports"`
			} `json:"spec"`
			Status struct {
				LoadBalancer struct {
					Ingress []struct {
						IP       string `json:"ip"`
						Hostname string `json:"hostname"`
					} `json:"ingress"`
				} `json:"loadBalancer"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse services: %w", err)
	}
	now := time.Now()
	svcs := make([]Service, 0, len(raw.Items))
	for _, it := range raw.Items {
		s := Service{
			Namespace: it.Metadata.Namespace,
			Name:      it.Metadata.Name,
			Type:      it.Spec.Type,
			ClusterIP: it.Spec.ClusterIP,
			Selector:  renderSelector(it.Spec.Selector),
			Age:       now.Sub(it.Metadata.CreationTimestamp),
		}
		external := append([]string{}, it.Spec.ExternalIPs...)
		for _, lb := range it.Status.LoadBalancer.Ingress {
			if lb.IP != "" {
				external = append(external, lb.IP)
			} else if lb.Hostname != "" {
				external = append(external, lb.Hostname)
			}
		}
		s.ExternalIP = "<none>"
		if len(external) > 0 {
			s.ExternalIP = strings.Join(external, ",")
		} else if s.Type == "LoadBalancer" {
			// An unfulfilled LoadBalancer is a real state an operator needs to
			// see, not an absence.
			s.ExternalIP = "<pending>"
		}
		var ports []string
		for _, p := range it.Spec.Ports {
			if p.NodePort > 0 {
				ports = append(ports, fmt.Sprintf("%d:%d/%s", p.Port, p.NodePort, p.Protocol))
				continue
			}
			ports = append(ports, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
		}
		s.Ports = strings.Join(ports, ",")
		svcs = append(svcs, s)
	}
	sortByNamespaceName(svcs, func(s Service) (string, string) { return s.Namespace, s.Name })
	return svcs, nil
}

// Ingress is one row of the ingress browser.
type Ingress struct {
	Namespace string
	Name      string
	Class     string
	Hosts     string
	Address   string
	Ports     string
	Age       time.Duration
}

// ListIngresses returns ingresses in ns. A cluster with no ingress API at all
// (or none installed) reports an empty list rather than an error, because
// "there are no ingresses" is the common case on a bare k3s.
func ListIngresses(exec Executor, ns string) ([]Ingress, error) {
	out, code, err := exec.Run(Builder(exec)("get ingresses "+nsFlag(ns)+" -o json") + " 2>/dev/null")
	if err != nil {
		return nil, err
	}
	if code != 0 || strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("kubectl get ingresses failed (exit %d)", code)
	}
	var raw struct {
		Items []struct {
			Metadata struct {
				Namespace         string    `json:"namespace"`
				Name              string    `json:"name"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
			Spec struct {
				IngressClassName string `json:"ingressClassName"`
				TLS              []struct {
					Hosts []string `json:"hosts"`
				} `json:"tls"`
				Rules []struct {
					Host string `json:"host"`
				} `json:"rules"`
			} `json:"spec"`
			Status struct {
				LoadBalancer struct {
					Ingress []struct {
						IP       string `json:"ip"`
						Hostname string `json:"hostname"`
					} `json:"ingress"`
				} `json:"loadBalancer"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse ingresses: %w", err)
	}
	now := time.Now()
	ings := make([]Ingress, 0, len(raw.Items))
	for _, it := range raw.Items {
		i := Ingress{
			Namespace: it.Metadata.Namespace,
			Name:      it.Metadata.Name,
			Class:     it.Spec.IngressClassName,
			Age:       now.Sub(it.Metadata.CreationTimestamp),
			Ports:     "80",
		}
		if i.Class == "" {
			i.Class = "<none>"
		}
		if len(it.Spec.TLS) > 0 {
			i.Ports = "80,443"
		}
		var hosts []string
		for _, r := range it.Spec.Rules {
			if r.Host != "" {
				hosts = append(hosts, r.Host)
			}
		}
		i.Hosts = strings.Join(hosts, ",")
		if i.Hosts == "" {
			i.Hosts = "*"
		}
		var addrs []string
		for _, lb := range it.Status.LoadBalancer.Ingress {
			if lb.IP != "" {
				addrs = append(addrs, lb.IP)
			} else if lb.Hostname != "" {
				addrs = append(addrs, lb.Hostname)
			}
		}
		i.Address = strings.Join(addrs, ",")
		ings = append(ings, i)
	}
	sortByNamespaceName(ings, func(i Ingress) (string, string) { return i.Namespace, i.Name })
	return ings, nil
}

// renderSelector turns matchLabels into kubectl's `-l` syntax, keys sorted so
// the same selector always renders the same way.
func renderSelector(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

// sortByNamespaceName orders any resource list the way kubectl does.
func sortByNamespaceName[T any](items []T, key func(T) (string, string)) {
	sort.Slice(items, func(i, j int) bool {
		nsI, nameI := key(items[i])
		nsJ, nameJ := key(items[j])
		if nsI != nsJ {
			return nsI < nsJ
		}
		return nameI < nameJ
	})
}

// --- ownership graph (xray) --------------------------------------------------

// TreeNode is one object in the ownership graph: a controller, the replica set
// it rolled out, or a pod.
type TreeNode struct {
	Kind      string
	Name      string
	Namespace string
	Status    string
	Healthy   bool
	Children  []*TreeNode
}

// ownerRef is the subset of metadata.ownerReferences the graph needs.
type ownerRef struct {
	UID  string `json:"uid"`
	Kind string `json:"kind"`
}

// objectMeta is what every object in the graph contributes.
type graphObject struct {
	Metadata struct {
		UID             string     `json:"uid"`
		Name            string     `json:"name"`
		Namespace       string     `json:"namespace"`
		OwnerReferences []ownerRef `json:"ownerReferences"`
	} `json:"metadata"`
	Kind   string `json:"kind"`
	Status struct {
		Phase           string `json:"phase"`
		ReadyReplicas   int    `json:"readyReplicas"`
		Replicas        int    `json:"replicas"`
		NumberReady     int    `json:"numberReady"`
		DesiredNumber   int    `json:"desiredNumberScheduled"`
		AvailableReps   int    `json:"availableReplicas"`
		ContainerStatus []struct {
			Ready bool `json:"ready"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// OwnerTree builds the deployment → replicaset → pod graph for ns, with
// statefulsets, daemonsets and unowned pods as their own roots.
//
// One query returns every kind at once: fetching them separately would build
// the graph out of objects observed at different moments, and a pod could then
// appear orphaned only because its owner was created between the two calls.
func OwnerTree(exec Executor, ns string) ([]*TreeNode, error) {
	out, code, err := exec.Run(Builder(exec)(
		"get deployments,statefulsets,daemonsets,replicasets,pods "+nsFlag(ns)+" -o json") + " 2>/dev/null")
	if err != nil {
		return nil, err
	}
	if code != 0 || strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("kubectl get workloads failed (exit %d)", code)
	}
	var raw struct {
		Items []graphObject `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse ownership graph: %w", err)
	}

	byUID := map[string]*TreeNode{}
	owner := map[string]string{} // child uid → owner uid
	order := make([]string, 0, len(raw.Items))
	for _, it := range raw.Items {
		n := &TreeNode{
			Kind:      it.Kind,
			Name:      it.Metadata.Name,
			Namespace: it.Metadata.Namespace,
		}
		switch it.Kind {
		case "Pod":
			n.Status = it.Status.Phase
			ready := 0
			for _, cs := range it.Status.ContainerStatus {
				if cs.Ready {
					ready++
				}
			}
			n.Healthy = it.Status.Phase == "Succeeded" ||
				(it.Status.Phase == "Running" && ready == len(it.Status.ContainerStatus) && ready > 0)
		case "DaemonSet":
			n.Status = fmt.Sprintf("%d/%d ready", it.Status.NumberReady, it.Status.DesiredNumber)
			n.Healthy = it.Status.NumberReady == it.Status.DesiredNumber
		default:
			n.Status = fmt.Sprintf("%d/%d ready", it.Status.ReadyReplicas, it.Status.Replicas)
			n.Healthy = it.Status.ReadyReplicas == it.Status.Replicas
		}
		byUID[it.Metadata.UID] = n
		order = append(order, it.Metadata.UID)
		for _, o := range it.Metadata.OwnerReferences {
			owner[it.Metadata.UID] = o.UID
			break
		}
	}

	var roots []*TreeNode
	for _, uid := range order {
		node := byUID[uid]
		parentUID, hasOwner := owner[uid]
		parent, known := byUID[parentUID]
		// An owner outside this namespace (or a kind we did not fetch) leaves
		// the object as a root: dropping it would hide running pods.
		if !hasOwner || !known {
			roots = append(roots, node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}
	// Empty replica sets are the residue of past rollouts; showing them buries
	// the live one under a pile of history.
	roots = pruneEmptyReplicaSets(roots)
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].Namespace != roots[j].Namespace {
			return roots[i].Namespace < roots[j].Namespace
		}
		if roots[i].Kind != roots[j].Kind {
			return roots[i].Kind < roots[j].Kind
		}
		return roots[i].Name < roots[j].Name
	})
	return roots, nil
}

func pruneEmptyReplicaSets(nodes []*TreeNode) []*TreeNode {
	out := make([]*TreeNode, 0, len(nodes))
	for _, n := range nodes {
		n.Children = pruneEmptyReplicaSets(n.Children)
		if n.Kind == "ReplicaSet" && len(n.Children) == 0 {
			continue
		}
		out = append(out, n)
	}
	return out
}
