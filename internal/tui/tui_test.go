package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/solutionforest/k3helper/internal/check"
	"github.com/solutionforest/k3helper/internal/config"
	"strings"
)

func testTargets() *config.Targets {
	return &config.Targets{
		Cluster: "test-cluster",
		Nodes: []config.Node{
			{Name: "node1", Role: "server", Host: "127.0.0.1", Port: 22, User: "x"},
			{Name: "node2", Role: "agent", Host: "127.0.0.1", Port: 22, User: "x"},
		},
	}
}

func TestTUICountersRender(t *testing.T) {
	m := New(testTargets())
	// simulate results without real SSH
	m.results = map[string][]check.Result{
		"node1": {
			{ID: "host.disk", Name: "Disk", Status: check.OK, Summary: "40% used"},
			{ID: "k3s.service", Name: "k3s", Status: check.OK, Summary: "active"},
		},
		"node2": {
			{ID: "host.disk", Name: "Disk", Status: check.Fail, Summary: "97% used", Remediation: "free space"},
		},
	}
	m.loading = false
	out := m.View()
	for _, want := range []string{"test-cluster", "2 ok", "1 fail", "node1", "node2", "free space"} {
		if !contains(out, want) {
			t.Errorf("view missing %q\n---\n%s", want, out)
		}
	}
}

func TestTUILoadingView(t *testing.T) {
	m := New(testTargets())
	out := m.View()
	if !contains(out, "running checks") {
		t.Errorf("loading view missing indicator: %s", out)
	}
}

func TestTUIQuitOnQ(t *testing.T) {
	tm := teatest.NewTestModel(t, New(testTargets()), teatest.WithInitialTermSize(100, 40))
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	tm.WaitFinished(t, teatest.WithFinalTimeout(time.Second*5))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && stringsIndexOf(s, sub) >= 0
}

func stringsIndexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// A skipped check must not drag the health score down.
//
// Every host check is skipped on a kubeconfig cluster, and counting those as
// "not OK" put "score 0%" at the top of a screen showing a perfectly healthy
// cluster — spotted in a screenshot of a live DigitalOcean cluster.
func TestHealthScoreIgnoresSkippedChecks(t *testing.T) {
	m := Model{results: map[string][]check.Result{
		"kubeconfig": check.SkippedHostResults("server"),
	}}
	if score, have := m.healthScore(); have {
		t.Errorf("a cluster with nothing but skipped checks reported a score of %d%%, want none", score)
	}

	m = Model{results: map[string][]check.Result{
		"node1": {
			{Status: check.OK},
			{Status: check.OK},
			{Status: check.Skip},
			{Status: check.Fail},
		},
	}}
	score, have := m.healthScore()
	if !have {
		t.Fatal("no score for a node with real results")
	}
	// 2 OK out of 3 that were actually run; the skip is not a third failure.
	if score != 66 {
		t.Errorf("score = %d%%, want 66%% (2 of 3 run, skip excluded)", score)
	}
}

// A kubeconfig cluster has no nodes in its targets file, and the dashboard
// used to iterate only over those — so the skipped host checks, and the reason
// they were skipped, never reached the screen. An empty dashboard reads as
// "all clear", which is the one thing those results exist to prevent.
func TestDashboardShowsResultsForClustersWithNoNodes(t *testing.T) {
	m := Model{
		targets: &config.Targets{Cluster: "prod", Kubeconfig: "/k/c.yaml"},
		results: map[string][]check.Result{
			"prod-admin (kubeconfig)": check.SkippedHostResults("server"),
		},
		hist: newHistory(),
	}
	body := m.dashboardBody()
	if !strings.Contains(body, "prod-admin") {
		t.Errorf("the result group is not on the dashboard:\n%s", body)
	}
	if !strings.Contains(body, "kubeconfig cluster") {
		t.Errorf("the reason the checks were skipped is not shown:\n%s", body)
	}
	if !strings.Contains(body, "skipped") {
		t.Errorf("the counter does not mention skipped checks:\n%s", body)
	}
}

// Nodes keep the targets file's order, which is the order an operator wrote
// them in and expects to read them in.
func TestDashboardKeepsNodeOrder(t *testing.T) {
	m := Model{
		targets: &config.Targets{Nodes: []config.Node{
			{Name: "server", Role: "server"},
			{Name: "agent1", Role: "agent"},
		}},
		results: map[string][]check.Result{
			"agent1": {{Name: "Disk", Status: check.OK, Summary: "fine"}},
			"server": {{Name: "Disk", Status: check.OK, Summary: "fine"}},
		},
		hist: newHistory(),
	}
	body := m.dashboardBody()
	if strings.Index(body, "server") > strings.Index(body, "agent1") {
		t.Errorf("agent1 was drawn before server:\n%s", body)
	}
}
