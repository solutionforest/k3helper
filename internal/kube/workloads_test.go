package kube

import (
	"strings"
	"testing"
)

const deploymentsJSON = `{"items":[
  {"kind":"Deployment","metadata":{"namespace":"prod","name":"web","creationTimestamp":"2026-09-08T10:00:00Z"},
   "spec":{"replicas":3,"selector":{"matchLabels":{"app":"web","tier":"front"}},
     "template":{"spec":{"containers":[{"image":"nginx:1.27"}]}}},
   "status":{"readyReplicas":3,"updatedReplicas":3,"availableReplicas":3,"replicas":3}},
  {"kind":"Deployment","metadata":{"namespace":"prod","name":"api","creationTimestamp":"2026-09-08T10:00:00Z"},
   "spec":{"selector":{"matchLabels":{"app":"api"}},
     "template":{"spec":{"containers":[{"image":"api:v2"}]}}},
   "status":{"readyReplicas":0,"updatedReplicas":1,"availableReplicas":0,"replicas":1}}
]}`

func TestListWorkloadsDeployments(t *testing.T) {
	f := &fakeExec{out: deploymentsJSON}
	ws, err := ListWorkloads(f, "deployments", "prod")
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d workloads, want 2", len(ws))
	}
	// Sorted by namespace/name: api before web.
	if ws[0].Name != "api" || ws[1].Name != "web" {
		t.Fatalf("unexpected order: %s, %s", ws[0].Name, ws[1].Name)
	}
	if ws[1].Ready != "3/3" || !ws[1].Healthy() {
		t.Errorf("web = %q healthy=%v; want 3/3 healthy", ws[1].Ready, ws[1].Healthy())
	}
	if ws[1].Selector != "app=web,tier=front" {
		t.Errorf("selector = %q; want app=web,tier=front", ws[1].Selector)
	}
	if ws[1].Images != "nginx:1.27" {
		t.Errorf("images = %q", ws[1].Images)
	}
	// A Deployment with no spec.replicas defaults to 1, not 0: reporting
	// "0/0" would make a broken single-replica deployment look healthy.
	if ws[0].Ready != "0/1" {
		t.Errorf("api ready = %q; want 0/1", ws[0].Ready)
	}
	if ws[0].Healthy() {
		t.Error("api has no ready replicas and must not read as healthy")
	}
	if !strings.Contains(f.last, "-n 'prod'") {
		t.Errorf("namespace not passed to kubectl: %s", f.last)
	}
}

const daemonsetsJSON = `{"items":[
  {"kind":"DaemonSet","metadata":{"namespace":"kube-system","name":"svclb","creationTimestamp":"2026-09-08T10:00:00Z"},
   "spec":{"selector":{"matchLabels":{"app":"svclb"}},"template":{"spec":{"containers":[{"image":"klipper:v1"}]}}},
   "status":{"desiredNumberScheduled":3,"numberReady":2,"updatedNumberScheduled":3,"numberAvailable":2}}
]}`

// A DaemonSet spells every status field differently from a Deployment; the
// browser must not report a half-rolled-out DaemonSet as "0/0".
func TestListWorkloadsDaemonSetFields(t *testing.T) {
	f := &fakeExec{out: daemonsetsJSON}
	ws, err := ListWorkloads(f, "daemonsets", "")
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if len(ws) != 1 {
		t.Fatalf("got %d, want 1", len(ws))
	}
	if ws[0].Ready != "2/3" {
		t.Errorf("ready = %q; want 2/3", ws[0].Ready)
	}
	if ws[0].Healthy() {
		t.Error("2/3 ready must not read as healthy")
	}
	if ws[0].UpToDate != 3 || ws[0].Available != 2 {
		t.Errorf("up-to-date/available = %d/%d; want 3/2", ws[0].UpToDate, ws[0].Available)
	}
}

func TestListWorkloadsRejectsUnknownKind(t *testing.T) {
	if _, err := ListWorkloads(&fakeExec{out: "{}"}, "cronjobs", ""); err == nil {
		t.Fatal("expected an error for an unsupported kind")
	}
}

const servicesJSON = `{"items":[
  {"metadata":{"namespace":"prod","name":"web","creationTimestamp":"2026-09-08T10:00:00Z"},
   "spec":{"type":"NodePort","clusterIP":"10.43.0.10","selector":{"app":"web"},
     "ports":[{"port":80,"nodePort":31000,"protocol":"TCP"}]},"status":{}},
  {"metadata":{"namespace":"prod","name":"lb","creationTimestamp":"2026-09-08T10:00:00Z"},
   "spec":{"type":"LoadBalancer","clusterIP":"10.43.0.11","ports":[{"port":443,"protocol":"TCP"}]},
   "status":{"loadBalancer":{}}}
]}`

func TestListServices(t *testing.T) {
	svcs, err := ListServices(&fakeExec{out: servicesJSON}, "prod")
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(svcs) != 2 {
		t.Fatalf("got %d services, want 2", len(svcs))
	}
	lb, web := svcs[0], svcs[1]
	if web.Ports != "80:31000/TCP" {
		t.Errorf("nodePort not rendered: %q", web.Ports)
	}
	if web.Selector != "app=web" {
		t.Errorf("selector = %q", web.Selector)
	}
	// A LoadBalancer with no address is waiting on something, and saying
	// "<none>" would hide that.
	if lb.ExternalIP != "<pending>" {
		t.Errorf("unfulfilled LoadBalancer external IP = %q; want <pending>", lb.ExternalIP)
	}
}

const ingressJSON = `{"items":[
  {"metadata":{"namespace":"prod","name":"web","creationTimestamp":"2026-09-08T10:00:00Z"},
   "spec":{"ingressClassName":"traefik","tls":[{"hosts":["a.example.com"]}],
     "rules":[{"host":"a.example.com"},{"host":"b.example.com"}]},
   "status":{"loadBalancer":{"ingress":[{"ip":"192.168.1.10"}]}}},
  {"metadata":{"namespace":"prod","name":"bare","creationTimestamp":"2026-09-08T10:00:00Z"},
   "spec":{"rules":[{}]},"status":{"loadBalancer":{}}}
]}`

func TestListIngresses(t *testing.T) {
	ings, err := ListIngresses(&fakeExec{out: ingressJSON}, "")
	if err != nil {
		t.Fatalf("ListIngresses: %v", err)
	}
	if len(ings) != 2 {
		t.Fatalf("got %d, want 2", len(ings))
	}
	bare, web := ings[0], ings[1]
	if web.Hosts != "a.example.com,b.example.com" {
		t.Errorf("hosts = %q", web.Hosts)
	}
	if web.Ports != "80,443" {
		t.Errorf("a TLS ingress serves 443 too; ports = %q", web.Ports)
	}
	if web.Address != "192.168.1.10" {
		t.Errorf("address = %q", web.Address)
	}
	if bare.Class != "<none>" || bare.Hosts != "*" {
		t.Errorf("bare ingress = class %q hosts %q", bare.Class, bare.Hosts)
	}
}

// A mixed list: two deployments, their replica sets, their pods, one old empty
// replica set, and a pod with no owner at all.
const graphJSON = `{"items":[
  {"kind":"Deployment","metadata":{"uid":"d1","name":"web","namespace":"prod"},
   "status":{"readyReplicas":1,"replicas":1}},
  {"kind":"ReplicaSet","metadata":{"uid":"rs1","name":"web-abc","namespace":"prod",
    "ownerReferences":[{"uid":"d1","kind":"Deployment"}]},"status":{"readyReplicas":1,"replicas":1}},
  {"kind":"ReplicaSet","metadata":{"uid":"rs0","name":"web-old","namespace":"prod",
    "ownerReferences":[{"uid":"d1","kind":"Deployment"}]},"status":{"readyReplicas":0,"replicas":0}},
  {"kind":"Pod","metadata":{"uid":"p1","name":"web-abc-1","namespace":"prod",
    "ownerReferences":[{"uid":"rs1","kind":"ReplicaSet"}]},
   "status":{"phase":"Running","containerStatuses":[{"ready":true}]}},
  {"kind":"Pod","metadata":{"uid":"p2","name":"loner","namespace":"prod"},
   "status":{"phase":"Pending","containerStatuses":[{"ready":false}]}}
]}`

func TestOwnerTree(t *testing.T) {
	roots, err := OwnerTree(&fakeExec{out: graphJSON}, "prod")
	if err != nil {
		t.Fatalf("OwnerTree: %v", err)
	}
	// The deployment and the unowned pod are roots; the replica sets are not.
	if len(roots) != 2 {
		t.Fatalf("got %d roots, want 2: %+v", len(roots), roots)
	}
	var deployment, loner *TreeNode
	for _, r := range roots {
		switch r.Kind {
		case "Deployment":
			deployment = r
		case "Pod":
			loner = r
		}
	}
	if deployment == nil || loner == nil {
		t.Fatalf("expected a Deployment root and an unowned Pod root, got %+v", roots)
	}
	// The empty replica set left over from an earlier rollout is pruned, so
	// the live one is not buried under rollout history.
	if len(deployment.Children) != 1 {
		t.Fatalf("deployment has %d children, want 1 (the live replica set)", len(deployment.Children))
	}
	rs := deployment.Children[0]
	if rs.Name != "web-abc" {
		t.Errorf("kept replica set %q, want web-abc", rs.Name)
	}
	if len(rs.Children) != 1 || rs.Children[0].Name != "web-abc-1" {
		t.Errorf("replica set children = %+v", rs.Children)
	}
	if !rs.Children[0].Healthy {
		t.Error("a Running pod with every container ready is healthy")
	}
	// A pod whose owner was not fetched must still appear: dropping it would
	// hide running workloads from the graph.
	if loner.Name != "loner" || loner.Healthy {
		t.Errorf("unowned pod = %+v", loner)
	}
}

func TestOwnerTreeReportsQueryFailure(t *testing.T) {
	if _, err := OwnerTree(&fakeExec{out: "", code: 1}, ""); err == nil {
		t.Fatal("a failed kubectl must be an error, not an empty graph")
	}
}

func TestRenderSelectorIsStable(t *testing.T) {
	got := renderSelector(map[string]string{"tier": "front", "app": "web"})
	if got != "app=web,tier=front" {
		t.Errorf("renderSelector = %q; want keys sorted", got)
	}
	if renderSelector(nil) != "" {
		t.Error("no labels must render as an empty selector, not as `=`")
	}
}
