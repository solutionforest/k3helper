package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/transport"
	"github.com/solutionforest/k3helper/internal/troubleshoot"
)

// doctorMsg carries a completed diagnosis.
type doctorMsg struct {
	diagnoses   []troubleshoot.Diagnosis
	unreachable []troubleshoot.UnreachableNode
	probeErrors map[string]string
	err         error
}

// runDoctor gathers evidence across every node and ranks root causes.
//
// It opens its own connections and closes them again, exactly as `k3helper
// doctor` does: a node going away is one of the faults being looked for, and a
// connection held open from startup would keep reporting the state it saw when
// it was made.
func runDoctor(targets *config.Targets) tea.Cmd {
	return func() tea.Msg {
		server, err := transport.Server(targets)
		if err != nil {
			return doctorMsg{err: err}
		}
		defer server.Close()

		hosts, failures, closeHosts := transport.Hosts(targets, server, transport.ServerName(targets))
		defer closeHosts()
		var unreachable []troubleshoot.UnreachableNode
		for _, f := range failures {
			unreachable = append(unreachable, troubleshoot.UnreachableNode{Name: f.Name, Reason: f.Reason})
		}
		var serverNames []string
		for _, n := range targets.Servers() {
			serverNames = append(serverNames, n.Name)
		}
		evidence := troubleshoot.Gatherer{
			Server: server, Hosts: hosts, Unreachable: unreachable, ServerNodes: serverNames,
			NoHostLayer: targets.Mode() == config.ModeKubeconfig,
		}.Collect()
		return doctorMsg{
			diagnoses:   troubleshoot.Diagnose(evidence),
			unreachable: unreachable,
			probeErrors: evidence.ProbeErrors,
		}
	}
}

// doctorRows renders findings ranked by confidence.
func doctorRows(ds []troubleshoot.Diagnosis, o rowOpts) []table.Row {
	rows := make([]table.Row, 0, len(ds))
	for _, d := range ds {
		if !o.match(d.SignatureID, d.Title, d.Remediation) {
			continue
		}
		rows = append(rows, table.Row{
			fmt.Sprintf("%d%%", d.Confidence),
			d.SignatureID,
			d.Title,
		})
	}
	return rows
}

// doctorDetail is the pane shown when a finding is opened: the evidence behind
// it and the remediation, which is the whole reason the finding exists.
func doctorDetail(d troubleshoot.Diagnosis) string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render(d.Title) + "\n\n")
	b.WriteString(helpStyle.Render("signature: ") + d.SignatureID + "\n")
	b.WriteString(helpStyle.Render("confidence: ") + fmt.Sprintf("%d%%", d.Confidence) + "\n\n")
	if d.Evidence != "" {
		b.WriteString(sectionStyle.Render("Evidence:") + "\n" + wrap(d.Evidence, 90) + "\n\n")
	}
	b.WriteString(sectionStyle.Render("Remediation:") + "\n" + statusWarnStyle.Render(wrap(d.Remediation, 90)))
	return b.String()
}

// doctorSummary is the one-line health verdict for the status bar.
func doctorSummary(ds []troubleshoot.Diagnosis) string {
	if len(ds) == 0 {
		return statusOKStyle.Render("healthy")
	}
	worst := 0
	for _, d := range ds {
		if d.Confidence > worst {
			worst = d.Confidence
		}
	}
	style := statusWarnStyle
	if worst >= 70 {
		style = statusFailStyle
	}
	return style.Render(fmt.Sprintf("%d finding(s)", len(ds)))
}

// probeErrorLines names what could not be gathered, sorted for a stable pane.
func probeErrorLines(errs map[string]string) []string {
	if len(errs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(errs))
	for k := range errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("could not gather %s: %s", k, errs[k]))
	}
	return out
}

// wrap breaks text at width, so remediation text does not run off the pane.
func wrap(s string, width int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := 0
	for i, w := range words {
		if i > 0 && line+1+len(w) > width {
			b.WriteString("\n")
			line = 0
		} else if i > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(w)
		line += len(w)
	}
	return b.String()
}
