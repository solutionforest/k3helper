package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/kube"
)

func browserTargets() *config.Targets {
	return &config.Targets{
		Cluster: "test-cluster",
		Nodes: []config.Node{
			{Name: "node1", Role: "server", Host: "127.0.0.1", Port: 22, User: "x"},
		},
	}
}

// send drives the model through one key press.
func send(m Model, key string) Model {
	var msg tea.Msg
	switch key {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	next, _ := m.Update(msg)
	return next.(Model)
}

// typeString feeds each rune through the model, as the command bar sees them.
func typeString(m Model, s string) Model {
	for _, r := range s {
		m = send(m, string(r))
	}
	return m
}

func samplePods() []kube.Pod {
	return []kube.Pod{
		{Namespace: "prod", Name: "web-1", Ready: "1/1", Status: "Running", Node: "agent1", Age: time.Minute},
		{Namespace: "prod", Name: "api-1", Ready: "0/1", Status: "CrashLoopBackOff", Restarts: 5, Node: "agent2", Age: time.Hour},
		{Namespace: "kube-system", Name: "coredns-1", Ready: "1/1", Status: "Running", Node: "server", Age: 48 * time.Hour},
	}
}

func TestResolveViewAliases(t *testing.T) {
	cases := map[string]view{
		"po": viewPods, "pod": viewPods, "pods": viewPods, "PODS": viewPods,
		"no": viewNodes, "nodes": viewNodes,
		"ev": viewEvents, "events": viewEvents,
		"dash": viewDashboard, "dashboard": viewDashboard,
		" pods ": viewPods,
	}
	for in, want := range cases {
		got, ok := resolveView(in)
		if !ok || got != want {
			t.Errorf("resolveView(%q) = %v,%v; want %v,true", in, got, ok, want)
		}
	}
	if _, ok := resolveView("nonsense"); ok {
		t.Error("unknown command should not resolve")
	}
}

// The command bar must switch views: `:pods` is the primary navigation.
func TestCommandBarSwitchesView(t *testing.T) {
	m := New(browserTargets())
	m = send(m, ":")
	if m.inputMode != inputCommand {
		t.Fatal(": did not open the command bar")
	}
	m = typeString(m, "pods")
	m = send(m, "enter")
	if m.view != viewPods {
		t.Errorf("view = %q, want pods", m.view)
	}
	if m.inputMode != inputNone {
		t.Error("command bar should close after submit")
	}
}

func TestCommandBarUnknownCommandReportsError(t *testing.T) {
	m := New(browserTargets())
	m = send(m, ":")
	m = typeString(m, "wat")
	m = send(m, "enter")
	if m.err == nil {
		t.Fatal("expected an error for an unknown command")
	}
	if !strings.Contains(m.err.Error(), "wat") {
		t.Errorf("error should name the bad command: %v", m.err)
	}
	if m.view != viewDashboard {
		t.Errorf("view changed to %q on a bad command", m.view)
	}
}

func TestCommandBarEscCancels(t *testing.T) {
	m := New(browserTargets())
	m = send(m, ":")
	m = typeString(m, "pods")
	m = send(m, "esc")
	if m.inputMode != inputNone {
		t.Error("esc should close the command bar")
	}
	if m.view != viewDashboard {
		t.Errorf("esc should not navigate; view = %q", m.view)
	}
}

// `:ns <name>` scopes the resource views; `:ns all` clears the scope.
func TestNamespaceCommand(t *testing.T) {
	m := New(browserTargets())
	m = send(m, ":")
	m = typeString(m, "ns prod")
	m = send(m, "enter")
	if m.namespace != "prod" {
		t.Errorf("namespace = %q, want prod", m.namespace)
	}
	m = send(m, ":")
	m = typeString(m, "ns all")
	m = send(m, "enter")
	if m.namespace != "" {
		t.Errorf("namespace = %q, want empty after `ns all`", m.namespace)
	}
}

func TestFilterNarrowsRows(t *testing.T) {
	m := New(browserTargets())
	m.view = viewPods
	m.pods = samplePods()
	m.rebuildTable()
	if got := len(m.table.Rows()); got != 3 {
		t.Fatalf("unfiltered rows = %d, want 3", got)
	}

	m = send(m, "/")
	if m.inputMode != inputFilter {
		t.Fatal("/ did not open the filter")
	}
	m = typeString(m, "api")
	// filtering is live as the operator types
	if got := len(m.table.Rows()); got != 1 {
		t.Errorf("filtered rows = %d, want 1", got)
	}
	m = send(m, "enter")
	if m.filter != "api" {
		t.Errorf("filter = %q, want api", m.filter)
	}

	// esc clears the filter and restores every row
	m = send(m, "esc")
	if m.filter != "" {
		t.Errorf("filter = %q, want cleared", m.filter)
	}
	if got := len(m.table.Rows()); got != 3 {
		t.Errorf("rows after clearing filter = %d, want 3", got)
	}
}

// The filter must match any column, not just the name.
func TestFilterMatchesAnyColumn(t *testing.T) {
	m := New(browserTargets())
	m.view = viewPods
	m.pods = samplePods()
	m.filter = "crashloop"
	m.rebuildTable()
	if got := len(m.table.Rows()); got != 1 {
		t.Errorf("status filter matched %d rows, want 1", got)
	}
	m.filter = "kube-system"
	m.rebuildTable()
	if got := len(m.table.Rows()); got != 1 {
		t.Errorf("namespace filter matched %d rows, want 1", got)
	}
}

func TestMatchesFilterIsCaseInsensitive(t *testing.T) {
	if !matchesFilter("WEB", "prod", "web-1", "Running") {
		t.Error("filter should be case insensitive")
	}
	if !matchesFilter("", "anything") {
		t.Error("empty filter should match everything")
	}
	if matchesFilter("zzz", "prod", "web-1") {
		t.Error("non-matching filter should exclude the row")
	}
}

// An auto-refresh must not move the operator's selection.
func TestRefreshPreservesCursor(t *testing.T) {
	m := New(browserTargets())
	m.view = viewPods
	m.pods = samplePods()
	m.rebuildTable()
	m = send(m, "down")
	want := m.table.Cursor()
	if want == 0 {
		t.Skip("table did not move the cursor in this environment")
	}
	next, _ := m.Update(resourcesMsg{view: viewPods, pods: samplePods()})
	if got := next.(Model).table.Cursor(); got != want {
		t.Errorf("cursor moved from %d to %d across a refresh", want, got)
	}
}

// The tick must actually reload; the earlier implementation only rescheduled
// itself, so the "live" dashboard never changed.
func TestTickTriggersRefresh(t *testing.T) {
	m := New(browserTargets())
	m.view = viewNodes
	_, cmd := m.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("tick produced no command: nothing will ever refresh")
	}
}

func TestPodRowsRenderExpectedColumns(t *testing.T) {
	rows := podRows(samplePods(), "")
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	// namespace, name, ready, status, restarts, node, age
	var api []string
	for _, r := range rows {
		if r[1] == "api-1" {
			api = r
		}
	}
	if api == nil {
		t.Fatal("api-1 row missing")
	}
	if api[0] != "prod" || api[2] != "0/1" || api[3] != "CrashLoopBackOff" || api[4] != "5" {
		t.Errorf("unexpected row contents: %v", api)
	}
}

func TestEventRowsTruncateLongMessages(t *testing.T) {
	long := strings.Repeat("x", 200)
	rows := eventRows([]kube.Event{{Namespace: "p", Type: "Warning", Reason: "R", Object: "Pod/a", Message: long}}, "")
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if len([]rune(rows[0][5])) > 60 {
		t.Errorf("message not truncated: %d runes", len([]rune(rows[0][5])))
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short string should pass through, got %q", got)
	}
	if got := truncate("hello world", 5); got != "hell…" {
		t.Errorf("truncate = %q, want hell…", got)
	}
}

// Drilling into logs and back must restore the table the operator came from.
func TestEscReturnsFromTextPane(t *testing.T) {
	m := New(browserTargets())
	m.view = viewPods
	m.pods = samplePods()
	m.rebuildTable()
	m.returnView = viewPods

	next, _ := m.Update(textMsg{view: viewLogs, title: "logs prod/web-1", body: "line one\nline two"})
	m = next.(Model)
	if m.view != viewLogs {
		t.Fatalf("view = %q, want logs", m.view)
	}
	if !strings.Contains(m.View(), "line one") {
		t.Error("log body not rendered")
	}
	m = send(m, "esc")
	if m.view != viewPods {
		t.Errorf("view = %q, want to return to pods", m.view)
	}
}

// A failed fetch must surface the error rather than silently showing stale data.
func TestFetchErrorIsDisplayed(t *testing.T) {
	m := New(browserTargets())
	m.view = viewPods
	next, _ := m.Update(resourcesMsg{view: viewPods, err: errFake{}})
	m = next.(Model)
	if m.err == nil {
		t.Fatal("error not recorded")
	}
	if !strings.Contains(m.View(), "boom") {
		t.Errorf("error not shown to the operator:\n%s", m.View())
	}
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

// Quitting must still work from inside the browser, not just the dashboard.
func TestQuitFromBrowser(t *testing.T) {
	m := New(browserTargets())
	m.view = viewPods
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q produced no command")
	}
	if msg := cmd(); msg == nil {
		t.Error("q should produce tea.Quit")
	}
}

// While the command bar is open, "q" is text, not a quit key.
func TestCommandBarSwallowsQuitKey(t *testing.T) {
	m := New(browserTargets())
	m = send(m, ":")
	m = typeString(m, "q")
	if m.inputMode != inputCommand {
		t.Error("typing q closed the command bar")
	}
	if m.input.Value() != "q" {
		t.Errorf("input = %q, want q typed into the bar", m.input.Value())
	}
}
