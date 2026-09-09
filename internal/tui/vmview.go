package tui

import (
	"fmt"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/solutionforest/k3helper/internal/vm"
)

// --- :vm — the targets file, live -------------------------------------------

// vmNode is one row of the targets view: what the file says, plus what the
// machine actually answers.
type vmNode struct {
	Name    string
	Role    string
	Address string
	SSH     string // "ok" or the connection error
	Version string // installed k3s/kubelet version, or "not installed"
	Ready   bool
}

// vmStatusMsg carries a completed probe of every target.
type vmStatusMsg struct {
	nodes []vmNode
}

// probeTargets dials every node in the targets file in parallel and asks what
// is installed on it.
//
// This is the view that has to work when the cluster does not: it never asks
// the API server anything, so a machine whose k3s is down still reports its
// SSH state and its installed version.
func probeTargets(targets *config.Targets) tea.Cmd {
	return func() tea.Msg {
		out := make([]vmNode, len(targets.Nodes))
		var wg sync.WaitGroup
		for i, n := range targets.Nodes {
			wg.Add(1)
			go func(i int, n config.Node) {
				defer wg.Done()
				row := vmNode{
					Name: n.Name, Role: n.Role, Address: address(n),
				}
				c, err := ssh.Dial(n.SSH())
				if err != nil {
					row.SSH = "unreachable"
					row.Version = err.Error()
					out[i] = row
					return
				}
				defer c.Close()
				row.SSH, row.Ready = "ok", true
				row.Version = installedVersion(c)
				out[i] = row
			}(i, n)
		}
		wg.Wait()
		return vmStatusMsg{nodes: out}
	}
}

func address(n config.Node) string {
	if n.Local {
		return "local"
	}
	if n.Port > 0 && n.Port != 22 {
		return fmt.Sprintf("%s:%d", n.Host, n.Port)
	}
	return n.Host
}

// installedVersion reports the k3s or kubelet version on a host, or that
// neither is installed — the state `vm setup` exists to change.
func installedVersion(c *ssh.Client) string {
	if out, code, err := c.Run(`k3s --version 2>/dev/null | head -1`); err == nil && code == 0 {
		if v := firstVersion(out); v != "" {
			return v
		}
	}
	if out, code, err := c.Run(`kubelet --version 2>/dev/null`); err == nil && code == 0 {
		if v := firstVersion(out); v != "" {
			return v
		}
	}
	return "not installed"
}

// firstVersion pulls the vX.Y.Z token out of a version banner.
func firstVersion(s string) string {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "v") && strings.Count(f, ".") >= 2 {
			return f
		}
	}
	return ""
}

func vmRows(nodes []vmNode, o rowOpts) []table.Row {
	rows := make([]table.Row, 0, len(nodes))
	for _, n := range nodes {
		if !o.match(n.Name, n.Role, n.Address, n.SSH, n.Version) {
			continue
		}
		if o.faultsOnly && n.Ready && n.Version != "not installed" {
			continue
		}
		rows = append(rows, table.Row{n.Name, n.Role, n.Address, n.SSH, truncate(n.Version, 18)})
	}
	return rows
}

// --- bootstrap with live output ---------------------------------------------

// vmLogMsg is one line of bootstrap output.
type vmLogMsg struct {
	line string
	done bool
	err  error
}

// bootstrapStream is the plumbing between vm.Setup, which writes progress to
// an io.Writer from its own goroutine, and the Bubble Tea update loop, which
// can only receive messages.
type bootstrapStream struct {
	lines chan string
	done  chan error
}

func (s *bootstrapStream) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		select {
		case s.lines <- line:
		default:
			// Drop rather than block. The update loop reads continuously, so
			// a full buffer means nothing is reading any more — and blocking
			// here would stop the install itself, which matters far more than
			// a progress line.
		}
	}
	return len(p), nil
}

// startBootstrap installs k3s across every target, streaming progress.
//
// Connections are opened here and closed when the install finishes, rather
// than reusing the dashboard's: bootstrapping is the one operation that runs
// as root on every node, and it should not inherit a session opened minutes
// earlier for read-only queries.
func startBootstrap(targets *config.Targets, opts vm.Options) (*bootstrapStream, tea.Cmd) {
	stream := &bootstrapStream{lines: make(chan string, 64), done: make(chan error, 1)}
	opts.Progress = stream

	go func() {
		var servers, agents []vm.Target
		var clients []*ssh.Client
		defer func() {
			for _, c := range clients {
				c.Close()
			}
			close(stream.lines)
		}()
		for _, n := range targets.Nodes {
			c, err := ssh.Dial(n.SSH())
			if err != nil {
				stream.done <- fmt.Errorf("connect to %s: %w", n.Name, err)
				return
			}
			clients = append(clients, c)
			t := vm.Target{Node: n.SSH(), Client: c}
			if n.Role == "server" {
				servers = append(servers, t)
				continue
			}
			agents = append(agents, t)
		}
		stream.done <- vm.Setup(servers, agents, opts)
	}()

	return stream, waitForBootstrapLine(stream)
}

// waitForBootstrapLine yields the next progress line, or the final result.
func waitForBootstrapLine(s *bootstrapStream) tea.Cmd {
	return func() tea.Msg {
		select {
		case line, ok := <-s.lines:
			if !ok {
				// The writer closed: the install goroutine has finished, so the
				// result is either already queued or about to be.
				return vmLogMsg{done: true, err: <-s.done}
			}
			return vmLogMsg{line: line}
		case err := <-s.done:
			return vmLogMsg{done: true, err: err}
		}
	}
}

// bootstrapFraction estimates progress from the lines seen so far: every node
// contributes one "installing"/"joining" line, so counting them against the
// node total is an honest, if coarse, measure.
func bootstrapFraction(lines []string, nodes int) float64 {
	if nodes == 0 {
		return 1
	}
	done := 0
	for _, l := range lines {
		if strings.Contains(l, "installing") || strings.Contains(l, "joining") {
			done++
		}
	}
	if done > nodes {
		done = nodes
	}
	return float64(done) / float64(nodes)
}

// --- :ctx — cluster switcher -------------------------------------------------

func ctxRows(f *config.File, current string, o rowOpts) []table.Row {
	if f == nil {
		return nil
	}
	var rows []table.Row
	for _, name := range f.Names() {
		t, err := f.Select(name)
		if err != nil {
			continue
		}
		if !o.match(name) {
			continue
		}
		marker := " "
		if name == current {
			marker = statusOKStyle.Render("●")
		}
		server := ""
		if s, err := t.Server(); err == nil {
			server = address(*s)
		}
		rows = append(rows, table.Row{marker, name, fmt.Sprint(len(t.Nodes)), server})
	}
	return rows
}
