package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/solutionforest/k3helper/internal/kube"
)

// view identifies what the browser is currently showing.
type view string

const (
	viewDashboard view = "dashboard"
	viewPods      view = "pods"
	viewNodes     view = "nodes"
	viewEvents    view = "events"
	viewLogs      view = "logs"
	viewDescribe  view = "describe"
)

// resolveView maps a command-bar word to a view, accepting the k9s-style
// short aliases operators already have in their fingers.
func resolveView(s string) (view, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "po", "pod", "pods":
		return viewPods, true
	case "no", "node", "nodes":
		return viewNodes, true
	case "ev", "event", "events":
		return viewEvents, true
	case "dash", "dashboard", "home":
		return viewDashboard, true
	}
	return "", false
}

// resourcesMsg carries a completed resource fetch.
type resourcesMsg struct {
	view   view
	pods   []kube.Pod
	nodes  []kube.Node
	events []kube.Event
	err    error
}

// textMsg carries logs or describe output.
type textMsg struct {
	view  view
	title string
	body  string
	err   error
}

// fetchResources loads the data backing a table view.
func fetchResources(exec kube.Executor, v view, namespace string) tea.Cmd {
	return func() tea.Msg {
		if exec == nil {
			return resourcesMsg{view: v, err: fmt.Errorf("no server connection")}
		}
		switch v {
		case viewPods:
			pods, err := kube.ListPods(exec, namespace)
			return resourcesMsg{view: v, pods: pods, err: err}
		case viewNodes:
			nodes, err := kube.ListNodes(exec)
			return resourcesMsg{view: v, nodes: nodes, err: err}
		case viewEvents:
			events, err := kube.ListEvents(exec, namespace)
			return resourcesMsg{view: v, events: events, err: err}
		}
		return resourcesMsg{view: v}
	}
}

// fetchLogs loads a pod's logs, falling back to the previous container
// instance when the current one has produced nothing yet.
func fetchLogs(exec kube.Executor, ns, pod string) tea.Cmd {
	return func() tea.Msg {
		title := fmt.Sprintf("logs %s/%s", ns, pod)
		if exec == nil {
			return textMsg{view: viewLogs, title: title, err: fmt.Errorf("no server connection")}
		}
		body, err := kube.Logs(exec, ns, pod, 300, false)
		if err == nil && strings.TrimSpace(body) == "" {
			// A crashlooping pod's current instance is often empty; the
			// interesting output is in the instance that died.
			if prev, perr := kube.Logs(exec, ns, pod, 300, true); perr == nil && strings.TrimSpace(prev) != "" {
				return textMsg{view: viewLogs, title: title + " (previous instance)", body: prev}
			}
			body = "(no log output)"
		}
		return textMsg{view: viewLogs, title: title, body: body, err: err}
	}
}

// fetchDescribe loads `kubectl describe` for the selected object.
func fetchDescribe(exec kube.Executor, kind, ns, name string) tea.Cmd {
	return func() tea.Msg {
		title := fmt.Sprintf("describe %s %s", kind, name)
		if exec == nil {
			return textMsg{view: viewDescribe, title: title, err: fmt.Errorf("no server connection")}
		}
		body, err := kube.Describe(exec, kind, ns, name)
		return textMsg{view: viewDescribe, title: title, body: body, err: err}
	}
}

// --- table construction -----------------------------------------------------

func newTable(cols []table.Column, rows []table.Row, height int) table.Model {
	if height < 3 {
		height = 3
	}
	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithFocused(true),
		table.WithHeight(height),
	)
	s := table.DefaultStyles()
	s.Header = s.Header.Bold(true).Foreground(accentColor).BorderBottom(true)
	s.Selected = s.Selected.Bold(true).Foreground(selectedFg).Background(selectedBg)
	t.SetStyles(s)
	return t
}

func podRows(pods []kube.Pod, filter string) []table.Row {
	rows := make([]table.Row, 0, len(pods))
	for _, p := range pods {
		if !matchesFilter(filter, p.Namespace, p.Name, p.Status) {
			continue
		}
		rows = append(rows, table.Row{
			p.Namespace, p.Name, p.Ready, p.Status,
			fmt.Sprint(p.Restarts), p.Node, kube.ShortAge(p.Age),
		})
	}
	return rows
}

func nodeRows(nodes []kube.Node, filter string) []table.Row {
	rows := make([]table.Row, 0, len(nodes))
	for _, n := range nodes {
		if !matchesFilter(filter, n.Name, n.Status, n.Roles) {
			continue
		}
		rows = append(rows, table.Row{n.Name, n.Status, n.Roles, n.Version, kube.ShortAge(n.Age)})
	}
	return rows
}

func eventRows(events []kube.Event, filter string) []table.Row {
	rows := make([]table.Row, 0, len(events))
	for _, e := range events {
		if !matchesFilter(filter, e.Namespace, e.Reason, e.Object, e.Message, e.Type) {
			continue
		}
		rows = append(rows, table.Row{
			kube.ShortAge(e.Age), e.Type, e.Reason, e.Namespace, e.Object, truncate(e.Message, 60),
		})
	}
	return rows
}

// matchesFilter reports whether any field contains the filter, case
// insensitively. An empty filter matches everything.
func matchesFilter(filter string, fields ...string) bool {
	if filter == "" {
		return true
	}
	f := strings.ToLower(filter)
	for _, v := range fields {
		if strings.Contains(strings.ToLower(v), f) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// columnsFor returns the table columns for a view, scaled to the terminal
// width so long names are not clipped on a wide screen.
func columnsFor(v view, width int) []table.Column {
	if width < 80 {
		width = 80
	}
	switch v {
	case viewPods:
		name := width - 16 - 6 - 18 - 5 - 14 - 6 - 8
		return []table.Column{
			{Title: "NAMESPACE", Width: 16},
			{Title: "NAME", Width: clamp(name, 20, 60)},
			{Title: "READY", Width: 6},
			{Title: "STATUS", Width: 18},
			{Title: "RST", Width: 5},
			{Title: "NODE", Width: 14},
			{Title: "AGE", Width: 6},
		}
	case viewNodes:
		return []table.Column{
			{Title: "NAME", Width: clamp(width-46, 16, 40)},
			{Title: "STATUS", Width: 10},
			{Title: "ROLES", Width: 16},
			{Title: "VERSION", Width: 16},
			{Title: "AGE", Width: 6},
		}
	case viewEvents:
		msg := width - 6 - 9 - 20 - 14 - 26 - 8
		return []table.Column{
			{Title: "AGE", Width: 6},
			{Title: "TYPE", Width: 9},
			{Title: "REASON", Width: 20},
			{Title: "NAMESPACE", Width: 14},
			{Title: "OBJECT", Width: 26},
			{Title: "MESSAGE", Width: clamp(msg, 20, 80)},
		}
	}
	return nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// newViewport builds the scrollable pane used for logs and describe output.
func newViewport(width, height int, body string) viewport.Model {
	if width < 20 {
		width = 20
	}
	if height < 3 {
		height = 3
	}
	vp := viewport.New(width, height)
	vp.SetContent(body)
	return vp
}
