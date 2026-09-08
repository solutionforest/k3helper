// Package tui is the interactive dashboard: k9s-style navigation with
// k3helper's doctor/check/deploy capabilities.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/solutionforest/k3helper/internal/check"
	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/ssh"
)

// tickMsg drives periodic refresh.
type tickMsg time.Time

// checkDoneMsg carries completed check results.
type checkDoneMsg struct {
	nodeResults map[string][]check.Result
}

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("205")).
			Padding(0, 1)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

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
	// results per node name
	results map[string][]check.Result
	// active view: "dashboard", "checks"
	view string
	err  error
}

// New creates the TUI model from a targets file.
func New(targets *config.Targets) Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	return Model{
		targets: targets,
		spinner: s,
		loading: true,
		results: map[string][]check.Result{},
		view:    "dashboard",
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
	return tea.Batch(m.spinner.Tick, doChecks(m.targets), tickEvery())
}

func tickEvery() tea.Cmd {
	return tea.Tick(30*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func doChecks(targets *config.Targets) tea.Cmd {
	return func() tea.Msg {
		results := map[string][]check.Result{}
		for _, node := range targets.Nodes {
			client, err := ssh.Dial(ssh.Node{Host: node.Host, Port: node.Port, User: node.User, Key: node.Key, Local: node.Local})
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

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			m.loading = true
			return m, doChecks(m.targets)
		}
		return m, nil
	case tickMsg:
		return m, tickEvery()
	case checkDoneMsg:
		m.results = msg.nodeResults
		m.loading = false
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

// View implements tea.Model.
func (m Model) View() string {
	if m.loading {
		var b strings.Builder
		b.WriteString(titleStyle.Render("☸ k3helper — "+m.targets.Cluster) + "\n\n")
		b.WriteString("  " + m.spinner.View() + " running checks...\n")
		b.WriteString("\n" + helpStyle.Render(" q: quit"))
		return b.String()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("☸ k3helper — "+m.targets.Cluster) + "\n\n")

	if m.loading {
		b.WriteString("  " + m.spinner.View() + " running checks...\n")
		b.WriteString("\n" + helpStyle.Render(" q: quit"))
		return b.String()
	}

	// summary line
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
	summary := fmt.Sprintf("%s %d ok   %s %d warn   %s %d fail",
		statusOKStyle.Render("●"), ok,
		statusWarnStyle.Render("●"), warn,
		statusFailStyle.Render("●"), fail)
	b.WriteString("  " + summary + "\n\n")

	// node cards
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

	b.WriteString("\n" + helpStyle.Render(" r: refresh  q: quit"))
	return b.String()
}
