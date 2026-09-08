// Package tui is the interactive dashboard: k9s-style navigation with
// k3helper's doctor/check/deploy capabilities.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/solutionforest/k3helper/internal/check"
	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/kube"
	"github.com/solutionforest/k3helper/internal/ssh"
)

// refreshInterval is how often the active view reloads itself.
const refreshInterval = 5 * time.Second

// tickMsg drives periodic refresh.
type tickMsg time.Time

// checkDoneMsg carries completed check results.
type checkDoneMsg struct {
	nodeResults map[string][]check.Result
}

// serverReadyMsg carries the connection used for cluster queries.
type serverReadyMsg struct {
	client *ssh.Client
	err    error
}

var (
	accentColor = lipgloss.Color("205")
	selectedFg  = lipgloss.Color("231")
	selectedBg  = lipgloss.Color("57")

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(accentColor).
			Padding(0, 1)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	errStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)

	statusOKStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	statusWarnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	statusFailStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	paneBorder      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
)

// Model is the root TUI model.
type Model struct {
	targets *config.Targets
	width   int
	height  int
	spinner spinner.Model
	loading bool
	// results per node name (dashboard)
	results map[string][]check.Result
	// view is what is currently on screen
	view view
	err  error

	// server is the connection cluster queries run through. Nil until the
	// dial completes, or when it failed.
	server    *ssh.Client
	serverErr error

	// resource browser state
	pods      []kube.Pod
	nodes     []kube.Node
	events    []kube.Event
	table     table.Model
	namespace string // "" = all namespaces

	// text pane (logs / describe)
	viewport  viewport.Model
	textTitle string

	// command bar and filter
	input      textinput.Model
	inputMode  inputMode
	filter     string
	returnView view
}

// inputMode says what the text input at the bottom is collecting.
type inputMode int

const (
	inputNone inputMode = iota
	inputCommand
	inputFilter
)

// New creates the TUI model from a targets file.
func New(targets *config.Targets) Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 64
	return Model{
		targets: targets,
		spinner: s,
		loading: true,
		results: map[string][]check.Result{},
		view:    viewDashboard,
		input:   in,
		width:   100,
		height:  30,
	}
}

// NewProgram starts the interactive program (separated for testability).
func NewProgram(m Model) (*tea.Program, error) {
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return p, err
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, doChecks(m.targets), dialServer(m.targets), tickEvery())
}

func tickEvery() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// dialServer opens the connection cluster queries run through.
func dialServer(targets *config.Targets) tea.Cmd {
	return func() tea.Msg {
		srv, err := targets.Server()
		if err != nil {
			return serverReadyMsg{err: err}
		}
		c, err := ssh.Dial(srv.SSH())
		return serverReadyMsg{client: c, err: err}
	}
}

func doChecks(targets *config.Targets) tea.Cmd {
	return func() tea.Msg {
		results := map[string][]check.Result{}
		for _, node := range targets.Nodes {
			client, err := ssh.Dial(node.SSH())
			if err != nil {
				results[node.Name] = []check.Result{{
					ID: "ssh.connect", Category: "host", Name: "SSH connectivity",
					Status: check.Fail, Summary: fmt.Sprintf("unreachable: %v", err),
				}}
				continue
			}
			runner := check.NewRunner(
				check.DiskUsageCheck{},
				check.MemoryCheck{},
				check.SwapCheck{},
				check.CgroupCheck{},
				check.K3sServiceCheck{Role: node.Role},
			)
			results[node.Name] = runner.RunAll(check.Context{Exec: execAdapter{client}, Node: node.Name})
			client.Close()
		}
		return checkDoneMsg{nodeResults: results}
	}
}

type execAdapter struct{ c *ssh.Client }

func (a execAdapter) Run(cmd string) (string, int, error) { return a.c.Run(cmd) }

// refresh reloads whatever the active view shows.
func (m Model) refresh() tea.Cmd {
	switch m.view {
	case viewDashboard:
		return doChecks(m.targets)
	case viewPods, viewNodes, viewEvents:
		return fetchResources(m.server, m.view, m.namespace)
	}
	return nil // logs/describe are point-in-time snapshots
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.applySize()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		// Reload the active view, then schedule the next tick. The previous
		// implementation only rescheduled, so nothing ever refreshed.
		return m, tea.Batch(m.refresh(), tickEvery())

	case serverReadyMsg:
		m.server, m.serverErr = msg.client, msg.err
		if msg.err == nil && m.view != viewDashboard {
			return m, m.refresh()
		}
		return m, nil

	case checkDoneMsg:
		m.results = msg.nodeResults
		m.loading = false
		return m, nil

	case resourcesMsg:
		return m.handleResources(msg)

	case textMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			m.view = msg.view
			m.textTitle = msg.title
			m.viewport = newViewport(m.width-2, m.bodyHeight(), msg.body)
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

// handleResources takes a value receiver so Update always returns the same
// concrete type: a pointer here would make tea.Model sometimes *Model and
// sometimes Model, and callers type-asserting on it would panic.
func (m Model) handleResources(msg resourcesMsg) (tea.Model, tea.Cmd) {
	m.loading = false
	m.err = msg.err
	if msg.err != nil {
		return m, nil
	}
	m.view = msg.view
	m.pods, m.nodes, m.events = msg.pods, msg.nodes, msg.events
	m.rebuildTable()
	return m, nil
}

// rebuildTable regenerates rows for the active view, preserving the cursor so
// an auto-refresh does not yank the selection out from under the operator.
func (m *Model) rebuildTable() {
	var rows []table.Row
	switch m.view {
	case viewPods:
		rows = podRows(m.pods, m.filter)
	case viewNodes:
		rows = nodeRows(m.nodes, m.filter)
	case viewEvents:
		rows = eventRows(m.events, m.filter)
	default:
		return
	}
	cursor := m.table.Cursor()
	m.table = newTable(columnsFor(m.view, m.width), rows, m.bodyHeight())
	if cursor > 0 && cursor < len(rows) {
		m.table.SetCursor(cursor)
	}
}

// bodyHeight is the space left for the table or text pane after the header,
// help line and command bar.
func (m Model) bodyHeight() int {
	h := m.height - 7
	if h < 3 {
		return 3
	}
	return h
}

func (m *Model) applySize() {
	if m.view == viewLogs || m.view == viewDescribe {
		m.viewport.Width = m.width - 2
		m.viewport.Height = m.bodyHeight()
		return
	}
	m.rebuildTable()
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The command bar and filter capture every key except submit/cancel.
	if m.inputMode != inputNone {
		switch msg.String() {
		case "esc":
			m.inputMode = inputNone
			m.input.Blur()
			m.input.SetValue("")
			return m, nil
		case "enter":
			value := m.input.Value()
			mode := m.inputMode
			m.inputMode = inputNone
			m.input.Blur()
			m.input.SetValue("")
			if mode == inputCommand {
				return m.runCommand(value)
			}
			m.filter = value
			m.rebuildTable()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		if m.inputMode == inputFilter {
			// live filtering as the operator types
			m.filter = m.input.Value()
			m.rebuildTable()
		}
		return m, cmd
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case ":":
		m.inputMode = inputCommand
		m.input.Placeholder = "pods | nodes | events | dashboard"
		m.input.Focus()
		return m, nil

	case "/":
		if m.view == viewPods || m.view == viewNodes || m.view == viewEvents {
			m.inputMode = inputFilter
			m.input.Placeholder = "filter"
			m.input.SetValue(m.filter)
			m.input.Focus()
		}
		return m, nil

	case "esc":
		// Step back out of a drill-down, or clear an active filter.
		if m.view == viewLogs || m.view == viewDescribe {
			m.view = m.returnView
			m.err = nil
			m.rebuildTable()
			return m, m.refresh()
		}
		if m.filter != "" {
			m.filter = ""
			m.rebuildTable()
		}
		return m, nil

	case "r":
		m.loading = true
		return m, m.refresh()

	case "l", "enter":
		// logs for the selected pod
		if m.view == viewPods {
			if ns, name, ok := m.selectedPod(); ok {
				m.returnView, m.loading = m.view, true
				return m, fetchLogs(m.server, ns, name)
			}
		}
		return m, nil

	case "d":
		// describe the selected object
		switch m.view {
		case viewPods:
			if ns, name, ok := m.selectedPod(); ok {
				m.returnView, m.loading = m.view, true
				return m, fetchDescribe(m.server, "pod", ns, name)
			}
		case viewNodes:
			if row := m.table.SelectedRow(); len(row) > 0 {
				m.returnView, m.loading = m.view, true
				return m, fetchDescribe(m.server, "node", "", row[0])
			}
		}
		return m, nil
	}

	// Remaining keys drive the active widget.
	var cmd tea.Cmd
	if m.view == viewLogs || m.view == viewDescribe {
		m.viewport, cmd = m.viewport.Update(msg)
	} else if m.view != viewDashboard {
		m.table, cmd = m.table.Update(msg)
	}
	return m, cmd
}

// selectedPod returns the namespace and name of the highlighted pod row.
func (m Model) selectedPod() (string, string, bool) {
	row := m.table.SelectedRow()
	if len(row) < 2 {
		return "", "", false
	}
	return row[0], row[1], true
}

// runCommand handles a `:` command-bar entry.
func (m Model) runCommand(cmd string) (tea.Model, tea.Cmd) {
	// `:ns <name>` scopes resource views to one namespace ("all" clears it).
	if rest, ok := strings.CutPrefix(strings.TrimSpace(cmd), "ns "); ok {
		rest = strings.TrimSpace(rest)
		if rest == "all" || rest == "*" {
			rest = ""
		}
		m.namespace = rest
		m.loading = true
		return m, m.refresh()
	}
	v, ok := resolveView(cmd)
	if !ok {
		m.err = fmt.Errorf("unknown command %q — try pods, nodes, events, dashboard, or ns <name>", cmd)
		return m, nil
	}
	m.err = nil
	m.filter = ""
	if v == viewDashboard {
		m.view = v
		m.loading = true
		return m, doChecks(m.targets)
	}
	m.view = v
	m.loading = true
	return m, fetchResources(m.server, v, m.namespace)
}

// View implements tea.Model.
func (m Model) View() string {
	var b strings.Builder
	b.WriteString(m.header())

	switch {
	case m.loading && m.view == viewDashboard && len(m.results) == 0:
		b.WriteString("  " + m.spinner.View() + " running checks...\n")
	case m.view == viewDashboard:
		b.WriteString(m.dashboardBody())
	case m.view == viewLogs || m.view == viewDescribe:
		b.WriteString(m.viewport.View() + "\n")
	default:
		b.WriteString(m.table.View() + "\n")
	}

	if m.err != nil {
		b.WriteString(errStyle.Render("  ! "+m.err.Error()) + "\n")
	}
	b.WriteString(m.footer())
	return b.String()
}

func (m Model) header() string {
	title := "☸ k3helper — " + m.targets.Cluster
	scope := string(m.view)
	if m.namespace != "" {
		scope += " · ns:" + m.namespace
	} else if m.view != viewDashboard && m.view != viewLogs && m.view != viewDescribe {
		scope += " · all namespaces"
	}
	if m.filter != "" {
		scope += " · /" + m.filter
	}
	if m.view == viewLogs || m.view == viewDescribe {
		scope = m.textTitle
	}
	return titleStyle.Render(title) + "  " + helpStyle.Render(scope) + "\n\n"
}

func (m Model) footer() string {
	if m.inputMode == inputCommand {
		return "\n:" + m.input.View()
	}
	if m.inputMode == inputFilter {
		return "\n/" + m.input.View()
	}
	var keys string
	switch m.view {
	case viewDashboard:
		keys = ": command  r: refresh  q: quit"
	case viewPods:
		keys = "↑↓: move  enter/l: logs  d: describe  /: filter  :: command  r: refresh  q: quit"
	case viewNodes:
		keys = "↑↓: move  d: describe  /: filter  :: command  r: refresh  q: quit"
	case viewEvents:
		keys = "↑↓: move  /: filter  :: command  r: refresh  q: quit"
	default:
		keys = "↑↓/pgup/pgdn: scroll  esc: back  q: quit"
	}
	if m.serverErr != nil {
		keys = "cluster unreachable: " + m.serverErr.Error() + "  ·  " + keys
	}
	return "\n" + helpStyle.Render(" "+keys)
}

// dashboardBody renders the per-node check cards.
func (m Model) dashboardBody() string {
	var b strings.Builder
	ok, warn, fail := 0, 0, 0
	for _, rs := range m.results {
		for _, r := range rs {
			switch r.Status {
			case check.OK:
				ok++
			case check.Warn:
				warn++
			case check.Fail:
				fail++
			}
		}
	}
	b.WriteString(fmt.Sprintf("  %s %d ok   %s %d warn   %s %d fail\n\n",
		statusOKStyle.Render("●"), ok,
		statusWarnStyle.Render("●"), warn,
		statusFailStyle.Render("●"), fail))

	for _, node := range m.targets.Nodes {
		rs := m.results[node.Name]
		var lines []string
		for _, r := range rs {
			style := statusOKStyle
			switch r.Status {
			case check.Warn:
				style = statusWarnStyle
			case check.Fail:
				style = statusFailStyle
			}
			lines = append(lines, fmt.Sprintf("%s %-24s %s", style.Render(r.Status.Icon()), r.Name, r.Summary))
			if r.Status == check.Fail && r.Remediation != "" {
				lines = append(lines, "    "+statusFailStyle.Render("↳ "+r.Remediation))
			}
		}
		card := fmt.Sprintf("%s (%s)\n%s", node.Name, node.Role, strings.Join(lines, "\n"))
		b.WriteString(paneBorder.Render(card) + "\n")
	}
	return b.String()
}
