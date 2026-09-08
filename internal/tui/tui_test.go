package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/solutionforest/k3helper/internal/check"
	"github.com/solutionforest/k3helper/internal/config"
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
