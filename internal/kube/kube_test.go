package kube

import (
	"strings"
	"testing"
	"time"
)

// fakeExec returns canned output for the first command whose substring matches.
type fakeExec struct {
	out  string
	code int
	last string
}

func (f *fakeExec) Run(cmd string) (string, int, error) {
	f.last = cmd
	return f.out, f.code, nil
}

const podsJSON = `{"items":[
  {"metadata":{"namespace":"prod","name":"web-1","creationTimestamp":"2026-09-08T10:00:00Z"},
   "spec":{"nodeName":"agent1"},
   "status":{"phase":"Running","containerStatuses":[
     {"ready":true,"restartCount":0,"state":{}},
     {"ready":true,"restartCount":2,"state":{}}]}},
  {"metadata":{"namespace":"prod","name":"broken-1","creationTimestamp":"2026-09-08T10:00:00Z"},
   "spec":{"nodeName":"agent2"},
   "status":{"phase":"Pending","containerStatuses":[
     {"ready":false,"restartCount":7,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},
  {"metadata":{"namespace":"kube-system","name":"dns-1","creationTimestamp":"2026-09-08T10:00:00Z"},
   "spec":{"nodeName":"server"},
   "status":{"phase":"Running","containerStatuses":[{"ready":true,"restartCount":0,"state":{}}]}}
]}`

func TestListPods(t *testing.T) {
	f := &fakeExec{out: podsJSON}
	pods, err := ListPods(f, "")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(pods) != 3 {
		t.Fatalf("got %d pods, want 3", len(pods))
	}
	// sorted by namespace then name: kube-system first
	if pods[0].Namespace != "kube-system" {
		t.Errorf("pods[0].Namespace = %q, want kube-system (sorted)", pods[0].Namespace)
	}

	var broken Pod
	for _, p := range pods {
		if p.Name == "broken-1" {
			broken = p
		}
	}
	// The waiting reason must win over the generic "Pending" phase: that is
	// the whole point of the column for an operator.
	if broken.Status != "CrashLoopBackOff" {
		t.Errorf("Status = %q, want CrashLoopBackOff not the phase", broken.Status)
	}
	if broken.Ready != "0/1" {
		t.Errorf("Ready = %q, want 0/1", broken.Ready)
	}
	if broken.Restarts != 7 {
		t.Errorf("Restarts = %d, want 7", broken.Restarts)
	}
	if broken.Healthy() {
		t.Error("a CrashLoopBackOff pod must not report Healthy")
	}

	var web Pod
	for _, p := range pods {
		if p.Name == "web-1" {
			web = p
		}
	}
	if web.Ready != "2/2" {
		t.Errorf("Ready = %q, want 2/2", web.Ready)
	}
	// restarts are summed across containers
	if web.Restarts != 2 {
		t.Errorf("Restarts = %d, want 2 (summed)", web.Restarts)
	}
	if !web.Healthy() {
		t.Error("a fully ready Running pod should report Healthy")
	}
}

// A Running pod with zero ready containers is not healthy, however cheerful
// its phase looks.
func TestPodHealthyRequiresReadyContainers(t *testing.T) {
	if (Pod{Status: "Running", Ready: "0/1"}).Healthy() {
		t.Error("Running with 0/1 ready must not be Healthy: the pod is failing readiness")
	}
	// A finished Job pod has no running containers by design.
	if !(Pod{Status: "Succeeded", Ready: "0/1"}).Healthy() {
		t.Error("a Succeeded pod should be Healthy despite 0 ready containers")
	}
	if !(Pod{Status: "Completed", Ready: "0/2"}).Healthy() {
		t.Error("a Completed pod should be Healthy despite 0 ready containers")
	}
	if (Pod{Status: "CrashLoopBackOff", Ready: "0/1"}).Healthy() {
		t.Error("CrashLoopBackOff must never be Healthy")
	}
}

func TestListPodsNamespaceScoping(t *testing.T) {
	f := &fakeExec{out: podsJSON}
	if _, err := ListPods(f, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.last, "-A") {
		t.Errorf("empty namespace should query all namespaces, got %q", f.last)
	}
	if _, err := ListPods(f, "prod"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.last, "-n 'prod'") {
		t.Errorf("expected -n 'prod' in %q", f.last)
	}
}

func TestListPodsErrors(t *testing.T) {
	if _, err := ListPods(&fakeExec{out: "", code: 1}, ""); err == nil {
		t.Error("expected an error when kubectl fails")
	}
	if _, err := ListPods(&fakeExec{out: "not json", code: 0}, ""); err == nil {
		t.Error("expected a parse error on garbage output")
	}
}

const nodesJSON = `{"items":[
  {"metadata":{"name":"server","creationTimestamp":"2026-09-01T10:00:00Z",
    "labels":{"node-role.kubernetes.io/control-plane":"true","node-role.kubernetes.io/master":"true"}},
   "status":{"conditions":[{"type":"Ready","status":"True"}],"nodeInfo":{"kubeletVersion":"v1.33.1+k3s1"}}},
  {"metadata":{"name":"agent1","creationTimestamp":"2026-09-01T10:00:00Z","labels":{}},
   "status":{"conditions":[{"type":"Ready","status":"False"}],"nodeInfo":{"kubeletVersion":"v1.33.1+k3s1"}}}
]}`

func TestListNodes(t *testing.T) {
	nodes, err := ListNodes(&fakeExec{out: nodesJSON})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(nodes))
	}
	// sorted by name: agent1 before server
	if nodes[0].Name != "agent1" {
		t.Errorf("nodes[0] = %q, want agent1 (sorted)", nodes[0].Name)
	}
	if nodes[0].Status != "NotReady" {
		t.Errorf("Ready=False should render NotReady, got %q", nodes[0].Status)
	}
	if nodes[0].Roles != "<none>" {
		t.Errorf("Roles = %q, want <none> for an unlabelled node", nodes[0].Roles)
	}
	if nodes[1].Status != "Ready" {
		t.Errorf("Status = %q, want Ready", nodes[1].Status)
	}
	if nodes[1].Roles != "control-plane,master" {
		t.Errorf("Roles = %q, want the sorted role labels", nodes[1].Roles)
	}
	if nodes[1].Version != "v1.33.1+k3s1" {
		t.Errorf("Version = %q", nodes[1].Version)
	}
}

func TestListEventsSortsNewestFirst(t *testing.T) {
	now := time.Now().UTC()
	data := `{"items":[
	  {"metadata":{"namespace":"prod"},"involvedObject":{"kind":"Pod","name":"old"},
	   "type":"Warning","reason":"Old","message":"a\nb","count":1,"lastTimestamp":"` +
		now.Add(-time.Hour).Format(time.RFC3339) + `"},
	  {"metadata":{"namespace":"prod"},"involvedObject":{"kind":"Pod","name":"new"},
	   "type":"Warning","reason":"New","message":"x","count":3,"lastTimestamp":"` +
		now.Add(-time.Minute).Format(time.RFC3339) + `"}
	]}`
	events, err := ListEvents(&fakeExec{out: data}, "")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Reason != "New" {
		t.Errorf("events[0].Reason = %q, want the most recent event first", events[0].Reason)
	}
	// newlines would break table rendering
	var old Event
	for _, e := range events {
		if e.Reason == "Old" {
			old = e
		}
	}
	if strings.Contains(old.Message, "\n") {
		t.Errorf("message should be flattened for table display: %q", old.Message)
	}
}

// events.k8s.io records carry eventTime instead of lastTimestamp; a missing
// lastTimestamp must not render every event as decades old.
func TestListEventsFallsBackToEventTime(t *testing.T) {
	now := time.Now().UTC()
	data := `{"items":[{"metadata":{"namespace":"x"},"involvedObject":{"kind":"Pod","name":"p"},
	  "type":"Warning","reason":"R","message":"m","eventTime":"` + now.Add(-2*time.Minute).Format(time.RFC3339) + `"}]}`
	events, err := ListEvents(&fakeExec{out: data}, "")
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Age > 10*time.Minute {
		t.Errorf("Age = %v, want ~2m from eventTime", events[0].Age)
	}
}

func TestLogsBuildsCommand(t *testing.T) {
	f := &fakeExec{out: "hello"}
	out, err := Logs(f, "prod", "web-1", 50, false)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if out != "hello" {
		t.Errorf("out = %q", out)
	}
	for _, want := range []string{"logs 'web-1'", "-n 'prod'", "--tail=50", "--all-containers=true"} {
		if !strings.Contains(f.last, want) {
			t.Errorf("command %q missing %q", f.last, want)
		}
	}
	if strings.Contains(f.last, "--previous") {
		t.Error("--previous should not be set unless requested")
	}
	if _, err := Logs(f, "prod", "web-1", 50, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.last, "--previous") {
		t.Errorf("expected --previous in %q", f.last)
	}
}

// Names reach the shell inside single quotes; an embedded quote must be
// escaped rather than closing the word.
func TestShellQuoteEscapes(t *testing.T) {
	got := shellQuote(`we'ird`)
	if got != `'we'\''ird'` {
		t.Errorf("shellQuote = %s", got)
	}
	f := &fakeExec{out: ""}
	Logs(f, "ns", `p'; touch /tmp/pwned; '`, 10, false)
	if strings.Contains(f.last, `; touch /tmp/pwned;`) && !strings.Contains(f.last, `'\''`) {
		t.Errorf("pod name was not escaped: %q", f.last)
	}
}

func TestLogsPreviousErrorIsExplicit(t *testing.T) {
	_, err := Logs(&fakeExec{out: "error", code: 1}, "ns", "pod", 10, true)
	if err == nil || !strings.Contains(err.Error(), "no previous container instance") {
		t.Errorf("err = %v, want an explicit no-previous-instance message", err)
	}
}

func TestShortAge(t *testing.T) {
	cases := map[time.Duration]string{
		-time.Second:     "0s",
		30 * time.Second: "30s",
		90 * time.Second: "1m",
		2 * time.Hour:    "2h",
		49 * time.Hour:   "2d",
		23*time.Hour + 1: "23h",
	}
	for d, want := range cases {
		if got := ShortAge(d); got != want {
			t.Errorf("ShortAge(%v) = %q, want %q", d, got, want)
		}
	}
}
